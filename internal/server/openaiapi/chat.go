// Package openaiapi implements the OpenAI-shaped ingress endpoints
// (/v1/chat/completions + a content-negotiated /v1/models). It mirrors the
// Anthropic ingress (internal/server/anthropicapi) but speaks the OpenAI wire
// protocol and, when the resolved provider's native wire is NOT OpenAI,
// CONVERTS the canonical (Anthropic-superset) response into OpenAI shape on the
// way out. Lives in its own package so internal/server can import it without an
// import cycle (server → openaiapi is fine; openaiapi must not import server).
package openaiapi

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
	"github.com/inferplane/inferplane/internal/openai"
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
	ingressName      = "openai"
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

type ChatHandler struct {
	r          *router.Router
	aud        *audit.Writer                                 // nil-safe: unit tests may omit
	gov        *governance.Governor                          // nil-safe: governance disabled when nil
	metrics    *metrics.Metrics                              // nil-safe: no-op when nil
	mask       *filter.Masking                               // nil-safe: masking off when nil (ADR-009)
	teamPolicy func(team string) (keystore.TeamRecord, bool) // nil-safe: no per-team overrides when nil (D6/D7, ADR-016 fresh-read pattern)
	bodies     *bodystore.Recorder                           // nil-safe: body capture off when nil (D4, ADR-018)
	usage      *telemetry.Collector                          // nil-safe: usage telemetry off when nil (control-plane mode)
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
// SetUsageCollector enables per-request usage telemetry (control-plane mode);
// nil-safe, mirrors anthropicapi.
func (h *ChatHandler) SetUsageCollector(c *telemetry.Collector) { h.usage = c }

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

func (h *ChatHandler) availableModelsErrorSuffix(p keystore.Principal) string {
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
	model, modelSubstituted := h.r.ResolveModel(canonical.Model)
	if modelSubstituted {
		// Unconfigured/unrouted model substituted for a configured one
		// (model_fallbacks or the same-family default, D5) BEFORE the
		// allow-list check — advertise it to the client.
		w.Header().Set("x-inferplane-model-fallback", model)
	}
	// Tracing (ADR-011): join the client trace, start ONE server span across the
	// request, end once via defer; no-op when off.
	tctx := tracing.Extract(req.Context(), req.Header)
	tctx, span := tracing.Start(tctx, "chat "+model)
	defer span.End()
	req = req.WithContext(tctx)
	tracing.SetGenAIRequest(span, model)
	traceID := tracing.TraceID(tctx)
	// Require an authenticated principal and enforce the per-key model
	// allow-list BEFORE resolving/forwarding (§3.1, §5.1).
	p, ok := principal.From(req.Context())
	if !ok {
		tracing.SetStatus(span, false, "no principal")
		writeErr(w, 401, "authentication_error", "no principal")
		return
	}
	// ADR-041: budget-tier substitution — see anthropicapi.MessagesHandler's
	// identical seam for the full rationale (runs after model_fallbacks,
	// before RBAC; never widens access, never denies).
	if served, tierSubstituted := h.r.SubstituteTier(p, model); tierSubstituted {
		h.metrics.ObserveModelSubstitution(p.Team, model, served)
		req = req.WithContext(audit.WithSubstitutedFrom(req.Context(), model))
		model = served
		tracing.SetGenAIRequest(span, model)
		w.Header().Set("x-inferplane-substituted-model", model)
	}
	// Fail closed for masked teams on the OpenAI ingress (ADR-009 round-2
	// CRITICAL): v1 masks only the Anthropic ingress, so a masked team must not
	// bypass PII masking by using /v1/chat/completions. Reject until OpenAI-ingress
	// masking ships.
	if h.mask.Enabled(p.Team) {
		// Audit the security-critical rejection (a masking-bypass attempt) — a
		// silent reject would be a blind spot in the tamper-evident chain (P4 gate).
		h.audit(req.Context(), p, model, "", &audit.OutcomeRef{Status: 400}, traceID)
		h.metrics.ObserveRequest(ingressName, rejectedModelLabel, "", p.Team, 400, time.Since(start).Seconds(), 0)
		tracing.SetStatus(span, false, "pii mask bypass blocked")
		writeErr(w, 400, "invalid_request_error", "PII masking is enabled for your team but not supported on the OpenAI-compatible endpoint yet; use /v1/messages")
		return
	}
	if !h.r.Allows(p, model) {
		h.audit(req.Context(), p, model, "", &audit.OutcomeRef{Status: 403, Error: audit.DenyModelNotAllowed.Ptr()}, traceID)
		// Pre-resolution reject: model is still attacker-controlled → sentinel label.
		h.metrics.ObserveRequest(ingressName, rejectedModelLabel, "", p.Team, 403, time.Since(start).Seconds(), 0)
		tracing.SetStatus(span, false, "model not allowed")
		writeErr(w, 403, "permission_error", "model not allowed for this key: "+model+h.availableModelsErrorSuffix(p))
		return
	}
	chain, st, err := h.r.ResolveChain(model)
	if err != nil {
		h.audit(req.Context(), p, model, "", &audit.OutcomeRef{Status: 404}, traceID)
		// Pre-resolution reject: model is still attacker-controlled → sentinel label.
		h.metrics.ObserveRequest(ingressName, rejectedModelLabel, "", p.Team, 404, time.Since(start).Seconds(), 0)
		tracing.SetStatus(span, false, "unknown model")
		writeErr(w, 404, "not_found_error", "unknown model: "+model+h.availableModelsErrorSuffix(p))
		return
	}
	// ResolveChain may have appended a cross-model fallback's targets (D5)
	// AFTER the allow-list check above already ran against `model` alone —
	// re-check those targets' model or a key allowed only `model` would
	// silently reach the fallback model.
	chain = router.FilterModelAllowed(chain, func(m string) bool { return h.r.Allows(p, m) })
	// Team-record fresh lookup (D6/D7, ADR-016 pattern): one call reused below
	// both for the region filter and the guardrail override.
	var teamRec keystore.TeamRecord
	if h.teamPolicy != nil {
		if rec, ok := h.teamPolicy(p.Team); ok {
			teamRec = rec
		}
	}
	// Per-team region lock (D7, ADR-020): drop targets outside the team's
	// allowed regions BEFORE governance/billing. An unlabeled target is always
	// dropped for a restricted team (fail-closed). Empty result → hard deny.
	if len(teamRec.AllowedRegions) > 0 {
		if filtered := router.FilterRegions(chain, teamRec.AllowedRegions); len(filtered) == 0 {
			h.audit(req.Context(), p, model, "", &audit.OutcomeRef{Status: 403, Error: audit.DenyRegionBlocked.Ptr()}, traceID)
			h.metrics.ObserveRequest(ingressName, rejectedModelLabel, "", p.Team, 403, time.Since(start).Seconds(), 0)
			tracing.SetStatus(span, false, "region blocked")
			writeErr(w, 403, "permission_error", "no allowed-region target for model: "+model)
			return
		} else {
			chain = filtered
		}
	}
	// Governance pre-check (rate/quota/budget) BEFORE the upstream call.
	// Pricing table from the SAME generation we resolved on (ADR-006).
	table := st.Pricing()
	// Context-window fast-fail (same rule and rationale as anthropicapi's —
	// estimate-only, before PreCheck, clear message; deliberate per-package
	// duplication like subjectOf).
	if win := h.r.ContextWindow(model); win > 0 {
		if est := estimateTokens(raw); est > win {
			msg := fmt.Sprintf("request is ~%d tokens but model %s has a %d-token context window — reduce the input (or raise models.%s.context_window if the declaration is wrong)", est, model, win, model)
			h.audit(req.Context(), p, model, chain[0].Upstream, &audit.OutcomeRef{Status: http.StatusBadRequest}, traceID)
			h.metrics.ObserveRequest(ingressName, model, chain[0].ProviderName, p.Team, http.StatusBadRequest, time.Since(start).Seconds(), 0)
			tracing.SetStatus(span, false, "context window exceeded")
			writeErr(w, http.StatusBadRequest, "invalid_request_error", msg)
			return
		}
	}
	// Pricing guard (ADR-030): with pricing.on_missing "block", refuse a
	// request whose resolved targets have no rate rather than serving it and
	// billing 0. Covers the routes boot validation cannot see (UI-write
	// models, fallback-appended targets). Same table used to settle below.
	// NOT gated on h.gov: on_missing "block" is a pricing setting, and a
	// deployment with governance off would otherwise serve unpriced traffic free.
	if dec := governance.PricingGuard(table, pricedTargets(chain)); !dec.Allowed {
		h.audit(req.Context(), p, model, chain[0].Upstream, &audit.OutcomeRef{Status: dec.Status, Error: dec.Code.Ptr()}, traceID)
		h.metrics.ObserveRequest(ingressName, model, chain[0].ProviderName, p.Team, dec.Status, time.Since(start).Seconds(), 0)
		tracing.SetStatus(span, false, "pricing missing")
		writeErr(w, dec.Status, govErrType(dec.Status), dec.Reason)
		return
	}
	if h.gov != nil {
		dec := h.gov.PreCheck(subjectOf(p), keyPolicyOf(p), estimateTokens(raw))
		if !dec.Allowed {
			h.audit(req.Context(), p, model, chain[0].Upstream, &audit.OutcomeRef{Status: dec.Status, Error: dec.Code.Ptr()}, traceID)
			h.metrics.ObserveRequest(ingressName, model, chain[0].ProviderName, p.Team, dec.Status, time.Since(start).Seconds(), 0)
			tracing.SetStatus(span, false, "governance deny")
			writeErr(w, dec.Status, govErrType(dec.Status), dec.Reason)
			return
		}
	}
	// request_started: the request passed auth + allow-list + governance and
	// resolved a target (the first in the priority chain).
	h.audit(req.Context(), p, model, chain[0].Upstream, nil, traceID)
	stream := canonical.Stream != nil && *canonical.Stream

	// Priority fallback chain (§4.5): try targets in order. A pre-TTFT failure
	// falls back to the next target, records the breaker result, and sets
	// x-inferplane-fallback. Stream fallback is pre-first-event only.
	for i, ct := range chain {
		upHeaders := req.Header.Clone()
		tracing.Inject(req.Context(), upHeaders)
		pr := &providers.ProxyRequest{
			Model: ct.Model, Upstream: ct.Upstream, Parsed: canonical,
			RawBody: raw, Headers: upHeaders, Stream: stream,
			IngressProtocol:  "openai",
			GuardrailID:      teamRec.GuardrailID,
			GuardrailVersion: teamRec.GuardrailVersion,
		}
		last := i == len(chain)-1
		// crossModelNext: the next target (if any) serves a different model
		// than this one — a D5 model-level fallback boundary, not just a
		// different provider for the same model.
		crossModelNext := !last && chain[i+1].Model != ct.Model
		if i > 0 {
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
			return
		}
		h.r.RecordResult(ct.ProviderName, ct.Identity, false)
		reason := "upstream_error"
		if crossModelNext {
			reason = "model_not_found"
		}
		h.metrics.ObserveFallback(ct.Model, ct.ProviderName, chain[i+1].ProviderName, reason)
	}
}

// serveComplete proxies one non-streaming target. It returns retriable=true on a
// pre-TTFT failure (transport error or upstream 5xx/429) when a next target
// exists (!last); otherwise it writes the response/error and returns false.
func (h *ChatHandler) serveComplete(w http.ResponseWriter, req *http.Request, prov providers.Provider, pr *providers.ProxyRequest, p keystore.Principal, model, providerName, identity, upstream string, last, crossModelNext bool, start time.Time, table *pricing.Table) (retriable bool) {
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
			st := ue.HTTPStatus()
			w.WriteHeader(st)
			w.Write(ue.Body)
			h.auditCompleted(req.Context(), ulid.New(), p, model, upstream, st, nil, nil, tracing.TraceID(req.Context()), "", pr.GuardrailID, pr.GuardrailVersion)
			recordSpanResponse(req, prov.Name(), upstream, nil, false)
			h.metrics.ObserveRequest(ingressName, model, providerName, p.Team, st, time.Since(start).Seconds(), 0)
			return false
		}
		writeErr(w, 502, "api_error", "upstream error")
		h.auditCompleted(req.Context(), ulid.New(), p, model, upstream, 502, nil, nil, tracing.TraceID(req.Context()), "", pr.GuardrailID, pr.GuardrailVersion)
		recordSpanResponse(req, prov.Name(), upstream, nil, false)
		h.metrics.ObserveRequest(ingressName, model, providerName, p.Team, 502, time.Since(start).Seconds(), 0)
		return false
	}
	if !last && (resp.StatusCode >= 500 || resp.StatusCode == 429 || (crossModelNext && isModelNotFound(resp.StatusCode, resp.RawBody))) {
		return true // upstream 5xx/429, or a cross-model "model not found" → fall back
	}
	if resp.StatusCode < 400 {
		h.r.RecordResult(providerName, identity, true)
	}
	w.Header().Set("Content-Type", "application/json")
	var clientBody []byte
	if providerWire(prov.Name()) == "openai" {
		// openai-wire provider: tee its OpenAI bytes verbatim (lossless, §3.3).
		clientBody = resp.RawBody
	} else {
		// anthropic-wire provider: CONVERT the canonical response → OpenAI shape.
		if resp.Parsed != nil {
			clientBody = openai.ResponseFromCanonical(resp.Parsed)
		} else {
			// No parsed canonical (e.g. non-2xx): tee whatever bytes we have.
			clientBody = resp.RawBody
		}
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(clientBody)
	var usage *audit.UsageRef
	var cost *audit.CostRef
	if resp.Parsed != nil {
		usage = usageRef(resp.Parsed.Usage)
		cost = h.settle(p, providerName, upstream, resp.Parsed.Usage, table, estimateTokens(pr.RawBody))
		h.observeTokens(model, providerName, p.Team, resp.Parsed.Usage)
	}
	// Body capture (D4, ADR-018): copy-only, AFTER the response was already
	// written to the client above. recID is minted here (not inside
	// auditCompleted) so the body row and the audit record share the same ID.
	recID := ulid.New()
	var bodyRef string
	if h.bodies != nil && resp.StatusCode < 400 {
		bodyRef = h.bodies.Capture(recID, p.Team, pr.RawBody, clientBody)
	}
	h.auditCompleted(req.Context(), recID, p, model, upstream, resp.StatusCode, usage, cost, tracing.TraceID(req.Context()), bodyRef, pr.GuardrailID, pr.GuardrailVersion)
	recordSpanSettled(req, prov.Name(), upstream, usage, cost, resp.StatusCode < 400, false)
	h.metrics.ObserveRequest(ingressName, model, providerName, p.Team, resp.StatusCode, time.Since(start).Seconds(), 0)
	return false
}

