// Package anthropicapi implements the Anthropic-shaped ingress endpoints.
package anthropicapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/inferplane/inferplane/internal/audit"
	"github.com/inferplane/inferplane/internal/governance"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/metrics"
	"github.com/inferplane/inferplane/internal/pricing"
	"github.com/inferplane/inferplane/internal/principal"
	"github.com/inferplane/inferplane/internal/router"
	"github.com/inferplane/inferplane/pkg/schema"
	"github.com/inferplane/inferplane/pkg/ulid"
	"github.com/inferplane/inferplane/providers"
)

const ingressName = "anthropic"

type MessagesHandler struct {
	r       *router.Router
	aud     *audit.Writer        // nil-safe: unit tests may omit
	gov     *governance.Governor // nil-safe: governance disabled when nil
	metrics *metrics.Metrics     // nil-safe: no-op when nil
}

func NewMessagesHandler(r *router.Router) *MessagesHandler { return &MessagesHandler{r: r} }

func NewMessagesHandlerWithAudit(r *router.Router, aud *audit.Writer) *MessagesHandler {
	return &MessagesHandler{r: r, aud: aud}
}

// NewMessagesHandlerFull wires the governance pipeline (rate/quota/budget
// pre-check + cost settlement) alongside audit. gov may be nil to disable
// governance.
func NewMessagesHandlerFull(r *router.Router, aud *audit.Writer, gov *governance.Governor) *MessagesHandler {
	return &MessagesHandler{r: r, aud: aud, gov: gov}
}

// NewMessagesHandlerMetrics is NewMessagesHandlerFull plus the Prometheus
// metrics sink (request/token/duration/ttft/fallback). m may be nil (no-op).
func NewMessagesHandlerMetrics(r *router.Router, aud *audit.Writer, gov *governance.Governor, m *metrics.Metrics) *MessagesHandler {
	return &MessagesHandler{r: r, aud: aud, gov: gov, metrics: m}
}

func (h *MessagesHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	start := time.Now()
	raw, err := io.ReadAll(req.Body)
	if err != nil {
		writeErr(w, 400, "invalid_request_error", "could not read request body")
		return
	}
	// Parse for routing/observation ONLY. RawBody is forwarded verbatim.
	var parsed schema.ChatRequest
	if err := json.Unmarshal(raw, &parsed); err != nil {
		writeErr(w, 400, "invalid_request_error", "malformed JSON")
		return
	}
	// M3 enforcement: require an authenticated principal and check the
	// per-key model allow-list BEFORE resolving/forwarding (§3.1, §5.1).
	p, ok := principal.From(req.Context())
	if !ok {
		writeErr(w, 401, "authentication_error", "no principal")
		return
	}
	if !p.Allows(parsed.Model) {
		// A deny is recorded as a started record carrying the 403 outcome.
		h.audit(p, parsed.Model, "", &audit.OutcomeRef{Status: 403})
		h.metrics.ObserveRequest(ingressName, parsed.Model, "", p.Team, 403, time.Since(start).Seconds(), 0)
		writeErr(w, 403, "permission_error", "model not allowed for this key: "+parsed.Model)
		return
	}
	chain, err := h.r.ResolveChain(parsed.Model)
	if err != nil {
		// Unknown model is recorded as a started record carrying the 404 outcome,
		// for consistency with the 403 allow-list deny above.
		h.audit(p, parsed.Model, "", &audit.OutcomeRef{Status: 404})
		h.metrics.ObserveRequest(ingressName, parsed.Model, "", p.Team, 404, time.Since(start).Seconds(), 0)
		writeErr(w, 404, "not_found_error", "unknown model: "+parsed.Model)
		return
	}
	// Governance pre-check (rate/quota/budget) BEFORE the upstream call. A block
	// is recorded as a started record carrying the deny status.
	if h.gov != nil {
		dec := h.gov.PreCheck(p.Team, estimateTokens(raw))
		if !dec.Allowed {
			h.audit(p, parsed.Model, chain[0].Upstream, &audit.OutcomeRef{Status: dec.Status})
			h.metrics.ObserveRequest(ingressName, parsed.Model, chain[0].ProviderName, p.Team, dec.Status, time.Since(start).Seconds(), 0)
			writeErr(w, dec.Status, govErrType(dec.Status), dec.Reason)
			return
		}
	}
	// request_started: the request passed auth + allow-list + governance and
	// resolved a target (the first in the priority chain).
	h.audit(p, parsed.Model, chain[0].Upstream, nil)
	stream := parsed.Stream != nil && *parsed.Stream

	// Priority fallback chain (§4.5): try targets in order. A pre-TTFT failure
	// (Complete error, or Stream() error before the first event) falls back to
	// the next target, records the breaker result, and sets x-inferplane-fallback.
	// Once a stream yields its first event the response is committed — no fallback.
	for i, ct := range chain {
		pr := &providers.ProxyRequest{
			Model: parsed.Model, Upstream: ct.Upstream, Parsed: &parsed,
			RawBody: raw, Headers: req.Header, Stream: stream,
			IngressProtocol: "anthropic",
		}
		last := i == len(chain)-1
		if i > 0 {
			// We fell back to this target; advertise it to the client.
			w.Header().Set("x-inferplane-fallback", ct.ProviderName)
		}
		var retriable bool
		if stream {
			retriable = h.serveStream(w, req, ct.Provider, pr, p, parsed.Model, ct.ProviderName, ct.Upstream, last, start)
		} else {
			retriable = h.serveComplete(w, req, ct.Provider, pr, p, parsed.Model, ct.ProviderName, ct.Upstream, last, start)
		}
		if !retriable {
			return // committed (success, or terminal error on the last target)
		}
		// Pre-TTFT failure with a next target available → record + fall back.
		h.r.RecordResult(ct.ProviderName, false)
		h.metrics.ObserveFallback(parsed.Model, ct.ProviderName, chain[i+1].ProviderName, "upstream_error")
	}
}

