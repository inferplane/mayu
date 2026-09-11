// Package anthropicapi implements the Anthropic-shaped ingress endpoints.
package anthropicapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/inferplane/inferplane/internal/audit"
	"github.com/inferplane/inferplane/internal/bodystore"
	"github.com/inferplane/inferplane/internal/filter"
	"github.com/inferplane/inferplane/internal/governance"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/metrics"
	"github.com/inferplane/inferplane/internal/pricing"
	"github.com/inferplane/inferplane/internal/principal"
	"github.com/inferplane/inferplane/internal/router"
	"github.com/inferplane/inferplane/internal/telemetry"
	"github.com/inferplane/inferplane/internal/tracing"
	"github.com/inferplane/inferplane/pkg/schema"
	"github.com/inferplane/inferplane/pkg/ulid"
	"github.com/inferplane/inferplane/providers"
	"go.opentelemetry.io/otel/trace"
)

const (
	ingressName      = "anthropic"
	maxModelsInError = 20
)

// rejectedModelLabel is the bounded sentinel used as the Prometheus `model`
// label on pre-resolution rejections (403 allow-list deny / 404 unknown model).
// At those points the model string is still attacker-controlled and has NOT been
// validated against config; recording it raw would let a client mint unbounded
// metric series (a cardinality DoS, §6.2 — team/model labels must come from
// config-declared values only). The requested model is still kept in the audit
// record, which is not a Prometheus label and carries no cardinality concern.
const rejectedModelLabel = "_rejected"

type MessagesHandler struct {
	r          *router.Router
	aud        *audit.Writer                                 // nil-safe: unit tests may omit
	gov        *governance.Governor                          // nil-safe: governance disabled when nil
	metrics    *metrics.Metrics                              // nil-safe: no-op when nil
	mask       *filter.Masking                               // nil-safe: masking off when nil (ADR-009)
	teamPolicy func(team string) (keystore.TeamRecord, bool) // nil-safe: no per-team overrides when nil (D6/D7, ADR-016 fresh-read pattern)
	bodies     *bodystore.Recorder                           // nil-safe: body capture off when nil (D4, ADR-018)
	usage      *telemetry.Collector                          // nil-safe: usage telemetry off when nil (control-plane mode)
}

// SetMasking enables the PII masking filter for the configured teams (ADR-009).
// nil-safe: leaving it unset keeps the verbatim fast path with zero overhead.
func (h *MessagesHandler) SetMasking(m *filter.Masking) { h.mask = m }

// SetTeamPolicy installs a fresh-per-request team-record lookup (same
// ADR-016 posture as Governor.SetTeamLookup — no caching, no hot-reload
// trigger) used for per-team overrides that live on the team record but are
// NOT governance: today, the D6/ADR-019 guardrail override. D7/ADR-020's
// region-lock reuses this same setter rather than adding a second one.
func (h *MessagesHandler) SetTeamPolicy(fn func(team string) (keystore.TeamRecord, bool)) {
	h.teamPolicy = fn
}

// SetBodyRecorder enables opt-in request/response body capture (D4, ADR-018).
// nil-safe: leaving it unset keeps the zero-overhead fast path.
func (h *MessagesHandler) SetBodyRecorder(r *bodystore.Recorder) { h.bodies = r }

// SetUsageCollector enables per-request usage telemetry (control-plane mode):
// every settle folds (team, owner, upstream model, usage, cost) into the
// collector's current window. nil-safe: unset keeps standalone behavior
// byte-identical.
func (h *MessagesHandler) SetUsageCollector(c *telemetry.Collector) { h.usage = c }

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

