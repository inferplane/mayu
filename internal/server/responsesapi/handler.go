// Package responsesapi serves Codex-compatible Responses requests through the
// same policy, identity and accounting pipeline as the other generation APIs.
package responsesapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"time"

	"github.com/inferplane/inferplane/internal/audit"
	"github.com/inferplane/inferplane/internal/bodystore"
	"github.com/inferplane/inferplane/internal/filter"
	"github.com/inferplane/inferplane/internal/governance"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/live"
	"github.com/inferplane/inferplane/internal/metrics"
	"github.com/inferplane/inferplane/internal/principal"
	"github.com/inferplane/inferplane/internal/responses"
	"github.com/inferplane/inferplane/internal/router"
	"github.com/inferplane/inferplane/internal/sensitivity"
	"github.com/inferplane/inferplane/internal/server/requestpolicy"
	"github.com/inferplane/inferplane/internal/telemetry"
	"github.com/inferplane/inferplane/internal/tracing"
	"github.com/inferplane/inferplane/providers"
)

type Handler struct {
	r          *router.Router
	aud        *audit.Writer
	gov        *governance.Governor
	metrics    *metrics.Metrics
	mask       *filter.Masking
	teamPolicy func(string) (keystore.TeamRecord, bool)
	bodies     *bodystore.Recorder
	usage      *telemetry.Collector
}