// govErrType maps a governance deny status to the Anthropic error `type`.
func govErrType(status int) string {
	switch status {
	case 429:
		return "rate_limit_error"
	case 402:
		return "permission_error"
	default:
		return "api_error"
	}
}

// serveComplete proxies one non-streaming target. It returns retriable=true
// when the call failed pre-TTFT (transport error, or an upstream 5xx/429) AND a
// next target exists (!last) — the caller then falls back. Otherwise it writes
// the response/error to the client and returns false (committed).
func (h *MessagesHandler) serveComplete(w http.ResponseWriter, req *http.Request, prov providers.Provider, pr *providers.ProxyRequest, p keystore.Principal, model, providerName, upstream string, last bool, start time.Time) (retriable bool) {
	resp, err := prov.Complete(req.Context(), pr)
	if err != nil {
		if !last {
			return true // transport error → fall back
		}
		writeErr(w, 502, "api_error", "upstream error")
		h.auditCompleted(p, model, upstream, 502, nil, nil)
		h.metrics.ObserveRequest(ingressName, model, providerName, p.Team, 502, time.Since(start).Seconds(), 0)
		return false
	}
	// An upstream 5xx/429 is a retriable failure when a next target exists.
	if !last && (resp.StatusCode >= 500 || resp.StatusCode == 429) {
		return true
	}
	if resp.Headers != nil {
		copyUpstreamHeaders(w.Header(), resp.Headers)
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(resp.RawBody) // tee verbatim (incl. non-2xx error bodies)
	// A 2xx is a breaker success; a committed non-2xx on the last target is not
	// counted (it was teed as the client's real upstream error).
	if resp.StatusCode < 400 {
		h.r.RecordResult(providerName, true)
	}
	// resp.Parsed.Usage is the observation hook for M3 audit / M5 quota.
	var usage *audit.UsageRef
	var cost *audit.CostRef
	if resp.Parsed != nil {
		usage = usageRef(resp.Parsed.Usage)
		cost = h.settle(p.Team, providerName, model, upstream, resp.Parsed.Usage)
		h.observeTokens(model, providerName, p.Team, resp.Parsed.Usage)
	}
	h.auditCompleted(p, model, upstream, resp.StatusCode, usage, cost)
	h.metrics.ObserveRequest(ingressName, model, providerName, p.Team, resp.StatusCode, time.Since(start).Seconds(), 0)
	return false
}

// serveStream proxies one streaming target. Fallback is PRE-TTFT ONLY: if
// Stream() returns an error before any event is yielded AND a next target exists
// (!last), it returns retriable=true and the caller falls back. Once the first
// event is teed the response is committed; a mid-stream error terminates the
// stream (no fallback). Returns false in all committed cases.
func (h *MessagesHandler) serveStream(w http.ResponseWriter, req *http.Request, prov providers.Provider, pr *providers.ProxyRequest, p keystore.Principal, model, providerName, upstream string, last bool, start time.Time) (retriable bool) {
	seq, err := prov.Stream(req.Context(), pr)
	if err != nil {
		if !last {
			return true // pre-TTFT failure → fall back
		}
		// Tee a non-2xx upstream error verbatim (status/body) so the client
		// sees Anthropic's real rate-limit/error response, not a fabricated one.
		var ue *providers.UpstreamError
		if errors.As(err, &ue) {
			copyUpstreamHeaders(w.Header(), ue.Header)
			if w.Header().Get("Content-Type") == "" {
				w.Header().Set("Content-Type", "application/json")
			}
			w.WriteHeader(ue.StatusCode)
			w.Write(ue.Body)
			h.auditCompleted(p, model, upstream, ue.StatusCode, nil, nil)
			h.metrics.ObserveRequest(ingressName, model, providerName, p.Team, ue.StatusCode, time.Since(start).Seconds(), 0)
			return false
		}
		writeErr(w, 502, "api_error", "upstream stream error")
		h.auditCompleted(p, model, upstream, 502, nil, nil)
		h.metrics.ObserveRequest(ingressName, model, providerName, p.Team, 502, time.Since(start).Seconds(), 0)
		return false
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "api_error", "streaming unsupported")
		h.auditCompleted(p, model, upstream, 500, nil, nil)
		h.metrics.ObserveRequest(ingressName, model, providerName, p.Team, 500, time.Since(start).Seconds(), 0)
		return false
	}
	// Stream() succeeded → the target is healthy (breaker success, post-TTFT).
	h.r.RecordResult(providerName, true)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)
	var usage *audit.UsageRef
	var lastUsage *schema.Usage
	var ttft float64
	for ev, err := range seq {
		if err != nil {
			// upstream broke mid-stream; client sees truncated stream (M5: error event)
			h.auditCompletedPartial(p, model, upstream, usage)
			h.metrics.ObserveRequest(ingressName, model, providerName, p.Team, 200, time.Since(start).Seconds(), ttft)
			return false
		}
		if ttft == 0 {
			ttft = time.Since(start).Seconds() // first streamed event = time to first token
		}
		w.Write(ev.Raw) // tee original bytes verbatim
		flusher.Flush()
		// ev.Chunk.Usage on message_delta is the settlement observation point (M3/M5).
		if ev.Chunk != nil && ev.Chunk.Usage != nil {
			usage = usageRef(ev.Chunk.Usage)
			lastUsage = ev.Chunk.Usage
		}
	}
	cost := h.settle(p.Team, providerName, model, upstream, lastUsage)
	h.observeTokens(model, providerName, p.Team, lastUsage)
	h.auditCompleted(p, model, upstream, 200, usage, cost)
	h.metrics.ObserveRequest(ingressName, model, providerName, p.Team, 200, time.Since(start).Seconds(), ttft)
	return false
}