func (h *MessagesHandler) availableModelsErrorSuffix(p keystore.Principal) string {
	names := h.r.AllModels()
	available := make([]string, 0, len(names))
	for _, name := range names {
		if h.r.Allows(p, name) {
			available = append(available, name)
		}
	}
	sort.Strings(available)
	if len(available) == 0 {
		return ". No models available for this key."
	}
	if len(available) > maxModelsInError {
		available = append(available[:maxModelsInError], "...")
	}
	return ". Available models: " + strings.Join(available, ", ")
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
	model, modelSubstituted := h.r.ResolveModel(parsed.Model)
	if modelSubstituted {
		// Unconfigured/unrouted model substituted for a configured one
		// (model_fallbacks or the same-family default, live.State.FallbackFor)
		// BEFORE the allow-list check, so RBAC/audit/metrics/pricing below all
		// key off the model actually served — advertise it to the client.
		w.Header().Set("x-inferplane-model-fallback", model)
	}
	// Tracing (ADR-011): join the client's trace (W3C traceparent), start ONE
	// server span owned across the whole request (incl. the fallback loop) and
	// end it exactly once via defer; no-op when tracing is off. The provider
	// system / response model / usage / terminal status are set later in the
	// serve methods via the span in the request context.
	tctx := tracing.Extract(req.Context(), req.Header)
	tctx, span := tracing.Start(tctx, "chat "+model)
	defer span.End()
	req = req.WithContext(tctx)
	tracing.SetGenAIRequest(span, model)
	traceID := tracing.TraceID(tctx)
	// M3 enforcement: require an authenticated principal and check the
	// per-key model allow-list BEFORE resolving/forwarding (§3.1, §5.1).
	p, ok := principal.From(req.Context())
	if !ok {
		tracing.SetStatus(span, false, "no principal")
		writeErr(w, 401, "authentication_error", "no principal")
		return
	}
	// ADR-041: budget-tier substitution runs AFTER model_fallbacks resolution
	// but BEFORE the RBAC check below, so Allows/audit/metrics/pricing all key
	// off the model actually served — same posture as the model_fallbacks
	// substitution above. SubstituteTier itself enforces "never widen, never
	// deny": it only fires when the ORIGINAL is already allowed and the
	// TARGET also passes RBAC, so the Allows check below is never bypassed,
	// only ever re-run against a (still fully RBAC'd) different model.
	if served, tierSubstituted := h.r.SubstituteTier(p, model); tierSubstituted {
		h.metrics.ObserveModelSubstitution(p.Team, model, served)
		req = req.WithContext(audit.WithSubstitutedFrom(req.Context(), model))
		model = served
		tracing.SetGenAIRequest(span, model)
		w.Header().Set("x-inferplane-substituted-model", model)
	}
	if !h.r.Allows(p, model) {
		// A deny is recorded as a started record carrying the 403 outcome.
		h.audit(req.Context(), p, model, "", &audit.OutcomeRef{Status: 403, Error: audit.DenyModelNotAllowed.Ptr()}, false, traceID)
		// Pre-resolution reject: model is still attacker-controlled → sentinel label.
		h.metrics.ObserveRequest(ingressName, rejectedModelLabel, "", p.Team, 403, time.Since(start).Seconds(), 0)
		tracing.SetStatus(span, false, "model not allowed")
		writeErr(w, 403, "permission_error", "model not allowed for this key: "+model+h.availableModelsErrorSuffix(p))
		return
	}
	chain, st, err := h.r.ResolveChain(model)
	if err != nil {
		// Unknown model is recorded as a started record carrying the 404 outcome,
		// for consistency with the 403 allow-list deny above.
		h.audit(req.Context(), p, model, "", &audit.OutcomeRef{Status: 404}, false, traceID)
		// Pre-resolution reject: model is still attacker-controlled → sentinel label.
		h.metrics.ObserveRequest(ingressName, rejectedModelLabel, "", p.Team, 404, time.Since(start).Seconds(), 0)
		tracing.SetStatus(span, false, "unknown model")
		writeErr(w, 404, "not_found_error", "unknown model: "+model+h.availableModelsErrorSuffix(p))
		return
	}
	// ResolveChain may have appended a cross-model fallback's targets (an
	// upstream "model not found" retry, D5) AFTER the allow-list check above
	// already ran against `model` alone — re-check those targets' model here
	// or a key allowed only `model` would silently reach the fallback model.
	chain = router.FilterModelAllowed(chain, func(m string) bool { return h.r.Allows(p, m) })
	// Team-record fresh lookup (D6/D7, ADR-016 pattern): one call reused below
	// both for the region filter and the guardrail override — a team with no
	// record is indistinguishable from a record with no overrides (zero value).
	var teamRec keystore.TeamRecord
	if h.teamPolicy != nil {
		if rec, ok := h.teamPolicy(p.Team); ok {
			teamRec = rec
		}
	}
	// Per-team region lock (D7, ADR-020): drop targets outside the team's
	// allowed regions BEFORE any billing/masking work. An unlabeled target is
	// always dropped for a restricted team (fail-closed — it cannot prove
	// residency). If every target is filtered out, this is a hard deny, same
	// shape as the allow-list 403 above.
	if len(teamRec.AllowedRegions) > 0 {
		if filtered := router.FilterRegions(chain, teamRec.AllowedRegions); len(filtered) == 0 {
			h.audit(req.Context(), p, model, "", &audit.OutcomeRef{Status: 403, Error: audit.DenyRegionBlocked.Ptr()}, false, traceID)
			h.metrics.ObserveRequest(ingressName, rejectedModelLabel, "", p.Team, 403, time.Since(start).Seconds(), 0)
			tracing.SetStatus(span, false, "region blocked")
			writeErr(w, 403, "permission_error", "no allowed-region target for model: "+model)
			return
		} else {
			chain = filtered
		}
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
			h.audit(req.Context(), p, model, chain[0].Upstream, &audit.OutcomeRef{Status: 400}, false, traceID)
			h.metrics.ObserveRequest(ingressName, model, chain[0].ProviderName, p.Team, 400, time.Since(start).Seconds(), 0)
			tracing.SetStatus(span, false, "pii mask failed")
			writeErr(w, 400, "invalid_request_error", "request could not be PII-masked")
			return
		}
		if n > 0 {
			var reparsed schema.ChatRequest
			if err := json.Unmarshal(masked, &reparsed); err != nil {
				h.audit(req.Context(), p, model, chain[0].Upstream, &audit.OutcomeRef{Status: 400}, false, traceID)
				h.metrics.ObserveRequest(ingressName, model, chain[0].ProviderName, p.Team, 400, time.Since(start).Seconds(), 0)
				tracing.SetStatus(span, false, "pii mask failed")
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
	// Context-window fast-fail: when the operator declared the model's window
	// (config models.<name>.context_window) and even the COARSE byte estimate
	// already exceeds it, refuse here with a message that names both numbers —
	// the upstream's own rejection reaches the client as an opaque scrubbed
	// ValidationException (providers/bedrock/errors.go never echoes upstream
	// text). Estimate-only, no max_tokens added: the estimate is an upper-ish
	// bound and adding output budget would false-reject borderline VALID
	// requests; anything borderline falls through to the upstream's exact
	// check. Runs before PreCheck so a doomed request never charges the TPM
	// estimate.
	if win := h.r.ContextWindow(model); win > 0 {
		if est := estimateTokens(raw); est > win {
			msg := fmt.Sprintf("request is ~%d tokens but model %s has a %d-token context window — reduce the input (or raise models.%s.context_window if the declaration is wrong)", est, model, win, model)
			h.audit(req.Context(), p, model, chain[0].Upstream, &audit.OutcomeRef{Status: http.StatusBadRequest}, false, traceID)
			h.metrics.ObserveRequest(ingressName, model, chain[0].ProviderName, p.Team, http.StatusBadRequest, time.Since(start).Seconds(), 0)
			tracing.SetStatus(span, false, "context window exceeded")
			writeErr(w, http.StatusBadRequest, "invalid_request_error", msg)
			return
		}
	}
	// Governance pre-check (rate/quota/budget) BEFORE the upstream call. A block
	// is recorded as a started record carrying the deny status.
	// Pricing guard (ADR-030): with pricing.on_missing "block", refuse a
	// request whose resolved targets have no rate rather than serving it and
	// billing 0. Covers the routes boot validation cannot see (UI-write
	// models, fallback-appended targets). Same table used to settle below.
	// NOT gated on h.gov: on_missing "block" is a pricing setting, and a
	// deployment with governance off would otherwise serve unpriced traffic free.
	if dec := governance.PricingGuard(table, pricedTargets(chain)); !dec.Allowed {
		h.audit(req.Context(), p, model, chain[0].Upstream, &audit.OutcomeRef{Status: dec.Status, Error: dec.Code.Ptr()}, false, traceID)
		h.metrics.ObserveRequest(ingressName, model, chain[0].ProviderName, p.Team, dec.Status, time.Since(start).Seconds(), 0)
		tracing.SetStatus(span, false, "pricing missing")
		writeErr(w, dec.Status, govErrType(dec.Status), dec.Reason)
		return
	}
	if h.gov != nil {
		dec := h.gov.PreCheck(subjectOf(p), keyPolicyOf(p), estimateTokens(raw))
		if !dec.Allowed {
			h.audit(req.Context(), p, model, chain[0].Upstream, &audit.OutcomeRef{Status: dec.Status, Error: dec.Code.Ptr()}, false, traceID)
			h.metrics.ObserveRequest(ingressName, model, chain[0].ProviderName, p.Team, dec.Status, time.Since(start).Seconds(), 0)
			tracing.SetStatus(span, false, "governance deny")
			writeErr(w, dec.Status, govErrType(dec.Status), dec.Reason)
			return
		}
	}
	// request_started: the request passed auth + allow-list + governance and
	// resolved a target (the first in the priority chain).
	h.audit(req.Context(), p, model, chain[0].Upstream, nil, piiMasked, traceID)
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
			Model: ct.Model, Upstream: ct.Upstream, Parsed: &parsed,
			RawBody: raw, Headers: upHeaders, Stream: stream,
			IngressProtocol:  "anthropic",
			GuardrailID:      teamRec.GuardrailID,
			GuardrailVersion: teamRec.GuardrailVersion,
		}
		last := i == len(chain)-1
		// crossModelNext: the NEXT target (if any) serves a different model
		// than this one — a D5 model-level fallback boundary, not just a
		// different provider for the same model. Only across that boundary
		// does an upstream "model not found" become retriable.
		crossModelNext := !last && chain[i+1].Model != ct.Model
		if i > 0 {
			// We fell back to this target; advertise it to the client.
			w.Header().Set("x-inferplane-fallback", ct.ProviderName)
			if ct.Model != model {
				w.Header().Set("x-inferplane-model-fallback", ct.Model)
			}
		}
		var retriable bool
		if stream {
			retriable = h.serveStream(w, req, ct.Provider, pr, p, ct.Model, ct.ProviderName, ct.Identity, ct.Upstream, last, crossModelNext, start, table)
		} else {
			retriable = h.serveComplete(w, req, ct.Provider, pr, p, ct.Model, ct.ProviderName, ct.Identity, ct.Upstream, last, crossModelNext, start, table)
		}
		if !retriable {
			return // committed (success, or terminal error on the last target)
		}
		// Pre-TTFT failure with a next target available → record + fall back.
		h.r.RecordResult(ct.ProviderName, ct.Identity, false)
		reason := "upstream_error"
		if crossModelNext {
			reason = "model_not_found"
		}
		h.metrics.ObserveFallback(ct.Model, ct.ProviderName, chain[i+1].ProviderName, reason)
	}
}

