// Package anthropicapi implements the Anthropic-shaped ingress endpoints.
package anthropicapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/inferplane/inferplane/internal/audit"
	"github.com/inferplane/inferplane/internal/filter"
	"github.com/inferplane/inferplane/internal/governance"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/metrics"
	"github.com/inferplane/inferplane/internal/pricing"
	"github.com/inferplane/inferplane/internal/principal"
	"github.com/inferplane/inferplane/internal/router"
	"github.com/inferplane/inferplane/internal/tracing"
	"github.com/inferplane/inferplane/pkg/schema"
	"github.com/inferplane/inferplane/pkg/ulid"
	"github.com/inferplane/inferplane/providers"
	"go.opentelemetry.io/otel/trace"
)

const ingressName = "anthropic"

// rejectedModelLabel is the bounded sentinel used as the Prometheus `model`
// label on pre-resolution rejections (403 allow-list deny / 404 unknown model).
// At those points the model string is still attacker-controlled and has NOT been
// validated against config; recording it raw would let a client mint unbounded
// metric series (a cardinality DoS, §6.2 — team/model labels must come from
// config-declared values only). The requested model is still kept in the audit
// record, which is not a Prometheus label and carries no cardinality concern.
const rejectedModelLabel = "_rejected"

type MessagesHandler struct {
	r       *router.Router
	aud     *audit.Writer        // nil-safe: unit tests may omit
	gov     *governance.Governor // nil-safe: governance disabled when nil
	metrics *metrics.Metrics     // nil-safe: no-op when nil
	mask    *filter.Masking      // nil-safe: masking off when nil (ADR-009)
}

