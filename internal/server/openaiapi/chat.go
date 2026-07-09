// Package openaiapi implements the OpenAI-shaped ingress endpoints
// (/v1/chat/completions + a content-negotiated /v1/models). It mirrors the
// Anthropic ingress (internal/server/anthropicapi) but speaks the OpenAI wire
// protocol and, when the resolved provider's native wire is NOT OpenAI,
// CONVERTS the canonical (Anthropic-superset) response into OpenAI shape on the
// way out. Lives in its own package so internal/server can import it without an
// import cycle (server → openaiapi is fine; openaiapi must not import server).
package openaiapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/inferplane/inferplane/internal/audit"
	"github.com/inferplane/inferplane/internal/bodystore"
	"github.com/inferplane/inferplane/internal/filter"
	"github.com/inferplane/inferplane/internal/governance"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/metrics"
	"github.com/inferplane/inferplane/internal/openai"
	"github.com/inferplane/inferplane/internal/pricing"
	"github.com/inferplane/inferplane/internal/principal"
	"github.com/inferplane/inferplane/internal/router"
	"github.com/inferplane/inferplane/internal/tracing"
	"github.com/inferplane/inferplane/pkg/schema"
	"github.com/inferplane/inferplane/pkg/ulid"
	"github.com/inferplane/inferplane/providers"
	"go.opentelemetry.io/otel/trace"
)

const ingressName = "openai"

// rejectedModelLabel is the bounded sentinel used as the Prometheus `model`
// label on pre-resolution rejections (403 allow-list deny / 404 unknown model).
// At those points the model string is still attacker-controlled and has NOT been
// validated against config; recording it raw would let a client mint unbounded
// metric series (a cardinality DoS, §6.2 — team/model labels must come from
// config-declared values only). The requested model is still kept in the audit
// record, which is not a Prometheus label and carries no cardinality concern.
const rejectedModelLabel = "_rejected"

type ChatHandler struct {
	r          *router.Router
	aud        *audit.Writer                                 // nil-safe: unit tests may omit
	gov        *governance.Governor                          // nil-safe: governance disabled when nil
	metrics    *metrics.Metrics                              // nil-safe: no-op when nil
	mask       *filter.Masking                               // nil-safe: masking off when nil (ADR-009)
	teamPolicy func(team string) (keystore.TeamRecord, bool) // nil-safe: no per-team overrides when nil (D6/D7, ADR-016 fresh-read pattern)
	bodies     *bodystore.Recorder                           // nil-safe: body capture off when nil (D4, ADR-018)
}

// SetMasking wires the masking decision. v1 does NOT mask the OpenAI ingress, so
// a masked team is REJECTED here (fail closed) — it must not bypass the control
// by switching protocol (ADR-009 round-2 CRITICAL). nil-safe.
func (h *ChatHandler) SetMasking(m *filter.Masking) { h.mask = m }

// SetTeamPolicy installs a fresh-per-request team-record lookup (mirrors
// anthropicapi.MessagesHandler.SetTeamPolicy — same ADR-016 posture) for
// per-team overrides that live on the team record but are not governance:
// D6/ADR-019's guardrail override today; D7/ADR-020's region-lock reuses it.
func (h *ChatHandler) SetTeamPolicy(fn func(team string) (keystore.TeamRecord, bool)) {
	h.teamPolicy = fn
}

// SetBodyRecorder enables opt-in request/response body capture (D4, ADR-018).
// nil-safe: leaving it unset keeps the zero-overhead fast path.
func (h *ChatHandler) SetBodyRecorder(r *bodystore.Recorder) { h.bodies = r }

func NewChatHandler(r *router.Router) *ChatHandler { return &ChatHandler{r: r} }

// NewChatHandlerFull wires the governance pipeline (rate/quota/budget pre-check
// + cost settlement) alongside audit. gov may be nil to disable governance.
func NewChatHandlerFull(r *router.Router, aud *audit.Writer, gov *governance.Governor) *ChatHandler {
	return &ChatHandler{r: r, aud: aud, gov: gov}
}

// NewChatHandlerMetrics is NewChatHandlerFull plus the Prometheus metrics sink
// (request/token/duration/ttft/fallback). m may be nil (no-op).
func NewChatHandlerMetrics(r *router.Router, aud *audit.Writer, gov *governance.Governor, m *metrics.Metrics) *ChatHandler {
	return &ChatHandler{r: r, aud: aud, gov: gov, metrics: m}
}

