package anthropicapi

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/inferplane/inferplane/internal/audit"
	"github.com/inferplane/inferplane/internal/filter"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/metrics"
	"github.com/inferplane/inferplane/internal/principal"
	"github.com/inferplane/inferplane/internal/router"
	"github.com/inferplane/inferplane/internal/server/requestpolicy"
	"github.com/inferplane/inferplane/pkg/schema"
	"github.com/inferplane/inferplane/providers"
)

type CountTokensHandler struct {
	ready      func() (bool, string)
	aud        *audit.Writer
	metrics    *metrics.Metrics
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

// SetGovernanceGate installs the SAME gate used by DataMux generation admission.
func (h *CountTokensHandler) SetGovernanceGate(gate func() (bool, string)) { h.ready = gate }

// SetObservability enables safe policy evidence; legacy count traffic stays silent.
func (h *CountTokensHandler) SetObservability(aud *audit.Writer, m *metrics.Metrics) {
	h.aud, h.metrics = aud, m
}

// ServeHTTP NEVER returns a non-200 / non-JSON response. A 501/4xx/5xx here
// crashes Claude Code (truncated-JSON crash, design doc §3.1). On any failure
// it falls back to a conservative estimate and still returns
// {"input_tokens": N} with HTTP 200.
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
	w.WriteHeader(200)
	json.NewEncoder(w).Encode(map[string]int64{"input_tokens": n})
}

func (h *CountTokensHandler) count(w http.ResponseWriter, req *http.Request, raw []byte) int64 {
	var parsed schema.ChatRequest
	_ = json.Unmarshal(raw, &parsed) // best-effort; estimator works on raw bytes too
	model, _ := h.r.ResolveModel(parsed.Model)
	// RBAC: a key must not trigger a real upstream CountTokens call for a
	// model outside its allow-list — fall back to the local estimate instead
	// (still 200; the never-non-200 mandate holds, but the upstream never
	// sees content the key isn't entitled to send it). Mirrors bedrockapi's
	// CountTokensHandler.count.
	if p, ok := principal.From(req.Context()); ok && !h.r.Allows(p, model) {
		return estimateTokens(raw)
	}

	chain, st, err := h.r.ResolveChain(model)
	if err != nil {
		return estimateTokens(raw)
	}
	// RBAC re-check (C3): ResolveChain may have appended a cross-model
	// fallback's targets AFTER the pre-routing Allows check above already ran
	// against `model` alone — re-check here, BEFORE the region filter below,
	// or a key allowed only `model` could silently reach the fallback model's
	// upstream once the region filter promotes it to chain[0]. Mirrors
	// messages.go's identical call, same ordering requirement
	// (FilterModelAllowed derives "primary" from chain[0].Model, which must
	// still be the original model at this point).
	if p, ok := principal.From(req.Context()); ok {
		chain = router.FilterModelAllowed(chain, func(m string) bool { return h.r.Allows(p, m) })
	}
	// Region lock (D7, ADR-020): drop out-of-region targets before the real
	// CountTokens call — count_tokens must never send content to a provider
	// the team isn't allowed to reach. If that empties the chain, fall back to
	// the local estimator; a known, documented gap (ADR-020), never a non-200.
	p, authenticated := principal.From(req.Context())
	if !authenticated {
		return estimateTokens(raw)
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
		Principal: p, Protocol: "anthropic", RawBody: raw, RequestedModel: model,
		Model: model, Chain: chain, State: st, AllowedRegions: regions, CountOnly: true,
	})
	req = requestpolicy.Observe(w, req, result, h.metrics)
	requestpolicy.CountRecord(h.aud, req, "anthropic", false)
	// Evaluate the final request context at return, after an actual attempt if any.
	defer func() { requestpolicy.CountRecord(h.aud, req, "anthropic", true) }()
	if routeErr != nil {
		return estimateTokens(raw)
	}
	chain = result.Chain
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

	ct := chain[0]
	if tc, ok := ct.Provider.(providers.TokenCounter); ok {
		req = req.WithContext(audit.WithRoutingAttempt(req.Context(), ct.Model, ct.ProviderName, ct.DataBoundary))
		pr := &providers.ProxyRequest{Model: ct.Model, IngressProtocol: "anthropic", Upstream: ct.Upstream, RawBody: raw, Headers: req.Header}
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
