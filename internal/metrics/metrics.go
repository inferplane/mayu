// Package metrics owns the Prometheus registry and exposes thin hook functions
// the rest of inferplane calls. Metric names follow OpenTelemetry GenAI semantic
// conventions (gen_ai.*) rendered in Prometheus form (gen_ai_*). Cardinality
// guard: callers must pass only config-declared team/model values, never raw
// request input. The budget_spend metric is an observability approximation —
// the settlement source of truth is the µUSD budget store, not this gauge.
package metrics

import (
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
)

type Metrics struct {
	reg *prometheus.Registry

	tokenUsage      *prometheus.CounterVec   // gen_ai_client_token_usage_total
	requestDuration *prometheus.HistogramVec // gen_ai_server_request_duration_seconds
	ttft            *prometheus.HistogramVec // gen_ai_server_time_to_first_token_seconds
	requestsTotal   *prometheus.CounterVec   // inferplane_requests_total
	fallbackTotal   *prometheus.CounterVec   // inferplane_fallback_total
	circuitState    *prometheus.GaugeVec     // inferplane_circuit_state
	quotaUtil       *prometheus.GaugeVec     // inferplane_quota_utilization_ratio
	budgetUtil      *prometheus.GaugeVec     // inferplane_budget_utilization_ratio
	budgetSpend     *prometheus.CounterVec   // inferplane_budget_spend_usd_total
	pricingMiss     *prometheus.CounterVec   // inferplane_pricing_miss_total
	auditFailures   *prometheus.CounterVec   // inferplane_audit_write_failures_total
	auditBufferUtil prometheus.Gauge         // inferplane_audit_buffer_utilization_ratio
	piiMask         *prometheus.CounterVec   // inferplane_pii_mask_redactions_total
	anchorFail      prometheus.Counter       // inferplane_audit_anchor_failures_total
	usageDropped    prometheus.Counter       // inferplane_usage_windows_dropped_total
	// budgetRejected is a Counter, matching the _total naming convention every
	// other cumulative metric in this file follows (anchorFail, usageDropped):
	// rate() works on it, unlike on a Gauge with the same suffix. The caller
	// (governance.Settle) only has the budget store's own cumulative total to
	// report, not a delta, so SetBudgetStoreRejections tracks the last value it
	// saw (budgetRejectedSeen) and Adds only the increase.
	budgetRejected     prometheus.Counter     // inferplane_budget_store_rejected_total
	budgetRejectedSeen int64                  // last value passed to SetBudgetStoreRejections; atomic
	substitution       *prometheus.CounterVec // inferplane_model_substitution_total (ADR-041)
	routingDecisions   *prometheus.CounterVec // inferplane_routing_decisions_total
}