// providerWire reports the native wire protocol a provider speaks, by name.
// openai_compatible (vLLM/Ollama/llm-d) is "openai" → the ingress tees its
// RawBody/Raw verbatim. Everything else (anthropic/bedrock/mock) is "anthropic"
// → the ingress CONVERTS the canonical Parsed/Chunk into OpenAI shape.
func providerWire(name string) string {
	if name == "openai_compatible" {
		return "openai"
	}
	return "anthropic"
}

func (h *ChatHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	start := time.Now()
	raw, err := io.ReadAll(req.Body)
	if err != nil {
		writeErr(w, 400, "invalid_request_error", "could not read request body")
		return
	}
	// Parse OpenAI body into canonical for routing/governance/observation. The
	// original OpenAI bytes (raw) are still carried for verbatim forwarding to
	// an openai-wire provider.
	canonical, err := openai.RequestToCanonical(raw)
	if err != nil {
		writeErr(w, 400, "invalid_request_error", "malformed JSON")
		return
	}
	// Tracing (ADR-011): join the client trace, start ONE server span across the
	// request, end once via defer; no-op when off.
	tctx := tracing.Extract(req.Context(), req.Header)
	tctx, span := tracing.Start(tctx, "chat "+canonical.Model)
	defer span.End()
	req = req.WithContext(tctx)
	tracing.SetGenAIRequest(span, canonical.Model)
	traceID := tracing.TraceID(tctx)
	// Require an authenticated principal and enforce the per-key model
	// allow-list BEFORE resolving/forwarding (§3.1, §5.1).
	p, ok := principal.From(req.Context())
	if !ok {
		tracing.SetStatus(span, false, "no principal")
		writeErr(w, 401, "authentication_error", "no principal")
		return
	}
	// Fail closed for masked teams on the OpenAI ingress (ADR-009 round-2
	// CRITICAL): v1 masks only the Anthropic ingress, so a masked team must not
	// bypass PII masking by using /v1/chat/completions. Reject until OpenAI-ingress
	// masking ships.
	if h.mask.Enabled(p.Team) {
		// Audit the security-critical rejection (a masking-bypass attempt) — a
		// silent reject would be a blind spot in the tamper-evident chain (P4 gate).
		h.audit(p, canonical.Model, "", &audit.OutcomeRef{Status: 400}, traceID)
		h.metrics.ObserveRequest(ingressName, rejectedModelLabel, "", p.Team, 400, time.Since(start).Seconds(), 0)
		tracing.SetStatus(span, false, "pii mask bypass blocked")
		writeErr(w, 400, "invalid_request_error", "PII masking is enabled for your team but not supported on the OpenAI-compatible endpoint yet; use /v1/messages")
		return
	}
	if !p.Allows(canonical.Model) {
		h.audit(p, canonical.Model, "", &audit.OutcomeRef{Status: 403}, traceID)
		// Pre-resolution reject: model is still attacker-controlled → sentinel label.
		h.metrics.ObserveRequest(ingressName, rejectedModelLabel, "", p.Team, 403, time.Since(start).Seconds(), 0)
		tracing.SetStatus(span, false, "model not allowed")
		writeErr(w, 403, "permission_error", "model not allowed for this key: "+canonical.Model)
		return
	}
	chain, st, err := h.r.ResolveChain(canonical.Model)
	if err != nil {
		h.audit(p, canonical.Model, "", &audit.OutcomeRef{Status: 404}, traceID)
		// Pre-resolution reject: model is still attacker-controlled → sentinel label.
		h.metrics.ObserveRequest(ingressName, rejectedModelLabel, "", p.Team, 404, time.Since(start).Seconds(), 0)
		tracing.SetStatus(span, false, "unknown model")
		writeErr(w, 404, "not_found_error", "unknown model: "+canonical.Model)
		return
	}
	// Governance pre-check (rate/quota/budget) BEFORE the upstream call.
	// Pricing table from the SAME generation we resolved on (ADR-006).
	table := st.Pricing()
	if h.gov != nil {
		dec := h.gov.PreCheck(p.Team, p.KeyID, keyPolicyOf(p), estimateTokens(raw))
		if !dec.Allowed {
			h.audit(p, canonical.Model, chain[0].Upstream, &audit.OutcomeRef{Status: dec.Status}, traceID)
			h.metrics.ObserveRequest(ingressName, canonical.Model, chain[0].ProviderName, p.Team, dec.Status, time.Since(start).Seconds(), 0)
			tracing.SetStatus(span, false, "governance deny")
			writeErr(w, dec.Status, govErrType(dec.Status), dec.Reason)
			return
		}
	}
	// request_started: the request passed auth + allow-list + governance and
	// resolved a target (the first in the priority chain).
	h.audit(p, canonical.Model, chain[0].Upstream, nil, traceID)
	stream := canonical.Stream != nil && *canonical.Stream

	// Team-record per-team guardrail override (D6, ADR-019) — one fresh
	// lookup for the whole request, reused across every fallback attempt.
	var teamGuardrailID, teamGuardrailVersion string
	if h.teamPolicy != nil {
		if rec, ok := h.teamPolicy(p.Team); ok {
			teamGuardrailID, teamGuardrailVersion = rec.GuardrailID, rec.GuardrailVersion
		}
	}

	// Priority fallback chain (§4.5): try targets in order. A pre-TTFT failure
	// falls back to the next target, records the breaker result, and sets
	// x-inferplane-fallback. Stream fallback is pre-first-event only.
	for i, ct := range chain {
		upHeaders := req.Header.Clone()
		tracing.Inject(req.Context(), upHeaders)
		pr := &providers.ProxyRequest{
			Model: canonical.Model, Upstream: ct.Upstream, Parsed: canonical,
			RawBody: raw, Headers: upHeaders, Stream: stream,
			IngressProtocol:  "openai",
			GuardrailID:      teamGuardrailID,
			GuardrailVersion: teamGuardrailVersion,
		}
		last := i == len(chain)-1
		if i > 0 {
			w.Header().Set("x-inferplane-fallback", ct.ProviderName)
		}
		var retriable bool
		if stream {
			retriable = h.serveStream(w, req, ct.Provider, pr, p, canonical.Model, ct.ProviderName, ct.Identity, ct.Upstream, last, start, table)
		} else {
			retriable = h.serveComplete(w, req, ct.Provider, pr, p, canonical.Model, ct.ProviderName, ct.Identity, ct.Upstream, last, start, table)
		}
		if !retriable {
			return
		}
		h.r.RecordResult(ct.ProviderName, ct.Identity, false)
		h.metrics.ObserveFallback(canonical.Model, ct.ProviderName, chain[i+1].ProviderName, "upstream_error")
	}
}