// isModelNotFound reports whether an upstream response looks like a
// "model not found" rejection rather than an unrelated client error — a plain
// 404, or a 400 whose body names a Bedrock ValidationException (Bedrock
// returns 400, not 404, for a model not enabled/available in that region).
// Deliberately narrow: only these are ever retried across a D5 model-level
// fallback boundary; any other 4xx stays a client error, teed verbatim.
func isModelNotFound(statusCode int, body []byte) bool {
	if statusCode == 404 {
		return true
	}
	return statusCode == 400 && bytes.Contains(body, []byte("ValidationException"))
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
// when the call failed pre-TTFT (transport error, an upstream 5xx/429, or —
// only when crossModelNext — a "model not found" response, D5) AND a next
// target exists (!last) — the caller then falls back. Otherwise it writes the
// response/error to the client and returns false (committed).
func (h *MessagesHandler) serveComplete(w http.ResponseWriter, req *http.Request, prov providers.Provider, pr *providers.ProxyRequest, p keystore.Principal, model, providerName, identity, upstream string, last, crossModelNext bool, start time.Time, table *pricing.Table) (retriable bool) {
	resp, err := prov.Complete(req.Context(), pr)
	if err != nil {
		if !last {
			return true // transport error → fall back
		}
		// Tee a non-2xx upstream error verbatim (status/body) so the client
		// sees the real rate-limit/error response, not a fabricated one —
		// mirrors serveStream's tee below (parity fix: bedrock's non-streaming
		// path can return an UpstreamError just like its streaming path does).
		var ue *providers.UpstreamError
		if errors.As(err, &ue) {
			copyUpstreamHeaders(w.Header(), ue.Header)
			if w.Header().Get("Content-Type") == "" {
				w.Header().Set("Content-Type", "application/json")
			}
			st := ue.HTTPStatus()
			w.WriteHeader(st)
			w.Write(ue.Body)
			h.auditCompleted(req.Context(), ulid.New(), p, model, upstream, st, nil, nil, tracing.TraceID(req.Context()), "", pr.GuardrailID, pr.GuardrailVersion)
			recordSpanResponse(req, prov.Name(), upstream, nil, false) // terminal
			h.metrics.ObserveRequest(ingressName, model, providerName, p.Team, st, time.Since(start).Seconds(), 0)
			return false
		}
		writeErr(w, 502, "api_error", "upstream error")
		h.auditCompleted(req.Context(), ulid.New(), p, model, upstream, 502, nil, nil, tracing.TraceID(req.Context()), "", pr.GuardrailID, pr.GuardrailVersion)
		recordSpanResponse(req, prov.Name(), upstream, nil, false) // terminal
		h.metrics.ObserveRequest(ingressName, model, providerName, p.Team, 502, time.Since(start).Seconds(), 0)
		return false
	}
	// An upstream 5xx/429 is a retriable failure when a next target exists.
	// Crossing a model-level fallback boundary (D5) also retries a "model not
	// found" response — narrow on purpose: a plain 400 unrelated to the
	// requested model must stay a client error, never replayed elsewhere.
	if !last && (resp.StatusCode >= 500 || resp.StatusCode == 429 || (crossModelNext && isModelNotFound(resp.StatusCode, resp.RawBody))) {
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
		cost = h.settle(p, providerName, model, upstream, resp.Parsed.Usage, table, estimateTokens(pr.RawBody))
		h.observeTokens(model, providerName, p.Team, resp.Parsed.Usage)
	}
	// Body capture (D4, ADR-018): copy-only, AFTER the response was already
	// written to the client above — provably off the response path. recID is
	// minted here (not inside auditCompleted) so the body row and the audit
	// record it's tagged with share the exact same ID.
	recID := ulid.New()
	var bodyRef string
	if h.bodies != nil && resp.StatusCode < 400 {
		bodyRef = h.bodies.Capture(recID, p.Team, pr.RawBody, resp.RawBody)
	}
	h.auditCompleted(req.Context(), recID, p, model, upstream, resp.StatusCode, usage, cost, tracing.TraceID(req.Context()), bodyRef, pr.GuardrailID, pr.GuardrailVersion)
	recordSpanSettled(req, prov.Name(), upstream, usage, cost, resp.StatusCode < 400, false)
	h.metrics.ObserveRequest(ingressName, model, providerName, p.Team, resp.StatusCode, time.Since(start).Seconds(), 0)
	return false
}

// serveStream proxies one streaming target. Fallback is PRE-TTFT ONLY: if
// Stream() returns an error before any event is yielded AND a next target exists
// (!last), it returns retriable=true and the caller falls back — this already
// covers a cross-model "model not found" (D5, crossModelNext) since the
// anthropic provider's Stream() wraps ANY non-2xx pre-TTFT response as an
// error, unlike Complete()'s status-in-response path (see serveComplete),
// so crossModelNext needs no extra gating here; it is accepted only for
// parity with serveComplete's signature. Once the first event is teed the
// response is committed; a mid-stream error terminates the stream (no
// fallback). Returns false in all committed cases.
func (h *MessagesHandler) serveStream(w http.ResponseWriter, req *http.Request, prov providers.Provider, pr *providers.ProxyRequest, p keystore.Principal, model, providerName, identity, upstream string, last, crossModelNext bool, start time.Time, table *pricing.Table) (retriable bool) {
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
			st := ue.HTTPStatus()
			w.WriteHeader(st)
			w.Write(ue.Body)
			h.auditCompleted(req.Context(), ulid.New(), p, model, upstream, st, nil, nil, tracing.TraceID(req.Context()), "", pr.GuardrailID, pr.GuardrailVersion)
			recordSpanResponse(req, prov.Name(), upstream, nil, false) // terminal
			h.metrics.ObserveRequest(ingressName, model, providerName, p.Team, st, time.Since(start).Seconds(), 0)
			return false
		}
		writeErr(w, 502, "api_error", "upstream stream error")
		h.auditCompleted(req.Context(), ulid.New(), p, model, upstream, 502, nil, nil, tracing.TraceID(req.Context()), "", pr.GuardrailID, pr.GuardrailVersion)
		recordSpanResponse(req, prov.Name(), upstream, nil, false) // terminal
		h.metrics.ObserveRequest(ingressName, model, providerName, p.Team, 502, time.Since(start).Seconds(), 0)
		return false
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "api_error", "streaming unsupported")
		h.auditCompleted(req.Context(), ulid.New(), p, model, upstream, 500, nil, nil, tracing.TraceID(req.Context()), "", pr.GuardrailID, pr.GuardrailVersion)
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
			// upstream broke mid-stream: the 200 is already committed, so the
			// failure surfaces as an SSE error event rather than a silently
			// truncated stream (H4 gate finding). The error detail is logged
			// server-side only — an AWS SDK error string can carry an account
			// id/ARN (same principle providers/bedrock/errors.go applies) — and
			// never echoed to the client.
			log.Printf("anthropicapi: stream interrupted (model=%s upstream=%s provider=%s): %v", model, upstream, providerName, err)
			writeStreamErrorEvent(w)
			flusher.Flush()
			// Tokens already delivered to the client are real infrastructure
			// cost — bill them (ADR-030). Before this, a stream that broke
			// mid-flight skipped settle() entirely and everything already
			// streamed was free, with no pricing_missing flag to show it.
			partialCost := h.settle(p, providerName, model, upstream, lastUsage, table, estimateTokens(pr.RawBody))
			// …and count them, on the same usage settle() just billed. Metering
			// only the clean path left gen_ai_client_token_usage_total below the
			// budget spend, the audit ledger, and the usage windows for every
			// interrupted stream.
			h.observeTokens(model, providerName, p.Team, lastUsage)
			h.auditCompletedPartial(req.Context(), p, model, upstream, usage, partialCost, tracing.TraceID(req.Context()))
			recordSpanSettled(req, prov.Name(), upstream, usage, partialCost, false, true) // committed (partial)
			h.metrics.ObserveRequest(ingressName, model, providerName, p.Team, 200, time.Since(start).Seconds(), ttft)
			return false
		}
		if ttft == 0 {
			ttft = time.Since(start).Seconds() // first streamed event = time to first token
		}
		w.Write(ev.Raw) // tee original bytes verbatim
		flusher.Flush()
		// Settlement observation: FOLD every usage-bearing frame, never
		// overwrite (ADR-030). Anthropic splits the counts across two frames —
		// message_start carries input + cache_read + cache_creation nested
		// under message.usage, while message_delta commonly carries
		// output_tokens alone. Reading only the top-level usage of the last
		// frame billed streaming requests for output tokens only.
		if ev.Chunk != nil {
			if ev.Chunk.Message != nil && ev.Chunk.Message.Usage != nil {
				lastUsage = schema.MergeUsage(lastUsage, ev.Chunk.Message.Usage)
			}
			if ev.Chunk.Usage != nil {
				lastUsage = schema.MergeUsage(lastUsage, ev.Chunk.Usage)
			}
			usage = usageRef(lastUsage)
		}
	}
	cost := h.settle(p, providerName, model, upstream, lastUsage, table, estimateTokens(pr.RawBody))
	h.observeTokens(model, providerName, p.Team, lastUsage)
	// Body capture (D4, ADR-018): REQUEST ONLY for streams — a streaming
	// response exists only as per-event ev.Raw, never buffered as a whole
	// (buffering would break the streaming memory posture), so there is no
	// resp bytes to capture here (§4.7's stated streaming limitation).
	recID := ulid.New()
	var bodyRef string
	if h.bodies != nil {
		bodyRef = h.bodies.Capture(recID, p.Team, pr.RawBody, nil)
	}
	h.auditCompleted(req.Context(), recID, p, model, upstream, 200, usage, cost, tracing.TraceID(req.Context()), bodyRef, pr.GuardrailID, pr.GuardrailVersion)
	recordSpanSettled(req, prov.Name(), upstream, usage, cost, true, false) // committed stream success
	h.metrics.ObserveRequest(ingressName, model, providerName, p.Team, 200, time.Since(start).Seconds(), ttft)
	return false
}