func New() *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		reg: reg,
		routingDecisions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "inferplane_routing_decisions_total", Help: "Request routing decisions by bounded mode and reason.",
		}, []string{"team", "mode", "reason"}),
		tokenUsage: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gen_ai_client_token_usage_total",
			Help: "Tokens used, by type (input|output|cache_read|cache_write_5m|cache_write_1h).",
		}, []string{"type", "model", "provider", "team"}),
		requestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "gen_ai_server_request_duration_seconds",
			Help:    "End-to-end request duration.",
			Buckets: prometheus.DefBuckets,
		}, []string{"model", "provider", "ingress", "status"}),
		ttft: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "gen_ai_server_time_to_first_token_seconds",
			Help:    "Time to first streamed token.",
			Buckets: prometheus.DefBuckets,
		}, []string{"model", "provider"}),
		requestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "inferplane_requests_total", Help: "Total requests.",
		}, []string{"ingress", "model", "provider", "team", "status"}),
		fallbackTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "inferplane_fallback_total", Help: "Provider fallbacks.",
		}, []string{"model", "from_provider", "to_provider", "reason"}),
		circuitState: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "inferplane_circuit_state", Help: "Circuit breaker state (0=closed,1=half,2=open).",
		}, []string{"provider"}),
		quotaUtil: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "inferplane_quota_utilization_ratio", Help: "Quota utilization 0..1.",
		}, []string{"team", "window"}),
		budgetUtil: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "inferplane_budget_utilization_ratio", Help: "Monthly budget utilization ratio (>1.0 = over budget, D5b/ADR-017).",
		}, []string{"team"}),
		budgetSpend: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "inferplane_budget_spend_usd_total", Help: "Approximate spend in USD (observability only; settlement truth is the µUSD store).",
		}, []string{"team", "model", "cost_type"}),
		pricingMiss: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "inferplane_pricing_miss_total", Help: "Requests with no pricing rate for (provider,model).",
		}, []string{"provider", "model"}),
		auditFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "inferplane_audit_write_failures_total", Help: "Audit sink write failures.",
		}, []string{"sink"}),
		auditBufferUtil: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "inferplane_audit_buffer_utilization_ratio", Help: "Audit WAL buffer utilization 0..1.",
		}),
		piiMask: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "inferplane_pii_mask_redactions_total", Help: "PII redactions applied to request text (ADR-009).",
		}, []string{"team"}),
		usageDropped: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "inferplane_usage_windows_dropped_total",
			Help: "Usage-telemetry windows dropped on pusher buffer overflow or permanent rejection (never silent; no team/key labels).",
		}),
		anchorFail: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "inferplane_audit_anchor_failures_total", Help: "Audit chain-head anchor (WORM) write failures (ADR-012).",
		}),
		budgetRejected: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "inferplane_budget_store_rejected_total",
			Help: "Cumulative requests denied by the budget store's at-capacity fail-safe, not by a real budget (no team/key/user label — same cardinality bar as key_id)." +
				" Non-zero means the in-memory budget store hit its entry cap and started fail-closing new counters.",
		}),
		substitution: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "inferplane_model_substitution_total",
			Help: "ADR-041 budget-tier model substitutions applied at ingress.",
			// Cardinality guard, same rule as fallbackTotal above: every
			// label here comes from a GovernancePolicy document's own
			// config-declared values (team, from/to model names) — never
			// raw, unvalidated client input.
		}, []string{"team", "from_model", "to_model"}),
	}
	reg.MustRegister(m.tokenUsage, m.requestDuration, m.ttft, m.requestsTotal,
		m.fallbackTotal, m.circuitState, m.quotaUtil, m.budgetUtil, m.budgetSpend, m.pricingMiss,
		m.auditFailures, m.auditBufferUtil, m.piiMask, m.anchorFail, m.usageDropped, m.budgetRejected, m.substitution, m.routingDecisions)
	// Prometheus only emits a labeled metric family once it has at least one
	// observed child series. Pre-initialize the token-usage family to zero so
	// gen_ai_client_token_usage_total is always present in exposition (stable
	// dashboards / scrape contracts) even before the first token is recorded.
	m.tokenUsage.WithLabelValues("input", "", "", "")
	return m
}

// ObserveRoutingDecision accepts only bounded enum values. Team comes from the
// authenticated principal (the same trusted team dimension as existing metrics).
// No model, policy name, detected value, user or key enters these labels.
func (m *Metrics) ObserveRoutingDecision(team, mode, reason string) {
	if m == nil {
		return
	}
	switch mode {
	case "Shadow", "Enforce":
	default:
		mode = "none"
	}
	switch reason {
	case "unchanged", "internal_only", "context_inspection_failed", "context_conflict",
		"context_shadow", "context_ineligible", "context_unchanged", "context_unavailable",
		"context_selected", "policy_lookup_failed", "model_forbidden", "inspection_failed",
		"sensitive_blocked", "no_safe_route", "context_affinity", "mask_failed",
		"budget_target", "budget_target_unavailable", "legacy_mask":
	default:
		reason = "unknown"
	}
	m.routingDecisions.WithLabelValues(team, mode, reason).Inc()
}

// Registry exposes the registry for the /metrics handler.
func (m *Metrics) Registry() *prometheus.Registry {
	if m == nil {
		return nil
	}
	return m.reg
}

func (m *Metrics) ObserveTokenUsage(typ, model, provider, team string, tokens int64) {
	if m == nil || tokens <= 0 {
		return
	}
	m.tokenUsage.WithLabelValues(typ, model, provider, team).Add(float64(tokens))
}

