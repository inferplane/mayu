package bedrockapi

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"

	"github.com/inferplane/inferplane/internal/audit"
	"github.com/inferplane/inferplane/internal/filter"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/live"
	"github.com/inferplane/inferplane/internal/metrics"
	"github.com/inferplane/inferplane/internal/principal"
	"github.com/inferplane/inferplane/internal/router"
	"github.com/inferplane/inferplane/internal/server/requestpolicy"
	"github.com/inferplane/inferplane/providers"
)

type CountTokensHandler struct {
	ready      func() (bool, string)
	aud        *audit.Writer
	metrics    *metrics.Metrics
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

// SetGovernanceGate installs the SAME gate used by DataMux generation admission.
func (h *CountTokensHandler) SetGovernanceGate(gate func() (bool, string)) { h.ready = gate }

// SetObservability enables safe policy evidence; legacy count traffic stays silent.
func (h *CountTokensHandler) SetObservability(aud *audit.Writer, m *metrics.Metrics) {
	h.aud, h.metrics = aud, m
}

// ServeHTTP NEVER returns a non-200 / non-JSON response. A non-200 here
// crashes Claude Code, so every failure falls back to a local estimate.
func (h *CountTokensHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	raw, readErr := io.ReadAll(req.Body)
	n := estimateTokens(raw)
	ready := true
	if h.ready != nil {
		ready, _ = h.ready()
	}
	// Even a valid JSON prefix from MaxBytesReader is NOT a complete request.
	// Never forward partial content, including declared oversized requests.
	if readErr == nil && ready && !requestpolicy.LocalCount(req.Context()) {
		n = h.count(w, req, raw)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]int64{"inputTokens": n})
}

func (h *CountTokensHandler) count(w http.ResponseWriter, req *http.Request, raw []byte) int64 {
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

	model, _, ok := resolveModel(h.r, h.holder, req.PathValue("modelId"))
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

	chain, st, err := h.r.ResolveChain(model)
	if err != nil {
		return estimateTokens(innerBody)
	}
	// RBAC re-check (C3): same ordering requirement as anthropicapi's
	// identical fix — must run BEFORE the servesBedrockIngress/FilterRegions
	// filters below, or a cross-model fallback promoted to chain[0] by either
	// filter would be mis-classified as "primary" by FilterModelAllowed and
	// receive the caller's body despite the key not being allowed it.
	if p, ok := principal.From(req.Context()); ok {
		chain = router.FilterModelAllowed(chain, func(m string) bool { return h.r.Allows(p, m) })
	}
	filtered := make([]router.ChainTarget, 0, len(chain))
	for _, ct := range chain {
		if servesBedrockIngress(ct.Provider.Name()) {
			filtered = append(filtered, ct)
		}
	}
	chain = filtered

	p, authenticated := principal.From(req.Context())
	if !authenticated {
		return estimateTokens(innerBody)
	}
	var regions []string
	if h.teamPolicy != nil {
		if rec, ok := h.teamPolicy(p.Team); ok {
			regions = rec.AllowedRegions
		}
	}
	if len(regions) > 0 {
		chain = router.FilterRegions(chain, regions)
	}
	result, routeErr := h.r.RouteRequest(req.Context(), router.RequestRoutingInput{
		Principal: p, Protocol: "bedrock", RawBody: innerBody, RequestedModel: model,
		Model: model, Chain: chain, State: st, AllowedRegions: regions, CountOnly: true,
	})
	req = requestpolicy.Observe(w, req, result, h.metrics)
	requestpolicy.CountRecord(h.aud, req, "bedrock", false)
	// Evaluate the final request context at return, after an actual attempt if any.
	defer func() { requestpolicy.CountRecord(h.aud, req, "bedrock", true) }()
	if routeErr != nil {
		return estimateTokens(innerBody)
	}
	chain = result.Chain
	if p, ok := principal.From(req.Context()); ok && h.mask.Enabled(p.Team) {
		masked, n, err := maskBody(innerBody, h.mask.Filter)
		if err != nil {
			return estimateTokens(innerBody)
		}
		if n > 0 {
			innerBody = masked
		}
	}

	ct := chain[0]
	if tc, ok := ct.Provider.(providers.TokenCounter); ok {
		req = req.WithContext(audit.WithRoutingAttempt(req.Context(), ct.Model, ct.ProviderName, ct.DataBoundary))
		pr := &providers.ProxyRequest{
			Model: ct.Model, IngressProtocol: "bedrock", Upstream: ct.Upstream, RawBody: innerBody, Headers: req.Header,
		}
		if got, err := tc.CountTokens(req.Context(), pr); err == nil && got > 0 {
			return got
		}
	}
	return estimateTokens(innerBody)
}