// settle maps the observed schema.Usage to pricing.Usage and runs the
// Governor's post-call settlement (quota debit + cost + budget debit), returning
// the audit CostRef. nil when governance is disabled or there is no usage.
//
// M5 mapping notes: schema does not yet split cache_creation by TTL, so the
// whole cache_creation_input_tokens total maps to CacheWrite5m as a
// conservative default (the cheaper 5m tier); cache_read maps to CacheRead.
func (h *MessagesHandler) settle(team, providerName, model, upstream string, u *schema.Usage) *audit.CostRef {
	if h.gov == nil || u == nil {
		return nil
	}
	pu := pricing.Usage{
		Input:        deref(u.InputTokens),
		Output:       deref(u.OutputTokens),
		CacheRead:    deref(u.CacheReadInputTokens),
		CacheWrite5m: deref(u.CacheCreationInputTokens),
	}
	cost, missing := h.gov.Settle(team, providerName, upstream, pu)
	return &audit.CostRef{
		AmountUSDMicros: cost,
		PricingMissing:  missing,
		PricingVersion:  h.gov.PricingVersion(),
	}
}

// observeTokens records the per-type token usage counters for one settled
// request. Mirrors the settle() mapping (cache_creation → cache_write_5m). The
// provider arg is the CONFIG provider name (pricing/metrics key), matching the
// request metric labels. No-op when usage is nil or metrics is nil.
func (h *MessagesHandler) observeTokens(model, provider, team string, u *schema.Usage) {
	if u == nil {
		return
	}
	h.metrics.ObserveTokenUsage("input", model, provider, team, deref(u.InputTokens))
	h.metrics.ObserveTokenUsage("output", model, provider, team, deref(u.OutputTokens))
	h.metrics.ObserveTokenUsage("cache_read", model, provider, team, deref(u.CacheReadInputTokens))
	h.metrics.ObserveTokenUsage("cache_write_5m", model, provider, team, deref(u.CacheCreationInputTokens))
}