// serveComplete proxies one non-streaming target. It returns retriable=true on a
// pre-TTFT failure (transport error or upstream 5xx/429) when a next target
// exists (!last); otherwise it writes the response/error and returns false.
func (h *ChatHandler) serveComplete(w http.ResponseWriter, req *http.Request, prov providers.Provider, pr *providers.ProxyRequest, p keystore.Principal, model, providerName, identity, upstream string, last bool, start time.Time, table *pricing.Table) (retriable bool) {
	resp, err := prov.Complete(req.Context(), pr)
	if err != nil {
		if !last {
			return true // fall back to the next target
		}
		// Tee a non-2xx upstream error verbatim when available.
		var ue *providers.UpstreamError
		if errors.As(err, &ue) {
			if w.Header().Get("Content-Type") == "" {
				w.Header().Set("Content-Type", "application/json")
			}
			w.WriteHeader(ue.StatusCode)
			w.Write(ue.Body)
			h.auditCompleted(ulid.New(), p, model, upstream, ue.StatusCode, nil, nil, tracing.TraceID(req.Context()), "")
			recordSpanResponse(req, prov.Name(), upstream, nil, false)
			h.metrics.ObserveRequest(ingressName, model, providerName, p.Team, ue.StatusCode, time.Since(start).Seconds(), 0)
			return false
		}
		writeErr(w, 502, "api_error", "upstream error")
		h.auditCompleted(ulid.New(), p, model, upstream, 502, nil, nil, tracing.TraceID(req.Context()), "")
		recordSpanResponse(req, prov.Name(), upstream, nil, false)
		h.metrics.ObserveRequest(ingressName, model, providerName, p.Team, 502, time.Since(start).Seconds(), 0)
		return false
	}
	if !last && (resp.StatusCode >= 500 || resp.StatusCode == 429) {
		return true // upstream 5xx/429 → fall back
	}
	if resp.StatusCode < 400 {
		h.r.RecordResult(providerName, identity, true)
	}
	w.Header().Set("Content-Type", "application/json")
	if providerWire(prov.Name()) == "openai" {
		// openai-wire provider: tee its OpenAI bytes verbatim (lossless, §3.3).
		w.WriteHeader(resp.StatusCode)
		w.Write(resp.RawBody)
	} else {
		// anthropic-wire provider: CONVERT the canonical response → OpenAI shape.
		w.WriteHeader(resp.StatusCode)
		if resp.Parsed != nil {
			w.Write(openai.ResponseFromCanonical(resp.Parsed))
		} else {
			// No parsed canonical (e.g. non-2xx): tee whatever bytes we have.
			w.Write(resp.RawBody)
		}
	}
	var usage *audit.UsageRef
	var cost *audit.CostRef
	if resp.Parsed != nil {
		usage = usageRef(resp.Parsed.Usage)
		cost = h.settle(p, providerName, upstream, resp.Parsed.Usage, table)
		h.observeTokens(model, providerName, p.Team, resp.Parsed.Usage)
	}
	// Body capture (D4, ADR-018): copy-only, AFTER the response was already
	// written to the client above. recID is minted here (not inside
	// auditCompleted) so the body row and the audit record share the same ID.
	recID := ulid.New()
	var bodyRef string
	if h.bodies != nil && resp.StatusCode < 400 {
		bodyRef = h.bodies.Capture(recID, p.Team, pr.RawBody, resp.RawBody)
	}
	h.auditCompleted(recID, p, model, upstream, resp.StatusCode, usage, cost, tracing.TraceID(req.Context()), bodyRef)
	recordSpanResponse(req, prov.Name(), upstream, usage, resp.StatusCode < 400)
	h.metrics.ObserveRequest(ingressName, model, providerName, p.Team, resp.StatusCode, time.Since(start).Seconds(), 0)
	return false
}