// serveStream proxies one streaming target. Fallback is PRE-TTFT ONLY: Stream()
// erroring before any event with a next target available (!last) returns
// retriable=true. Once the first event is rendered the response is committed.
func (h *ChatHandler) serveStream(w http.ResponseWriter, req *http.Request, prov providers.Provider, pr *providers.ProxyRequest, p keystore.Principal, model, providerName, identity, upstream string, last, crossModelNext bool, start time.Time, table *pricing.Table) (retriable bool) {
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
			st := ue.HTTPStatus()
			w.WriteHeader(st)
			w.Write(ue.Body)
			h.auditCompleted(req.Context(), ulid.New(), p, model, upstream, st, nil, nil, tracing.TraceID(req.Context()), "", pr.GuardrailID, pr.GuardrailVersion)
			recordSpanResponse(req, prov.Name(), upstream, nil, false)
			h.metrics.ObserveRequest(ingressName, model, providerName, p.Team, st, time.Since(start).Seconds(), 0)
			return false
		}
		writeErr(w, 502, "api_error", "upstream stream error")
		h.auditCompleted(req.Context(), ulid.New(), p, model, upstream, 502, nil, nil, tracing.TraceID(req.Context()), "", pr.GuardrailID, pr.GuardrailVersion)
		recordSpanResponse(req, prov.Name(), upstream, nil, false)
		h.metrics.ObserveRequest(ingressName, model, providerName, p.Team, 502, time.Since(start).Seconds(), 0)
		return false
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "api_error", "streaming unsupported")
		h.auditCompleted(req.Context(), ulid.New(), p, model, upstream, 500, nil, nil, tracing.TraceID(req.Context()), "", pr.GuardrailID, pr.GuardrailVersion)
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
	// An anthropic-wire provider's stream is rendered by us, so usage only
	// reaches the client if we put it there — and only when the client's own
	// stream_options.include_usage asked for it.
	st := openai.StreamState{IncludeUsage: openai.StreamWantsUsage(pr.RawBody)}
	var usage *audit.UsageRef
	var lastUsage *schema.Usage
	var ttft float64
	for ev, err := range seq {
		if err != nil {
			// upstream broke mid-stream: the 200 is already committed, so the
			// failure surfaces as an SSE error event + terminal [DONE] rather
			// than a silently truncated stream (mirrors anthropicapi's
			// writeStreamErrorEvent / bedrockapi's exception-frame path). The
			// error detail is logged server-side only, never echoed to the
			// client (an AWS SDK error string can carry an account id/ARN).
			log.Printf("openaiapi: stream interrupted (model=%s upstream=%s provider=%s): %v", model, upstream, providerName, err)
			writeStreamErrorEvent(w)
			flusher.Flush()
			// Tokens already delivered to the client are real infrastructure
			// cost — bill them (ADR-030). Before this, a stream that broke
			// mid-flight skipped settle() entirely and everything already
			// streamed was free, with no pricing_missing flag to show it.
			partialCost := h.settle(p, providerName, upstream, lastUsage, table, estimateTokens(pr.RawBody))
			// …and count them, on the same usage settle() just billed (see
			// messages.go's twin: metering only the clean path left the token
			// counters below the billed spend for every interrupted stream).
			h.observeTokens(model, providerName, p.Team, lastUsage)
			h.auditCompletedPartial(req.Context(), p, model, upstream, usage, partialCost, tracing.TraceID(req.Context()))
			recordSpanSettled(req, prov.Name(), upstream, usage, partialCost, false, true) // committed (partial)
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
		// FOLD every usage-bearing frame rather than overwriting (ADR-030).
		// The canonical stream keeps Anthropic's frame vocabulary, so the
		// input and cache counts arrive on message_start (nested under
		// message.usage) while message_delta commonly carries output alone.
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
	if !openaiWire {
		// We rendered the OpenAI stream ourselves, so append the terminal marker.
		w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	}
	cost := h.settle(p, providerName, upstream, lastUsage, table, estimateTokens(pr.RawBody))
	h.observeTokens(model, providerName, p.Team, lastUsage)
	// Body capture (D4, ADR-018): REQUEST ONLY for streams (no buffered
	// response bytes exist to capture — see messages.go's serveStream).
	recID := ulid.New()
	var bodyRef string
	if h.bodies != nil {
		bodyRef = h.bodies.Capture(recID, p.Team, pr.RawBody, nil)
	}
	h.auditCompleted(req.Context(), recID, p, model, upstream, 200, usage, cost, tracing.TraceID(req.Context()), bodyRef, pr.GuardrailID, pr.GuardrailVersion)
	recordSpanSettled(req, prov.Name(), upstream, usage, cost, true, false)
	h.metrics.ObserveRequest(ingressName, model, providerName, p.Team, 200, time.Since(start).Seconds(), ttft)
	return false
}

// isModelNotFound reports whether an upstream response looks like a "model
// not found" rejection rather than an unrelated client error — a plain 404,
// or a 400 whose body names a Bedrock ValidationException (Bedrock returns
// 400, not 404, for a model not enabled/available in that region).
// Deliberately narrow: only these are ever retried across a D5 model-level
// fallback boundary; any other 4xx stays a client error, teed verbatim.
func isModelNotFound(statusCode int, body []byte) bool {
	if statusCode == 404 {
		return true
	}
	return statusCode == 400 && bytes.Contains(body, []byte("ValidationException"))
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
// (providerName, upstream-model) to match the pricing table. Cache writes are
// TTL-tiered via schema.Usage.CacheWriteTiers (ADR-030).
func (h *ChatHandler) settle(p keystore.Principal, providerName, upstream string, u *schema.Usage, table *pricing.Table, estimatedTokens int64) *audit.CostRef {
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
// request, mirroring settle()'s cache_creation → cache_write_5m mapping. The
// provider arg is the CONFIG provider name (pricing/metrics key). No-op when
// usage is nil or metrics is nil.
func (h *ChatHandler) observeTokens(model, provider, team string, u *schema.Usage) {
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

// partialSpanDesc is the fixed span-status description for an upstream-truncated
// stream — fixed because the underlying error string can carry an account
// id/ARN and a span export leaves the process.
const partialSpanDesc = "upstream stream interrupted"

// recordSpanSettled is recordSpanResponse for a response that reached
// settlement: cache tiers + settled µUSD cost, and a truncated stream marked
// both partial and errored (the wire status stayed 200, so the span is the only
// place a trace consumer sees the truncation). Mirrors messages.go.
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

func (h *ChatHandler) audit(ctx context.Context, p keystore.Principal, model, upstream string, outcome *audit.OutcomeRef, traceID string) {
	if h.aud == nil {
		return
	}
	rec := audit.Record{
		SchemaVersion: 1,
		Event:         "request_started",
		ID:            ulid.New(),
		TS:            time.Now().UTC().Format(time.RFC3339Nano),
		Principal:     audit.PrincipalRef{KeyID: p.KeyID, Team: p.Team},
		Request:       audit.RequestRef{Ingress: "openai", ModelRequested: model, ModelResolved: upstream, ModelSubstitutedFrom: audit.SubstitutedFrom(ctx)},
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
func (h *ChatHandler) auditCompleted(ctx context.Context, id string, p keystore.Principal, model, upstream string, status int, usage *audit.UsageRef, cost *audit.CostRef, traceID, bodyRef, guardrailID, guardrailVersion string) {
	if h.aud == nil {
		return
	}
	rec := audit.Record{
		SchemaVersion: 1,
		Event:         "request_completed",
		ID:            id,
		TS:            time.Now().UTC().Format(time.RFC3339Nano),
		Principal:     audit.PrincipalRef{KeyID: p.KeyID, Team: p.Team},
		Request:       audit.RequestRef{Ingress: "openai", ModelRequested: model, ModelResolved: upstream, ModelSubstitutedFrom: audit.SubstitutedFrom(ctx)},
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

func (h *ChatHandler) auditCompletedPartial(ctx context.Context, p keystore.Principal, model, upstream string, usage *audit.UsageRef, cost *audit.CostRef, traceID string) {
	if h.aud == nil {
		return
	}
	rec := audit.Record{
		SchemaVersion: 1,
		Event:         "request_completed",
		ID:            ulid.New(),
		TS:            time.Now().UTC().Format(time.RFC3339Nano),
		Principal:     audit.PrincipalRef{KeyID: p.KeyID, Team: p.Team},
		Request:       audit.RequestRef{Ingress: "openai", ModelRequested: model, ModelResolved: upstream, ModelSubstitutedFrom: audit.SubstitutedFrom(ctx)},
		Outcome:       &audit.OutcomeRef{Status: 200, Partial: true},
		Usage:         usage,
		Cost:          cost,
	}
	if traceID != "" {
		rec.TraceID = &traceID
	}
	h.aud.Append(rec)
}

// usageRef maps an observed schema.Usage to the audit UsageRef. Cache writes are
// recorded both as the total and as the 1.25x/2x TTL split, from the same
// CacheWriteTiers resolution settle() bills from — see messages.go's twin for
// why reading the flat field alone recorded a zero.
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

// writeStreamErrorEvent emits an OpenAI-shaped SSE error event on a stream
// whose 200 status is already committed (mid-stream upstream failure),
// followed by the terminal [DONE] marker so the client's stream parser sees a
// clean end rather than a silent truncation — mirrors anthropicapi's
// writeStreamErrorEvent. The message is fixed/generic; the real error goes to
// the server log only (see caller).
func writeStreamErrorEvent(w http.ResponseWriter) {
	io.WriteString(w, "data: {\"error\":{\"message\":\"upstream stream interrupted\",\"type\":\"api_error\",\"code\":null}}\n\n")
	io.WriteString(w, "data: [DONE]\n\n")
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