// settle maps the observed schema.Usage to pricing.Usage and runs the
// Governor's post-call settlement (quota debit + cost + budget debit), returning
// the audit CostRef. nil when governance is disabled or there is no usage.
//
// Cache writes are TTL-tiered (1h costs 2x the input rate, 5m costs 1.25x), so
// the two tiers are resolved separately via schema.Usage.CacheWriteTiers rather
// than collapsed into the cheaper one (ADR-030 — the collapse under-billed 1h
// writes by ~40%).
func (h *MessagesHandler) settle(p keystore.Principal, providerName, model, upstream string, u *schema.Usage, table *pricing.Table, estimatedTokens int64) *audit.CostRef {
	if h.gov == nil || u == nil {
		return nil
	}
	write5m, write1h := u.CacheWriteTiers()
	pu := pricing.Usage{
		Input:        deref(u.InputTokens),
		Output:       deref(u.OutputTokens),
		CacheRead:    deref(u.CacheReadInputTokens),
		CacheWrite5m: write5m,
		CacheWrite1h: write1h,
	}
	cost, missing := h.gov.Settle(subjectOf(p), keyPolicyOf(p), providerName, upstream, pu, table, estimatedTokens)
	if h.usage != nil {
		// Attribute to the UPSTREAM model — the name pricing billed.
		h.usage.Record(p.Team, p.Owner, upstream, pu, cost)
	}
	return &audit.CostRef{
		AmountUSDMicros: cost,
		PricingMissing:  missing,
		PricingVersion:  governance.PricingVersionOf(table),
	}
}

