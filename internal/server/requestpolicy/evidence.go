// Package requestpolicy adapts router decisions to ingress observability.
// Keeping this outside the parent server package avoids parent/child cycles.
package requestpolicy

import (
	"net/http"
	"time"

	"github.com/inferplane/inferplane/internal/audit"
	"github.com/inferplane/inferplane/internal/live"
	"github.com/inferplane/inferplane/internal/metrics"
	"github.com/inferplane/inferplane/internal/principal"
	"github.com/inferplane/inferplane/internal/router"
	"github.com/inferplane/inferplane/pkg/ulid"
)

// Active excludes legacy traffic, preserving its audit bytes and headers.
// A rejected policy lookup is evidence even when no rule could be loaded.
func Active(d router.RoutingDecision) bool {
	return len(d.Policies) > 0 || d.Reason == "policy_lookup_failed" ||
		d.Reason == "budget_target" || d.Reason == "budget_target_unavailable"
}

// Observe copies ONLY safe decision scalars; never marshal result or Chain.
func Observe(w http.ResponseWriter, req *http.Request, result router.RequestRoutingResult, m *metrics.Metrics) *http.Request {
	d := result.Decision
	if !Active(d) {
		return req
	}
	ref := &audit.RoutingRef{
		RequestedModel: d.RequestedModel, SelectedModel: d.SelectedModel, ProposedModel: d.ProposedModel,
		Mode: d.Mode, Reason: d.Reason, Inspection: d.Inspection, Privacy: d.Privacy, Categories: d.Categories,
		Masked: d.Masked,
	}
	for _, p := range d.Policies {
		ref.Policies = append(ref.Policies, audit.RoutingPolicyRef{Name: p.Name, Generation: p.Generation, Rule: p.Rule})
	}
	for _, r := range d.Recommendations {
		ref.Recommendations = append(ref.Recommendations, audit.RoutingRecommendation{
			Policy: audit.RoutingPolicyRef{Name: r.Policy.Name, Generation: r.Policy.Generation, Rule: r.Policy.Rule},
			Model:  r.Model, Reason: r.Reason,
		})
	}
	if len(result.Chain) > 0 {
		ref.PlannedProvider = result.Chain[0].ProviderName
		ref.PlannedBoundary = result.Chain[0].DataBoundary
		if ref.PlannedBoundary == "" {
			ref.PlannedBoundary = "unknown"
		}
	}
	if p, ok := principal.From(req.Context()); ok {
		m.ObserveRoutingDecision(p.Team, d.Mode, d.Reason)
	}
	w.Header().Set("x-inferplane-routing-reason", d.Reason)
	if d.SelectedModel != "" && (d.Privacy == "internal_only" || d.Reason == "context_selected" || d.Reason == "context_affinity" || d.Reason == "budget_target") {
		w.Header().Set("x-inferplane-routed-model", d.SelectedModel)
	}
	return req.WithContext(audit.WithRouting(req.Context(), ref))
}

// ContextWindow reads only the topology captured by the routing decision.
func ContextWindow(st *live.State, model string) int64 {
	if st == nil {
		return 0
	}
	m, _ := st.Route(model)
	return m.ContextWindow
}

// CountRecord emits decision evidence only for policy-aware count requests.
// A local security refusal still completes with 200 and has no actual target.
func CountRecord(w *audit.Writer, req *http.Request, protocol string, completed bool) {
	if w == nil {
		return
	}
	ref := audit.RoutingFrom(req.Context())
	p, ok := principal.From(req.Context())
	if ref == nil || !ok {
		return
	}
	rec := audit.Record{
		SchemaVersion: 1, Event: "request_started", ID: ulid.New(), TS: time.Now().UTC().Format(time.RFC3339Nano),
		Principal: audit.PrincipalRef{KeyID: p.KeyID, Team: p.Team},
		Request:   audit.RequestRef{Ingress: protocol, ModelRequested: ref.RequestedModel, Routing: ref},
	}
	if completed {
		rec.Event = "request_completed"
		rec.Outcome = &audit.OutcomeRef{Status: http.StatusOK}
	}
	w.Append(rec)
}