// serveStream proxies one streaming target. Fallback is PRE-TTFT ONLY: Stream()
// erroring before any event with a next target available (!last) returns
// retriable=true. Once the first event is rendered the response is committed.
func (h *ChatHandler) serveStream(w http.ResponseWriter, req *http.Request, prov providers.Provider, pr *providers.ProxyRequest, p keystore.Principal, model, providerName, identity, upstream string, last bool, start time.Time, table *pricing.Table) (retriable bool) {
	seq, err := prov.Stream(req.Context(), pr)
	if err != nil {
		if !last {
			return true // pre-TTFT failure → fall back
		}
		var ue *providers.UpstreamError
		if errors.As(err, &ue) {
			if w.Header().Get("Content-Type") == "" {
				w.Header().Set("Content-Type", "application/json")
			}
			w.WriteHeader(ue.StatusCode)
			w.Write(ue.Body)
			h.auditCompleted(ulid.New(), p, model, upstream, ue.StatusCode, nil, nil, tracing.TraceID(req.Context()), "")
			recordSpanResponse(req, prov.Name(), upstream, nil, false)
			h.metrics.ObserveRequest(ingressName, model, providerName, p.Team, ue.StatusCode, time.Since(start).Seconds(), 0)
			return false
		}
		writeErr(w, 502, "api_error", "upstream stream error")
		h.auditCompleted(ulid.New(), p, model, upstream, 502, nil, nil, tracing.TraceID(req.Context()), "")
		recordSpanResponse(req, prov.Name(), upstream, nil, false)
		h.metrics.ObserveRequest(ingressName, model, providerName, p.Team, 502, time.Since(start).Seconds(), 0)
		return false
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "api_error", "streaming unsupported")
		h.auditCompleted(ulid.New(), p, model, upstream, 500, nil, nil, tracing.TraceID(req.Context()), "")
		recordSpanResponse(req, prov.Name(), upstream, nil, false)
		h.metrics.ObserveRequest(ingressName, model, providerName, p.Team, 500, time.Since(start).Seconds(), 0)
		return false
	}
	// Stream() succeeded → the target is healthy (breaker success, post-TTFT).
	h.r.RecordResult(providerName, identity, true)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)

	openaiWire := providerWire(prov.Name()) == "openai"
	var st openai.StreamState
	var usage *audit.UsageRef
	var lastUsage *schema.Usage
	var ttft float64
	for ev, err := range seq {
		if err != nil {
			// upstream broke mid-stream; client sees a truncated stream.
			h.auditCompletedPartial(p, model, upstream, usage, tracing.TraceID(req.Context()))
			h.metrics.ObserveRequest(ingressName, model, providerName, p.Team, 200, time.Since(start).Seconds(), ttft)
			return false
		}
		if ttft == 0 {
			ttft = time.Since(start).Seconds() // first streamed event = time to first token
		}
		if openaiWire {
			// openai-wire provider: tee the upstream OpenAI SSE bytes verbatim
			// (already includes the terminal [DONE]).
			w.Write(ev.Raw)
		} else if ev.Chunk != nil {
			// anthropic-wire provider: re-serialize the canonical chunk into an
			// OpenAI chat.completion.chunk. nil → event with no OpenAI equivalent.
			if chunk := openai.ChunkFromCanonical(ev.Chunk, &st); chunk != nil {
				w.Write([]byte("data: "))
				w.Write(chunk)
				w.Write([]byte("\n\n"))
			}
		}
		flusher.Flush()
		if ev.Chunk != nil && ev.Chunk.Usage != nil {
			usage = usageRef(ev.Chunk.Usage)
			lastUsage = ev.Chunk.Usage
		}
	}
	if !openaiWire {
		// We rendered the OpenAI stream ourselves, so append the terminal marker.
		w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	}
	cost := h.settle(p, providerName, upstream, lastUsage, table)
	h.observeTokens(model, providerName, p.Team, lastUsage)
	// Body capture (D4, ADR-018): REQUEST ONLY for streams (no buffered
	// response bytes exist to capture — see messages.go's serveStream).
	recID := ulid.New()
	var bodyRef string
	if h.bodies != nil {
		bodyRef = h.bodies.Capture(recID, p.Team, pr.RawBody, nil)
	}
	h.auditCompleted(recID, p, model, upstream, 200, usage, cost, tracing.TraceID(req.Context()), bodyRef)
	recordSpanResponse(req, prov.Name(), upstream, usage, true)
	h.metrics.ObserveRequest(ingressName, model, providerName, p.Team, 200, time.Since(start).Seconds(), ttft)
	return false
}