func NewHandler(r *router.Router, aud *audit.Writer, gov *governance.Governor, m *metrics.Metrics) *Handler {
	return &Handler{r: r, aud: aud, gov: gov, metrics: m}
}
func (h *Handler) SetMasking(m *filter.Masking)             { h.mask = m }
func (h *Handler) SetBodyRecorder(r *bodystore.Recorder)    { h.bodies = r }
func (h *Handler) SetUsageCollector(c *telemetry.Collector) { h.usage = c }
func (h *Handler) SetTeamPolicy(f func(string) (keystore.TeamRecord, bool)) {
	h.teamPolicy = f
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	p, ok := principal.From(req.Context())
	if !ok {
		writeError(w, 401, "authentication_error", "authentication required")
		return
	}
	start := time.Now()
	ctx, span := tracing.Start(req.Context(), "responses")
	defer span.End()
	req = req.WithContext(ctx)
	raw, err := io.ReadAll(req.Body)
	if err != nil {
		status := http.StatusBadRequest
		var oversized *http.MaxBytesError
		if errors.As(err, &oversized) {
			status = http.StatusRequestEntityTooLarge
		}
		writeError(w, status, "invalid_request_error", "could not read request body")
		return
	}
	parsed, err := responses.RequestToCanonical(raw)
	if err != nil {
		writeError(w, 400, "invalid_request_error", "invalid Responses request")
		return
	}
	model, _ := h.r.ResolveModel(parsed.Model)
	if !h.r.Allows(p, model) {
		h.denied(req, p, "_rejected", 403, "model_forbidden", start)
		writeError(w, 403, "permission_error", "model is not allowed")
		return
	}
	requested := model
	if selected, changed := h.r.SubstituteTier(p, model); changed {
		req = req.WithContext(audit.WithSubstitutedFrom(req.Context(), model))
		h.metrics.ObserveModelSubstitution(p.Team, model, selected)
		model = selected
		w.Header().Set("x-inferplane-substituted-model", model)
	}
	chain, st, err := h.r.ResolveChain(model)
	if err != nil {
		h.denied(req, p, "_rejected", 404, "model_unavailable", start)
		writeError(w, 404, "invalid_request_error", "model is unavailable")
		return
	}
	var team keystore.TeamRecord
	if h.teamPolicy != nil {
		team, _ = h.teamPolicy(p.Team)
	}
	chain = router.FilterModelAllowed(chain, func(m string) bool { return h.r.Allows(p, m) })
	if len(team.AllowedRegions) > 0 {
		chain = router.FilterRegions(chain, team.AllowedRegions)
	}
	// Apply at every candidate selection, including private recovery and
	// context alternatives. Observation parsing is not conversion permission.
	compatible := func(ct router.ChainTarget) bool { return compatibleTarget(raw, ct, st) }
	result, routeErr := h.r.RouteRequest(req.Context(), router.RequestRoutingInput{
		Principal: p, Protocol: "responses", RawBody: raw, RequestedModel: requested,
		Model: model, Chain: chain, State: st, AllowedRegions: team.AllowedRegions,
		SessionHint: requestpolicy.SessionHint(req), Compatible: compatible,
		Redactor: requestpolicy.CombinedRedactor(h.mask, p.Team),
	})
	if routeErr != nil {
		req = requestpolicy.Observe(w, req, result, h.metrics)
		status, reason := 403, result.Decision.Reason
		if !requestpolicy.Active(result.Decision) && len(chain) > 0 && !slices.ContainsFunc(chain, compatible) {
			status, reason = 400, "unsupported_conversion"
		}
		h.denied(req, p, model, status, reason, start)
		writeError(w, status, "invalid_request_error", "no policy-permitted compatible Responses target")
		return
	}
	model, chain, st = result.Model, result.Chain, result.State
	if result.MaskRequired {
		parsed, err = responses.RequestToCanonical(result.SanitizedBody)
		if err != nil || len(result.SanitizedBody) == 0 {
			h.denied(req, p, model, 403, "masking_failed", start)
			writeError(w, 403, "permission_error", "request could not be completely masked")
			return
		}
		raw = result.SanitizedBody
	}
	if h.mask.Enabled(p.Team) && !result.Decision.Masked {
		sanitized, maskErr := sensitivity.NewRedactorWithMasker(h.mask.Filter).Redact(req.Context(), "responses", raw)
		if maskErr != nil {
			result.Decision.Reason = "mask_failed"
			req = requestpolicy.Observe(w, req, result, h.metrics)
			h.denied(req, p, model, 403, "mask_failed", start)
			writeError(w, 403, "permission_error", "request could not be completely masked")
			return
		}
		parsed, err = responses.RequestToCanonical(sanitized)
		if err != nil {
			writeError(w, 403, "permission_error", "request could not be completely masked")
			return
		}
		result.Decision.Masked = !bytes.Equal(raw, sanitized)
		if result.Decision.Masked && len(result.Decision.Policies) == 0 {
			result.Decision.Reason = "legacy_mask"
		}
		raw = sanitized
	}
	req = requestpolicy.Observe(w, req, result, h.metrics)
	// A defensive final check protects native-only fields even on a legacy
	// no-policy chain, and prevents accidentally adding a lossy adapter.
	for _, ct := range chain {
		if !compatibleTarget(raw, ct, st) {
			h.denied(req, p, model, 400, "unsupported_conversion", start)
			writeError(w, 400, "invalid_request_error", "request requires a native Responses target")
			return
		}
	}
	tracing.SetGenAIRequest(span, model)
	estimate := max(int64(1), int64(len(raw)/4))
	if limit := requestpolicy.ContextWindow(st, model); limit > 0 && estimate > limit {
		h.denied(req, p, model, 400, "context_window_exceeded", start)
		writeError(w, 400, "invalid_request_error", "request exceeds the configured context window")
		return
	}
	table := st.Pricing()
	priced := make([]governance.PricedTarget, 0, len(chain))
	for _, ct := range chain {
		priced = append(priced, governance.PricedTarget{Provider: ct.ProviderName, Upstream: ct.Upstream})
	}
	if dec := governance.PricingGuard(table, priced); !dec.Allowed {
		h.denied(req, p, model, dec.Status, string(dec.Code), start)
		writeError(w, dec.Status, "insufficient_quota", dec.Reason)
		return
	}
	if h.gov != nil {
		if dec := h.gov.PreCheck(subject(p), keyPolicy(p), estimate); !dec.Allowed {
			h.denied(req, p, model, dec.Status, string(dec.Code), start)
			writeError(w, dec.Status, "insufficient_quota", dec.Reason)
			return
		}
	}
	stream := parsed.Stream != nil && *parsed.Stream
	for i, ct := range chain {
		if !h.r.BudgetTargetAllowed(p, requested, ct.Model, st) {
			w.Header().Set("x-inferplane-routing-reason", "budget_target_changed")
			w.Header().Del("x-inferplane-routed-model")
			h.denied(req, p, model, 402, "budget_target_changed", start)
			writeError(w, 402, "insufficient_quota", "budget routing changed; retry to select an eligible model")
			return
		}
		if i > 0 && h.gov != nil {
			if dec := h.gov.PreCheck(subject(p), keyPolicy(p), estimate); !dec.Allowed {
				h.denied(req, p, model, dec.Status, string(dec.Code), start)
				writeError(w, dec.Status, "insufficient_quota", dec.Reason)
				return
			}
		}
		headers := req.Header.Clone()
		tracing.Inject(req.Context(), headers)
		pr := &providers.ProxyRequest{
			Model: ct.Model, Upstream: ct.Upstream, Parsed: parsed, RawBody: raw,
			Headers: headers, Stream: stream, IngressProtocol: "responses",
			GuardrailID: team.GuardrailID, GuardrailVersion: team.GuardrailVersion,
		}
		if ct.Provider.Name() == "anthropic" {
			if pr.Headers.Get("Anthropic-Version") == "" {
				pr.Headers.Set("Anthropic-Version", "2023-06-01")
			}
			pr.RawBody, err = anthropicRequest(parsed)
			if err != nil {
				h.denied(req, p, model, 400, "unsupported_conversion", start)
				writeError(w, 400, "invalid_request_error", "request cannot be represented on the selected provider")
				return
			}
		}
		if i > 0 {
			w.Header().Set("x-inferplane-fallback", ct.ProviderName)
			if ct.Model != model {
				w.Header().Set("x-inferplane-model-fallback", ct.Model)
			}
		}
		reservedReq, finishBudget, reserveErr := requestpolicy.ReserveBudget(req, h.gov, p, ct, st, raw)
		if reserveErr != nil {
			status := requestpolicy.BudgetStatus(reserveErr)
			w.Header().Set("Retry-After", "1")
			h.denied(req, p, model, status, "budget_authority_unavailable", start)
			writeError(w, status, "insufficient_quota", "budget authority unavailable")
			return
		}
		defer finishBudget()
		attemptReq := reservedReq.WithContext(audit.WithRoutingAttempt(reservedReq.Context(), ct.Model, ct.ProviderName, ct.DataBoundary))
		attemptReq = requestpolicy.WithAffinityAttempt(attemptReq, h.r, result.AffinityToken, ct)
		a := attempt{h: h, req: attemptReq, principal: p, target: ct, proxy: pr, table: table, started: start, estimate: estimate, inputBody: raw}
		h.started(a)
		last := i == len(chain)-1
		retry := false
		if stream {
			retry = a.stream(w, last)
		} else {
			retry = a.complete(w, last)
		}
		finishBudget()
		if !retry {
			return
		}
		h.r.RecordResult(ct.ProviderName, ct.Identity, false)
		h.metrics.ObserveFallback(ct.Model, ct.ProviderName, chain[i+1].ProviderName, "upstream_error")
	}
}

func compatibleTarget(raw []byte, ct router.ChainTarget, st *live.State) bool {
	if ct.Provider == nil {
		return false
	}
	if ct.Provider.Name() == "openai_responses" {
		return true
	}
	if responses.ValidateConversion(raw) != nil {
		return false
	}
	strict, err := responses.StrictTools(raw)
	if err != nil {
		return false
	}
	if strict {
		// The native wire retains strictness directly. A foreign wire needs
		// explicit structured-output capability; Anthropic strict-tool
		// transport is not part of this adapter's proven contract.
		if ct.Provider.Name() == "anthropic" || st == nil {
			return false
		}
		model, ok := st.Route(ct.Model)
		if !ok || !slices.Contains(model.Capabilities, "structured_output") {
			return false
		}
	}
	switch ct.Provider.Name() {
	case "openai_compatible", "anthropic":
		return true
	case "bedrock":
		return false // requires a separately proven Responses-to-Bedrock contract
	default:
		supporter, ok := ct.Provider.(router.IngressSupporter)
		return ok && supporter.SupportsIngress("responses")
	}
}

func writeError(w http.ResponseWriter, status int, kind, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
		"type": kind, "message": message, "code": nil, "param": nil,
	}})
}