// copyUpstreamHeaders tees upstream response headers to the client, skipping
// hop-by-hop headers Go manages itself. Preserves request-id and
// anthropic-ratelimit-*/retry-after so the client keeps its backoff signal.
func copyUpstreamHeaders(dst http.Header, src http.Header) {
	for k, vs := range src {
		switch http.CanonicalHeaderKey(k) {
		case "Content-Length", "Transfer-Encoding", "Connection":
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// audit emits a request_started record. A nil outcome is the normal "request
// admitted" case; a non-nil outcome (e.g. 403) records a denied request as a
// started record carrying that outcome (no completed record follows). No-op
// when the handler has no audit writer (unit tests).
func (h *MessagesHandler) audit(p keystore.Principal, model, upstream string, outcome *audit.OutcomeRef) {
	if h.aud == nil {
		return
	}
	h.aud.Append(audit.Record{
		SchemaVersion: 1,
		Event:         "request_started",
		ID:            ulid.New(),
		TS:            time.Now().UTC().Format(time.RFC3339Nano),
		Principal:     audit.PrincipalRef{KeyID: p.KeyID, Team: p.Team},
		Request:       audit.RequestRef{Ingress: "anthropic", ModelRequested: model, ModelResolved: upstream},
		Outcome:       outcome,
	})
}

// auditCompleted emits a request_completed record with the final status and
// observed usage. No-op without an audit writer.
func (h *MessagesHandler) auditCompleted(p keystore.Principal, model, upstream string, status int, usage *audit.UsageRef, cost *audit.CostRef) {
	if h.aud == nil {
		return
	}
	h.aud.Append(audit.Record{
		SchemaVersion: 1,
		Event:         "request_completed",
		ID:            ulid.New(),
		TS:            time.Now().UTC().Format(time.RFC3339Nano),
		Principal:     audit.PrincipalRef{KeyID: p.KeyID, Team: p.Team},
		Request:       audit.RequestRef{Ingress: "anthropic", ModelRequested: model, ModelResolved: upstream},
		Outcome:       &audit.OutcomeRef{Status: status},
		Usage:         usage,
		Cost:          cost,
	})
}

// auditCompletedPartial records a stream that broke mid-flight: status 200 was
// already sent to the client, but the response is partial.
func (h *MessagesHandler) auditCompletedPartial(p keystore.Principal, model, upstream string, usage *audit.UsageRef) {
	if h.aud == nil {
		return
	}
	h.aud.Append(audit.Record{
		SchemaVersion: 1,
		Event:         "request_completed",
		ID:            ulid.New(),
		TS:            time.Now().UTC().Format(time.RFC3339Nano),
		Principal:     audit.PrincipalRef{KeyID: p.KeyID, Team: p.Team},
		Request:       audit.RequestRef{Ingress: "anthropic", ModelRequested: model, ModelResolved: upstream},
		Outcome:       &audit.OutcomeRef{Status: 200, Partial: true},
		Usage:         usage,
	})
}

// usageRef maps an observed schema.Usage to the audit UsageRef, dereferencing
// the *int64 token fields nil-safe (a missing upstream key counts as 0).
func usageRef(u *schema.Usage) *audit.UsageRef {
	if u == nil {
		return nil
	}
	return &audit.UsageRef{
		InputTokens:              deref(u.InputTokens),
		OutputTokens:             deref(u.OutputTokens),
		CacheReadInputTokens:     deref(u.CacheReadInputTokens),
		CacheCreationInputTokens: deref(u.CacheCreationInputTokens),
	}
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func writeErr(w http.ResponseWriter, status int, errType, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": errType, "message": msg},
	})
}