// SetMasking enables the PII masking filter for the configured teams (ADR-009).
// nil-safe: leaving it unset keeps the verbatim fast path with zero overhead.
func (h *MessagesHandler) SetMasking(m *filter.Masking) { h.mask = m }

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
	// Tracing (ADR-011): join the client's trace (W3C traceparent), start ONE
	// server span owned across the whole request (incl. the fallback loop) and
	// end it exactly once via defer; no-op when tracing is off. The provider
	// system / response model / usage / terminal status are set later in the
	// serve methods via the span in the request context.
	tctx := tracing.Extract(req.Context(), req.Header)
	tctx, span := tracing.Start(tctx, "chat "+parsed.Model)
	defer span.End()
	req = req.WithContext(tctx)
	tracing.SetGenAIRequest(span, parsed.Model)
	traceID := tracing.TraceID(tctx)
	// M3 enforcement: require an authenticated principal and check the
	// per-key model allow-list BEFORE resolving/forwarding (§3.1, §5.1).
	p, ok := principal.From(req.Context())
	if !ok {
		writeErr(w, 401, "authentication_error", "no principal")
		return
	}
	if !p.Allows(parsed.Model) {
		// A deny is recorded as a started record carrying the 403 outcome.
		h.audit(p, parsed.Model, "", &audit.OutcomeRef{Status: 403}, false, traceID)
		// Pre-resolution reject: model is still attacker-controlled → sentinel label.
		h.metrics.ObserveRequest(ingressName, rejectedModelLabel, "", p.Team, 403, time.Since(start).Seconds(), 0)
		writeErr(w, 403, "permission_error", "model not allowed for this key: "+parsed.Model)
		return
	}
	chain, st, err := h.r.ResolveChain(parsed.Model)
	if err != nil {
		// Unknown model is recorded as a started record carrying the 404 outcome,
		// for consistency with the 403 allow-list deny above.
		h.audit(p, parsed.Model, "", &audit.OutcomeRef{Status: 404}, false, traceID)
		// Pre-resolution reject: model is still attacker-controlled → sentinel label.
		h.metrics.ObserveRequest(ingressName, rejectedModelLabel, "", p.Team, 404, time.Since(start).Seconds(), 0)
		writeErr(w, 404, "not_found_error", "unknown model: "+parsed.Model)
		return
	}
	// PII masking (ADR-009): for a masked team, mask request text BEFORE the
	// governance estimate and the upstream call. Masking updates BOTH RawBody and
	// the parsed request (the openai_compatible provider converts from Parsed, not
	// RawBody — masking only one would leak PII). FAIL CLOSED: a masker error
	// rejects the request; the unmasked body is never forwarded.
	piiMasked := false
	if h.mask.Enabled(p.Team) {
		masked, n, err := maskBody(raw, h.mask.Filter)
		if err != nil {
			h.audit(p, parsed.Model, chain[0].Upstream, &audit.OutcomeRef{Status: 400}, false, traceID)
			h.metrics.ObserveRequest(ingressName, parsed.Model, chain[0].ProviderName, p.Team, 400, time.Since(start).Seconds(), 0)
			writeErr(w, 400, "invalid_request_error", "request could not be PII-masked")
			return
		}
		if n > 0 {
			var reparsed schema.ChatRequest
			if err := json.Unmarshal(masked, &reparsed); err != nil {
				h.audit(p, parsed.Model, chain[0].Upstream, &audit.OutcomeRef{Status: 400}, false, traceID)
				h.metrics.ObserveRequest(ingressName, parsed.Model, chain[0].ProviderName, p.Team, 400, time.Since(start).Seconds(), 0)
				writeErr(w, 400, "invalid_request_error", "request could not be PII-masked")
				return
			}
			raw = masked
			parsed = reparsed
			piiMasked = true
			h.metrics.ObservePIIMask(p.Team, n)
		}
	}
	// Pricing table from the SAME generation we resolved on (ADR-006): a reload
	// between now and Settle must not bill at a different generation's rates.
	table := st.Pricing()
	// Governance pre-check (rate/quota/budget) BEFORE the upstream call. A block
	// is recorded as a started record carrying the deny status.
	if h.gov != nil {
		dec := h.gov.PreCheck(p.Team, estimateTokens(raw))
		if !dec.Allowed {
			h.audit(p, parsed.Model, chain[0].Upstream, &audit.OutcomeRef{Status: dec.Status}, false, traceID)
			h.metrics.ObserveRequest(ingressName, parsed.Model, chain[0].ProviderName, p.Team, dec.Status, time.Since(start).Seconds(), 0)
			writeErr(w, dec.Status, govErrType(dec.Status), dec.Reason)
			return
		}
	}
	// request_started: the request passed auth + allow-list + governance and
	// resolved a target (the first in the priority chain).
	h.audit(p, parsed.Model, chain[0].Upstream, nil, piiMasked, traceID)
	stream := parsed.Stream != nil && *parsed.Stream

	// Priority fallback chain (§4.5): try targets in order. A pre-TTFT failure
	// (Complete error, or Stream() error before the first event) falls back to
	// the next target, records the breaker result, and sets x-inferplane-fallback.
	// Once a stream yields its first event the response is committed — no fallback.
	for i, ct := range chain {
		// Inject the trace context into a CLONE of the inbound headers per attempt
		// (ADR-011 gate): never mutate the shared req.Header or bleed across
		// attempts; the body is untouched (header-only — cache-safe §4.4).
		upHeaders := req.Header.Clone()
		tracing.Inject(req.Context(), upHeaders)
		pr := &providers.ProxyRequest{
			Model: parsed.Model, Upstream: ct.Upstream, Parsed: &parsed,
			RawBody: raw, Headers: upHeaders, Stream: stream,
			IngressProtocol: "anthropic",
		}
		last := i == len(chain)-1
		if i > 0 {
			// We fell back to this target; advertise it to the client.
			w.Header().Set("x-inferplane-fallback", ct.ProviderName)
		}
		var retriable bool
		if stream {
			retriable = h.serveStream(w, req, ct.Provider, pr, p, parsed.Model, ct.ProviderName, ct.Identity, ct.Upstream, last, start, table)
		} else {
			retriable = h.serveComplete(w, req, ct.Provider, pr, p, parsed.Model, ct.ProviderName, ct.Identity, ct.Upstream, last, start, table)
		}
		if !retriable {
			return // committed (success, or terminal error on the last target)
		}
		// Pre-TTFT failure with a next target available → record + fall back.
		h.r.RecordResult(ct.ProviderName, ct.Identity, false)
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
func (h *MessagesHandler) serveComplete(w http.ResponseWriter, req *http.Request, prov providers.Provider, pr *providers.ProxyRequest, p keystore.Principal, model, providerName, identity, upstream string, last bool, start time.Time, table *pricing.Table) (retriable bool) {
	resp, err := prov.Complete(req.Context(), pr)
	if err != nil {
		if !last {
			return true // transport error → fall back
		}
		writeErr(w, 502, "api_error", "upstream error")
		h.auditCompleted(p, model, upstream, 502, nil, nil, tracing.TraceID(req.Context()))
		recordSpanResponse(req, prov.Name(), upstream, nil, false) // terminal
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
		h.r.RecordResult(providerName, identity, true)
	}
	// resp.Parsed.Usage is the observation hook for M3 audit / M5 quota.
	var usage *audit.UsageRef
	var cost *audit.CostRef
	if resp.Parsed != nil {
		usage = usageRef(resp.Parsed.Usage)
		cost = h.settle(p.Team, providerName, model, upstream, resp.Parsed.Usage, table)
		h.observeTokens(model, providerName, p.Team, resp.Parsed.Usage)
	}
	h.auditCompleted(p, model, upstream, resp.StatusCode, usage, cost, tracing.TraceID(req.Context()))
	recordSpanResponse(req, prov.Name(), upstream, usage, resp.StatusCode < 400)
	h.metrics.ObserveRequest(ingressName, model, providerName, p.Team, resp.StatusCode, time.Since(start).Seconds(), 0)
	return false
}

// serveStream proxies one streaming target. Fallback is PRE-TTFT ONLY: if
// Stream() returns an error before any event is yielded AND a next target exists
// (!last), it returns retriable=true and the caller falls back. Once the first
// event is teed the response is committed; a mid-stream error terminates the
// stream (no fallback). Returns false in all committed cases.
func (h *MessagesHandler) serveStream(w http.ResponseWriter, req *http.Request, prov providers.Provider, pr *providers.ProxyRequest, p keystore.Principal, model, providerName, identity, upstream string, last bool, start time.Time, table *pricing.Table) (retriable bool) {
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
			h.auditCompleted(p, model, upstream, ue.StatusCode, nil, nil, tracing.TraceID(req.Context()))
			recordSpanResponse(req, prov.Name(), upstream, nil, false) // terminal
			h.metrics.ObserveRequest(ingressName, model, providerName, p.Team, ue.StatusCode, time.Since(start).Seconds(), 0)
			return false
		}
		writeErr(w, 502, "api_error", "upstream stream error")
		h.auditCompleted(p, model, upstream, 502, nil, nil, tracing.TraceID(req.Context()))
		recordSpanResponse(req, prov.Name(), upstream, nil, false) // terminal
		h.metrics.ObserveRequest(ingressName, model, providerName, p.Team, 502, time.Since(start).Seconds(), 0)
		return false
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "api_error", "streaming unsupported")
		h.auditCompleted(p, model, upstream, 500, nil, nil, tracing.TraceID(req.Context()))
		recordSpanResponse(req, prov.Name(), upstream, nil, false) // terminal
		h.metrics.ObserveRequest(ingressName, model, providerName, p.Team, 500, time.Since(start).Seconds(), 0)
		return false
	}
	// Stream() succeeded → the target is healthy (breaker success, post-TTFT).
	h.r.RecordResult(providerName, identity, true)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)
	var usage *audit.UsageRef
	var lastUsage *schema.Usage
	var ttft float64
	for ev, err := range seq {
		if err != nil {
			// upstream broke mid-stream; client sees truncated stream (M5: error event)
			h.auditCompletedPartial(p, model, upstream, usage, tracing.TraceID(req.Context()))
			recordSpanResponse(req, prov.Name(), upstream, usage, true) // committed (partial)
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
	cost := h.settle(p.Team, providerName, model, upstream, lastUsage, table)
	h.observeTokens(model, providerName, p.Team, lastUsage)
	h.auditCompleted(p, model, upstream, 200, usage, cost, tracing.TraceID(req.Context()))
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
func (h *MessagesHandler) settle(team, providerName, model, upstream string, u *schema.Usage, table *pricing.Table) *audit.CostRef {
	if h.gov == nil || u == nil {
		return nil
	}
	pu := pricing.Usage{
		Input:        deref(u.InputTokens),
		Output:       deref(u.OutputTokens),
		CacheRead:    deref(u.CacheReadInputTokens),
		CacheWrite5m: deref(u.CacheCreationInputTokens),
	}
	cost, missing := h.gov.Settle(team, providerName, upstream, pu, table)
	return &audit.CostRef{
		AmountUSDMicros: cost,
		PricingMissing:  missing,
		PricingVersion:  governance.PricingVersionOf(table),
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
func (h *MessagesHandler) audit(p keystore.Principal, model, upstream string, outcome *audit.OutcomeRef, piiMasked bool, traceID string) {
	if h.aud == nil {
		return
	}
	rec := audit.Record{
		SchemaVersion: 1,
		Event:         "request_started",
		ID:            ulid.New(),
		TS:            time.Now().UTC().Format(time.RFC3339Nano),
		Principal:     audit.PrincipalRef{KeyID: p.KeyID, Team: p.Team},
		Request:       audit.RequestRef{Ingress: "anthropic", ModelRequested: model, ModelResolved: upstream, PIIMasked: piiMasked},
		Outcome:       outcome,
	}
	if traceID != "" {
		rec.TraceID = &traceID
	}
	h.aud.Append(rec)
}

// auditCompleted emits a request_completed record with the final status and
// observed usage. No-op without an audit writer.
func (h *MessagesHandler) auditCompleted(p keystore.Principal, model, upstream string, status int, usage *audit.UsageRef, cost *audit.CostRef, traceID string) {
	if h.aud == nil {
		return
	}
	rec := audit.Record{
		SchemaVersion: 1,
		Event:         "request_completed",
		ID:            ulid.New(),
		TS:            time.Now().UTC().Format(time.RFC3339Nano),
		Principal:     audit.PrincipalRef{KeyID: p.KeyID, Team: p.Team},
		Request:       audit.RequestRef{Ingress: "anthropic", ModelRequested: model, ModelResolved: upstream},
		Outcome:       &audit.OutcomeRef{Status: status},
		Usage:         usage,
		Cost:          cost,
	}
	if traceID != "" {
		rec.TraceID = &traceID
	}
	h.aud.Append(rec)
}

// auditCompletedPartial records a stream that broke mid-flight: status 200 was
// already sent to the client, but the response is partial.
func (h *MessagesHandler) auditCompletedPartial(p keystore.Principal, model, upstream string, usage *audit.UsageRef, traceID string) {
	if h.aud == nil {
		return
	}
	rec := audit.Record{
		SchemaVersion: 1,
		Event:         "request_completed",
		ID:            ulid.New(),
		TS:            time.Now().UTC().Format(time.RFC3339Nano),
		Principal:     audit.PrincipalRef{KeyID: p.KeyID, Team: p.Team},
		Request:       audit.RequestRef{Ingress: "anthropic", ModelRequested: model, ModelResolved: upstream},
		Outcome:       &audit.OutcomeRef{Status: 200, Partial: true},
		Usage:         usage,
	}
	if traceID != "" {
		rec.TraceID = &traceID
	}
	h.aud.Append(rec)
}

// recordSpanResponse sets the response-side GenAI attributes + terminal status on
// the request span (from req's context). ok=false marks the span errored —
// callers pass false ONLY on a terminal (non-retriable) outcome, so a request
// that recovers via fallback is not left red (ADR-011 gate). No-op span when off.
func recordSpanResponse(req *http.Request, system, upstream string, usage *audit.UsageRef, ok bool) {
	span := trace.SpanFromContext(req.Context())
	var in, out int64
	if usage != nil {
		in, out = usage.InputTokens, usage.OutputTokens
	}
	tracing.SetGenAIResponse(span, system, upstream, in, out)
	tracing.SetStatus(span, ok, "")
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
