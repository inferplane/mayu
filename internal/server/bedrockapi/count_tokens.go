package bedrockapi

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"

	"github.com/inferplane/inferplane/internal/filter"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/live"
	"github.com/inferplane/inferplane/internal/principal"
	"github.com/inferplane/inferplane/internal/router"
	"github.com/inferplane/inferplane/providers"
)

type CountTokensHandler struct {
	r          *router.Router
	holder     *live.Holder
	mask       *filter.Masking
	teamPolicy func(team string) (keystore.TeamRecord, bool)
}

func NewCountTokensHandler(r *router.Router, holder *live.Holder) *CountTokensHandler {
	return &CountTokensHandler{r: r, holder: holder}
}

// SetMasking enables PII masking on the count path. nil-safe.
func (h *CountTokensHandler) SetMasking(m *filter.Masking) { h.mask = m }

// SetTeamPolicy installs the fresh-per-request team-record lookup used to
// enforce region restrictions before calling an upstream token counter.
func (h *CountTokensHandler) SetTeamPolicy(fn func(team string) (keystore.TeamRecord, bool)) {
	h.teamPolicy = fn
}

// ServeHTTP NEVER returns a non-200 / non-JSON response. A non-200 here
// crashes Claude Code, so every failure falls back to a local estimate.
func (h *CountTokensHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	raw, _ := io.ReadAll(req.Body)
	n := h.count(req, raw)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]int64{"inputTokens": n})
}

func (h *CountTokensHandler) count(req *http.Request, raw []byte) int64 {
	var wrapper struct {
		Input struct {
			InvokeModel struct {
				Body *string `json:"body"`
			} `json:"invokeModel"`
		} `json:"input"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil || wrapper.Input.InvokeModel.Body == nil {
		return estimateTokens(raw)
	}

	// input.invokeModel.body is a base64 blob per the AWS API reference
	// (InvokeModelTokensRequest: "Base64-encoded binary data object"). If a
	// client ever sends it as raw JSON instead, the decode fails and we fall
	// through to the coarse local estimate — still 200, never an error.
	innerBody, err := base64.StdEncoding.DecodeString(*wrapper.Input.InvokeModel.Body)
	if err != nil {
		return estimateTokens(raw)
	}

	model, ok := resolveModel(h.r, h.holder, req.PathValue("modelId"))
	if !ok {
		return estimateTokens(innerBody)
	}

	// RBAC: a key must not trigger a real upstream CountTokens call for a
	// model outside its allow-list — fall back to the local estimate instead
	// (still 200; the never-non-200 mandate holds, but the upstream never
	// sees content the key isn't entitled to send it).
	if p, ok := principal.From(req.Context()); ok && !h.r.Allows(p, model) {
		return estimateTokens(innerBody)
	}

	if p, ok := principal.From(req.Context()); ok && h.mask.Enabled(p.Team) {
		masked, n, err := maskBody(innerBody, h.mask.Filter)
		if err != nil {
			return estimateTokens(innerBody)
		}
		if n > 0 {
			innerBody = masked
		}
	}

	chain, _, err := h.r.ResolveChain(model)
	if err != nil {
		return estimateTokens(innerBody)
	}
	filtered := make([]router.ChainTarget, 0, len(chain))
	for _, ct := range chain {
		if servesBedrockIngress(ct.Provider.Name()) {
			filtered = append(filtered, ct)
		}
	}
	chain = filtered

	if p, ok := principal.From(req.Context()); ok && h.teamPolicy != nil {
		if rec, ok := h.teamPolicy(p.Team); ok && len(rec.AllowedRegions) > 0 {
			chain = router.FilterRegions(chain, rec.AllowedRegions)
		}
	}
	if len(chain) == 0 {
		return estimateTokens(innerBody)
	}

	ct := chain[0]
	if tc, ok := ct.Provider.(providers.TokenCounter); ok {
		pr := &providers.ProxyRequest{
			Model: model, Upstream: ct.Upstream, RawBody: innerBody, Headers: req.Header,
		}
		if got, err := tc.CountTokens(req.Context(), pr); err == nil && got > 0 {
			return got
		}
	}
	return estimateTokens(innerBody)
}
