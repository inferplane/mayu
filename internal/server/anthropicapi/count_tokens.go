package anthropicapi

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/inferplane/inferplane/internal/filter"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/principal"
	"github.com/inferplane/inferplane/internal/router"
	"github.com/inferplane/inferplane/pkg/schema"
	"github.com/inferplane/inferplane/providers"
)

type CountTokensHandler struct {
	r          *router.Router
	mask       *filter.Masking                               // nil-safe: masking off when nil (ADR-009)
	teamPolicy func(team string) (keystore.TeamRecord, bool) // nil-safe: no region lock when nil (D7, ADR-020)
}

func NewCountTokensHandler(r *router.Router) *CountTokensHandler { return &CountTokensHandler{r: r} }

// SetMasking enables PII masking on the count path (ADR-009). nil-safe.
func (h *CountTokensHandler) SetMasking(m *filter.Masking) { h.mask = m }

// SetTeamPolicy installs the same fresh-per-request team-record lookup as
// MessagesHandler (D6/D7, ADR-016 pattern), so a region-restricted team's
// count_tokens call never reaches an out-of-region provider's real
// CountTokens API — it falls back to the local estimator instead (below).
func (h *CountTokensHandler) SetTeamPolicy(fn func(team string) (keystore.TeamRecord, bool)) {
	h.teamPolicy = fn
}

// ServeHTTP NEVER returns a non-200 / non-JSON response. A 501/4xx/5xx here
// crashes Claude Code (truncated-JSON crash, design doc §3.1). On any failure
// it falls back to a conservative estimate and still returns
// {"input_tokens": N} with HTTP 200.
func (h *CountTokensHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	raw, _ := io.ReadAll(req.Body)
	n := h.count(req, raw)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	json.NewEncoder(w).Encode(map[string]int64{"input_tokens": n})
}

func (h *CountTokensHandler) count(req *http.Request, raw []byte) int64 {
	var parsed schema.ChatRequest
	_ = json.Unmarshal(raw, &parsed) // best-effort; estimator works on raw bytes too
	model := h.r.Canonical(parsed.Model)
	// PII masking (ADR-009): mask BEFORE forwarding to the upstream counter so the
	// count reflects what is sent AND the upstream never sees unmasked PII. On a
	// masker error, return a LOCAL estimate — never forward unmasked, never 500.
	if p, ok := principal.From(req.Context()); ok && h.mask.Enabled(p.Team) {
		masked, n, err := maskBody(raw, h.mask.Filter)
		if err != nil {
			return estimateTokens(raw) // local, no upstream call, never leaks
		}
		if n > 0 {
			raw = masked
		}
	}
	chain, _, err := h.r.ResolveChain(model)
	if err != nil || len(chain) == 0 {
		return estimateTokens(raw)
	}
	// Region lock (D7, ADR-020): drop out-of-region targets before the real
	// CountTokens call — count_tokens must never send content to a provider
	// the team isn't allowed to reach. If that empties the chain, fall back to
	// the local estimator; a known, documented gap (ADR-020), never a non-200.
	if p, ok := principal.From(req.Context()); ok && h.teamPolicy != nil {
		if rec, ok := h.teamPolicy(p.Team); ok && len(rec.AllowedRegions) > 0 {
			chain = router.FilterRegions(chain, rec.AllowedRegions)
		}
	}
	if len(chain) == 0 {
		return estimateTokens(raw)
	}
	ct := chain[0]
	if tc, ok := ct.Provider.(providers.TokenCounter); ok {
		pr := &providers.ProxyRequest{Model: model, Upstream: ct.Upstream, RawBody: raw, Headers: req.Header}
		if got, cerr := tc.CountTokens(req.Context(), pr); cerr == nil {
			return got
		}
	}
	return estimateTokens(raw)
}

// estimateTokens is the conservative fallback for providers without a
// TokenCounter (M2: none; M4/M5 may bundle a tokenizer per §10 #1). ~4 bytes
// per token is a coarse upper-ish bound; valid output matters more than
// precision here.
func estimateTokens(raw []byte) int64 {
	n := int64(len(raw) / 4)
	if n < 1 {
		n = 1
	}
	return n
}