// govErrType maps a governance deny status to the OpenAI error `type`.
func govErrType(status int) string {
	switch status {
	case 429:
		return "rate_limit_exceeded"
	case 402:
		return "insufficient_quota"
	default:
		return "api_error"
	}
}

// settle maps observed schema.Usage to pricing.Usage and runs the Governor's
// post-call settlement (quota debit + cost + budget debit), returning the audit
// CostRef. nil when governance is disabled or there is no usage. The cost key is
// (providerName, upstream-model) to match the pricing table.
func (h *ChatHandler) settle(p keystore.Principal, providerName, upstream string, u *schema.Usage, table *pricing.Table) *audit.CostRef {
	if h.gov == nil || u == nil {
		return nil
	}
	pu := pricing.Usage{
		Input:        deref(u.InputTokens),
		Output:       deref(u.OutputTokens),
		CacheRead:    deref(u.CacheReadInputTokens),
		CacheWrite5m: deref(u.CacheCreationInputTokens),
	}
	cost, missing := h.gov.Settle(p.Team, p.KeyID, keyPolicyOf(p), providerName, upstream, pu, table)
	return &audit.CostRef{
		AmountUSDMicros: cost,
		PricingMissing:  missing,
		PricingVersion:  governance.PricingVersionOf(table),
	}
}

// keyPolicyOf maps a Principal's optional per-key budget/TPM/RPM (§8 D2) to
// the governance package's KeyPolicy; governance stays a leaf and does not
// import keystore.
func keyPolicyOf(p keystore.Principal) governance.KeyPolicy {
	return governance.KeyPolicy{RatePerMin: p.RPM, TokensPerMinute: p.TPM, BudgetMicrosPerMonth: p.BudgetUSDMicros}
}