// subjectOf maps a Principal to the governance package's Subject: the team, the
// virtual key, and the individual the key was issued to. Owner is what carries
// per-user budget enforcement (ADR-042 Phase 3) — governance skips the user
// lookup entirely when Subject.User is empty, so dropping it here would
// silently disable the feature rather than fail. Same
// deliberately-duplicated-per-package shape as keyPolicyOf below, and for the
// same reason: governance stays a leaf and does not import keystore.
func subjectOf(p keystore.Principal) governance.Subject {
	return governance.Subject{Team: p.Team, KeyID: p.KeyID, User: p.Owner}
}

// keyPolicyOf maps a Principal's optional per-key budget/TPM/RPM (§8 D2) to
// the governance package's KeyPolicy; governance stays a leaf and does not
// import keystore.
func keyPolicyOf(p keystore.Principal) governance.KeyPolicy {
	return governance.KeyPolicy{RatePerMin: p.RPM, TokensPerMinute: p.TPM, BudgetMicrosPerMonth: p.BudgetUSDMicros, BudgetMicrosPerDay: p.BudgetUSDMicrosPerDay}
}

// observeTokens records the per-type token usage counters for one settled
// request. Mirrors the settle() mapping, including the cache-write TTL split
// (ADR-030) so the metrics and the billed amount can't disagree. The provider
// arg is the CONFIG provider name (pricing/metrics key), matching the request
// metric labels. No-op when usage is nil or metrics is nil.
func (h *MessagesHandler) observeTokens(model, provider, team string, u *schema.Usage) {
	if u == nil {
		return
	}
	write5m, write1h := u.CacheWriteTiers()
	h.metrics.ObserveTokenUsage("input", model, provider, team, deref(u.InputTokens))
	h.metrics.ObserveTokenUsage("output", model, provider, team, deref(u.OutputTokens))
	h.metrics.ObserveTokenUsage("cache_read", model, provider, team, deref(u.CacheReadInputTokens))
	h.metrics.ObserveTokenUsage("cache_write_5m", model, provider, team, write5m)
	h.metrics.ObserveTokenUsage("cache_write_1h", model, provider, team, write1h)
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
func (h *MessagesHandler) audit(ctx context.Context, p keystore.Principal, model, upstream string, outcome *audit.OutcomeRef, piiMasked bool, traceID string) {
	if h.aud == nil {
		return
	}
	rec := audit.Record{
		SchemaVersion: 1,
		Event:         "request_started",
		ID:            ulid.New(),
		TS:            time.Now().UTC().Format(time.RFC3339Nano),
		Principal:     audit.PrincipalRef{KeyID: p.KeyID, Team: p.Team},
		Request:       audit.RequestRef{Ingress: "anthropic", ModelRequested: model, ModelResolved: upstream, PIIMasked: piiMasked, ModelSubstitutedFrom: audit.SubstitutedFrom(ctx)},
		Outcome:       outcome,
	}
	if traceID != "" {
		rec.TraceID = &traceID
	}
	h.aud.Append(rec)
}

// auditCompleted emits a request_completed record with the final status and
// observed usage. id is the record's ULID — minted by the caller (rather than
// here) so a body capture (D4, ADR-018), which must be tagged with this exact
// ID, can happen BEFORE the record is built. bodyRef is "" when body logging
// is off or nothing was captured for this request. No-op without an audit
// writer.
func (h *MessagesHandler) auditCompleted(ctx context.Context, id string, p keystore.Principal, model, upstream string, status int, usage *audit.UsageRef, cost *audit.CostRef, traceID, bodyRef, guardrailID, guardrailVersion string) {
	if h.aud == nil {
		return
	}
	rec := audit.Record{
		SchemaVersion: 1,
		Event:         "request_completed",
		ID:            id,
		TS:            time.Now().UTC().Format(time.RFC3339Nano),
		Principal:     audit.PrincipalRef{KeyID: p.KeyID, Team: p.Team},
		Request:       audit.RequestRef{Ingress: "anthropic", ModelRequested: model, ModelResolved: upstream, ModelSubstitutedFrom: audit.SubstitutedFrom(ctx)},
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
	if guardrailID != "" {
		rec.GuardrailID = &guardrailID
	}
	if guardrailVersion != "" {
		rec.GuardrailVersion = &guardrailVersion
	}
	h.aud.Append(rec)
}

// auditCompletedPartial records a stream that broke mid-flight: status 200 was
// already sent to the client, but the response is partial.
func (h *MessagesHandler) auditCompletedPartial(ctx context.Context, p keystore.Principal, model, upstream string, usage *audit.UsageRef, cost *audit.CostRef, traceID string) {
	if h.aud == nil {
		return
	}
	rec := audit.Record{
		SchemaVersion: 1,
		Event:         "request_completed",
		ID:            ulid.New(),
		TS:            time.Now().UTC().Format(time.RFC3339Nano),
		Principal:     audit.PrincipalRef{KeyID: p.KeyID, Team: p.Team},
		Request:       audit.RequestRef{Ingress: "anthropic", ModelRequested: model, ModelResolved: upstream, ModelSubstitutedFrom: audit.SubstitutedFrom(ctx)},
		Outcome:       &audit.OutcomeRef{Status: 200, Partial: true},
		Usage:         usage,
		Cost:          cost,
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

// partialSpanDesc is the fixed span-status description for a stream the upstream
// truncated. Fixed on purpose: the underlying error string can carry an account
// id/ARN (same principle as the client-facing error frame), and a span export
// leaves the process.
const partialSpanDesc = "upstream stream interrupted"

// recordSpanSettled is recordSpanResponse for a response that reached
// settlement: it adds the cache tiers and the settled µUSD cost, and marks a
// truncated stream both partial and errored. The wire status stays 200 for a
// partial (it was committed before the break), so the span is the only place a
// trace consumer can see the truncation. Callers with no settlement (a terminal
// pre-commit failure) keep using recordSpanResponse.
func recordSpanSettled(req *http.Request, system, upstream string, usage *audit.UsageRef, cost *audit.CostRef, ok, partial bool) {
	span := trace.SpanFromContext(req.Context())
	var in, out, cacheRead, write5m, write1h int64
	if usage != nil {
		in, out = usage.InputTokens, usage.OutputTokens
		cacheRead = usage.CacheReadInputTokens
		write5m, write1h = usage.CacheCreation5mInputTokens, usage.CacheCreation1hInputTokens
	}
	tracing.SetGenAIResponse(span, system, upstream, in, out)
	tracing.SetUsageDetail(span, cacheRead, write5m, write1h)
	if cost != nil {
		tracing.SetCost(span, cost.AmountUSDMicros, cost.PricingMissing)
	}
	if partial {
		tracing.SetPartial(span)
		tracing.SetStatus(span, false, partialSpanDesc)
		return
	}
	tracing.SetStatus(span, ok, "")
}

// usageRef maps an observed schema.Usage to the audit UsageRef, dereferencing
// the *int64 token fields nil-safe (a missing upstream key counts as 0).
//
// Cache writes are recorded BOTH ways, from the same CacheWriteTiers resolution
// settle() bills from: the flat field carries the total (what existing readers
// consume) and the two tier fields carry the 1.25x/2x split. Reading only
// u.CacheCreationInputTokens wrote a zero whenever the upstream sent the tiers
// under cache_creation and omitted the flat total — which is Anthropic's
// standard shape, so the audit record showed no cache write on a request that
// was billed for one.
func usageRef(u *schema.Usage) *audit.UsageRef {
	if u == nil {
		return nil
	}
	write5m, write1h := u.CacheWriteTiers()
	return &audit.UsageRef{
		InputTokens:                deref(u.InputTokens),
		OutputTokens:               deref(u.OutputTokens),
		CacheReadInputTokens:       deref(u.CacheReadInputTokens),
		CacheCreationInputTokens:   write5m + write1h,
		CacheCreation5mInputTokens: write5m,
		CacheCreation1hInputTokens: write1h,
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

// writeStreamErrorEvent emits an Anthropic SSE `error` event on a stream whose
// 200 status is already committed (mid-stream upstream failure) — mirroring
// bedrockapi's exception-frame partialFinish path so a client sees a real
// error instead of a silently truncated stream. The message is a fixed,
// generic string; the real error goes to the server log only (see caller).
func writeStreamErrorEvent(w http.ResponseWriter) {
	io.WriteString(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"upstream stream interrupted\"}}\n\n")
}

// pricedTargets projects the resolved chain onto the (provider, upstream) pairs
// the request could be billed against — the same key pricing.CostUSDMicros
// uses — so the pricing guard checks exactly what settlement will look up.
func pricedTargets(chain []router.ChainTarget) []governance.PricedTarget {
	out := make([]governance.PricedTarget, 0, len(chain))
	for _, ct := range chain {
		out = append(out, governance.PricedTarget{Provider: ct.ProviderName, Upstream: ct.Upstream})
	}
	return out
}