// ObserveRequest records one completed request: counter + duration, and TTFT if >0.
func (m *Metrics) ObserveRequest(ingress, model, provider, team string, status int, durationSec, ttftSec float64) {
	if m == nil {
		return
	}
	st := statusClass(status)
	m.requestsTotal.WithLabelValues(ingress, model, provider, team, st).Inc()
	m.requestDuration.WithLabelValues(model, provider, ingress, st).Observe(durationSec)
	if ttftSec > 0 {
		m.ttft.WithLabelValues(model, provider).Observe(ttftSec)
	}
}

// ObservePIIMask records redactions applied to a team's request text. Only the
// (bounded) team label + a count — never any redacted value (ADR-009).
func (m *Metrics) ObservePIIMask(team string, redactions int) {
	if m == nil || redactions <= 0 {
		return
	}
	m.piiMask.WithLabelValues(team).Add(float64(redactions))
}

// IncUsageWindowDropped counts a usage-telemetry window dropped by the
// pusher (buffer overflow or a permanently-rejected batch) — surfacing loss
// instead of silence. Deliberately unlabeled: window contents span teams.
func (m *Metrics) IncUsageWindowDropped() {
	if m == nil {
		return
	}
	m.usageDropped.Inc()
}

// IncAnchorFailure counts a failed audit-anchor write (ADR-012).
func (m *Metrics) IncAnchorFailure() {
	if m == nil {
		return
	}
	m.anchorFail.Inc()
}

func (m *Metrics) ObserveFallback(model, from, to, reason string) {
	if m == nil {
		return
	}
	m.fallbackTotal.WithLabelValues(model, from, to, reason).Inc()
}
func (m *Metrics) ObserveModelSubstitution(team, from, to string) {
	if m == nil {
		return
	}
	m.substitution.WithLabelValues(team, from, to).Inc()
}
func (m *Metrics) SetCircuitState(provider string, state int) {
	if m == nil {
		return
	}
	m.circuitState.WithLabelValues(provider).Set(float64(state))
}
func (m *Metrics) SetQuotaUtilization(team, window string, ratio float64) {
	if m == nil {
		return
	}
	m.quotaUtil.WithLabelValues(team, window).Set(ratio)
}
func (m *Metrics) SetBudgetUtilization(team string, ratio float64) {
	if m == nil {
		return
	}
	m.budgetUtil.WithLabelValues(team).Set(ratio)
}
func (m *Metrics) AddBudgetSpend(team, model, costType string, usd float64) {
	if m == nil {
		return
	}
	m.budgetSpend.WithLabelValues(team, model, costType).Add(usd)
}
func (m *Metrics) IncPricingMiss(provider, model string) {
	if m == nil {
		return
	}
	m.pricingMiss.WithLabelValues(provider, model).Inc()
}
func (m *Metrics) IncAuditFailure(sink string) {
	if m == nil {
		return
	}
	m.auditFailures.WithLabelValues(sink).Inc()
}

// SetBudgetStoreRejections reports the budget store's own cumulative
// rejection total (budget.Memory.Rejections()), NOT a delta — Settle calls
// this on every request with a fresh snapshot. A CAS loop on
// budgetRejectedSeen (rather than a plain load-then-store) is what keeps
// concurrent Settle calls from double-crediting or dropping an increment:
// each successful swap advances the "last seen" value by exactly the amount
// it Adds, so the sum of every successful Add across any number of
// concurrent callers telescopes to n_final − 0, matching the store's real
// total regardless of interleaving.
func (m *Metrics) SetBudgetStoreRejections(n int64) {
	if m == nil {
		return
	}
	for {
		old := atomic.LoadInt64(&m.budgetRejectedSeen)
		if n <= old {
			return
		}
		if atomic.CompareAndSwapInt64(&m.budgetRejectedSeen, old, n) {
			m.budgetRejected.Add(float64(n - old))
			return
		}
	}
}
func (m *Metrics) SetAuditBufferUtilization(r float64) {
	if m == nil {
		return
	}
	m.auditBufferUtil.Set(r)
}

func statusClass(status int) string {
	switch {
	case status >= 200 && status < 300:
		return "2xx"
	case status >= 400 && status < 500:
		return "4xx"
	case status >= 500:
		return "5xx"
	default:
		return "other"
	}
}