// observeTokens records the per-type token usage counters for one settled
// request, mirroring settle()'s cache_creation → cache_write_5m mapping. The
// provider arg is the CONFIG provider name (pricing/metrics key). No-op when
// usage is nil or metrics is nil.
func (h *ChatHandler) observeTokens(model, provider, team string, u *schema.Usage) {
	if u == nil {
		return
	}
	h.metrics.ObserveTokenUsage("input", model, provider, team, deref(u.InputTokens))
	h.metrics.ObserveTokenUsage("output", model, provider, team, deref(u.OutputTokens))
	h.metrics.ObserveTokenUsage("cache_read", model, provider, team, deref(u.CacheReadInputTokens))
	h.metrics.ObserveTokenUsage("cache_write_5m", model, provider, team, deref(u.CacheCreationInputTokens))
}

// estimateTokens is the conservative input-token estimate fed to the governance
// pre-check. ~4 bytes per token over the raw request body.
func estimateTokens(raw []byte) int64 {
	n := int64(len(raw) / 4)
	if n < 1 {
		n = 1
	}
	return n
}

// recordSpanResponse sets response-side GenAI attributes + terminal status on the
// request span (ADR-011). ok=false ONLY on a terminal (non-retriable) outcome.
func recordSpanResponse(req *http.Request, system, upstream string, usage *audit.UsageRef, ok bool) {
	span := trace.SpanFromContext(req.Context())
	var in, out int64
	if usage != nil {
		in, out = usage.InputTokens, usage.OutputTokens
	}
	tracing.SetGenAIResponse(span, system, upstream, in, out)
	tracing.SetStatus(span, ok, "")
}

func (h *ChatHandler) audit(p keystore.Principal, model, upstream string, outcome *audit.OutcomeRef, traceID string) {
	if h.aud == nil {
		return
	}
	rec := audit.Record{
		SchemaVersion: 1,
		Event:         "request_started",
		ID:            ulid.New(),
		TS:            time.Now().UTC().Format(time.RFC3339Nano),
		Principal:     audit.PrincipalRef{KeyID: p.KeyID, Team: p.Team},
		Request:       audit.RequestRef{Ingress: "openai", ModelRequested: model, ModelResolved: upstream},
		Outcome:       outcome,
	}
	if traceID != "" {
		rec.TraceID = &traceID
	}
	h.aud.Append(rec)
}

// auditCompleted emits a request_completed record. id is the record's ULID —
// minted by the caller so a body capture (D4, ADR-018) tagged with this exact
// ID can happen BEFORE the record is built. bodyRef is "" when body logging
// is off or nothing was captured. No-op without an audit writer.
func (h *ChatHandler) auditCompleted(id string, p keystore.Principal, model, upstream string, status int, usage *audit.UsageRef, cost *audit.CostRef, traceID, bodyRef string) {
	if h.aud == nil {
		return
	}
	rec := audit.Record{
		SchemaVersion: 1,
		Event:         "request_completed",
		ID:            id,
		TS:            time.Now().UTC().Format(time.RFC3339Nano),
		Principal:     audit.PrincipalRef{KeyID: p.KeyID, Team: p.Team},
		Request:       audit.RequestRef{Ingress: "openai", ModelRequested: model, ModelResolved: upstream},
		Outcome:       &audit.OutcomeRef{Status: status},
		Usage:         usage,
		Cost:          cost,
	}
	if traceID != "" {
		rec.TraceID = &traceID
	}
	if bodyRef != "" {
		rec.BodyRef = &bodyRef
	}
	h.aud.Append(rec)
}

func (h *ChatHandler) auditCompletedPartial(p keystore.Principal, model, upstream string, usage *audit.UsageRef, traceID string) {
	if h.aud == nil {
		return
	}
	rec := audit.Record{
		SchemaVersion: 1,
		Event:         "request_completed",
		ID:            ulid.New(),
		TS:            time.Now().UTC().Format(time.RFC3339Nano),
		Principal:     audit.PrincipalRef{KeyID: p.KeyID, Team: p.Team},
		Request:       audit.RequestRef{Ingress: "openai", ModelRequested: model, ModelResolved: upstream},
		Outcome:       &audit.OutcomeRef{Status: 200, Partial: true},
		Usage:         usage,
	}
	if traceID != "" {
		rec.TraceID = &traceID
	}
	h.aud.Append(rec)
}

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

// writeErr renders an OpenAI-shaped error envelope.
func writeErr(w http.ResponseWriter, status int, errType, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    errType,
			"code":    nil,
		},
	})
}
