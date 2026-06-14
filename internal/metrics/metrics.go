// Package metrics owns the Prometheus registry and exposes thin hook functions
// the rest of inferplane calls. Metric names follow OpenTelemetry GenAI semantic
// conventions (gen_ai.*) rendered in Prometheus form (gen_ai_*). Cardinality
// guard: callers must pass only config-declared team/model values, never raw
// request input. The budget_spend metric is an observability approximation —
// the settlement source of truth is the µUSD budget store, not this gauge.
package metrics

import "github.com/prometheus/client_golang/prometheus"

type Metrics struct {
	reg *prometheus.Registry

	tokenUsage      *prometheus.CounterVec   // gen_ai_client_token_usage_total
	requestDuration *prometheus.HistogramVec // gen_ai_server_request_duration_seconds
	ttft            *prometheus.HistogramVec // gen_ai_server_time_to_first_token_seconds
	requestsTotal   *prometheus.CounterVec   // inferplane_requests_total
	fallbackTotal   *prometheus.CounterVec   // inferplane_fallback_total
	circuitState    *prometheus.GaugeVec     // inferplane_circuit_state
	quotaUtil       *prometheus.GaugeVec     // inferplane_quota_utilization_ratio
	budgetSpend     *prometheus.CounterVec   // inferplane_budget_spend_usd_total
	pricingMiss     *prometheus.CounterVec   // inferplane_pricing_miss_total
	auditFailures   *prometheus.CounterVec   // inferplane_audit_write_failures_total
	auditBufferUtil prometheus.Gauge         // inferplane_audit_buffer_utilization_ratio
	piiMask         *prometheus.CounterVec   // inferplane_pii_mask_redactions_total
}

func New() *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		reg: reg,
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
	}
	reg.MustRegister(m.tokenUsage, m.requestDuration, m.ttft, m.requestsTotal,
		m.fallbackTotal, m.circuitState, m.quotaUtil, m.budgetSpend, m.pricingMiss,
		m.auditFailures, m.auditBufferUtil, m.piiMask)
	// Prometheus only emits a labeled metric family once it has at least one
	// observed child series. Pre-initialize the token-usage family to zero so
	// gen_ai_client_token_usage_total is always present in exposition (stable
	// dashboards / scrape contracts) even before the first token is recorded.
	m.tokenUsage.WithLabelValues("input", "", "", "")
	return m
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

func (m *Metrics) ObserveFallback(model, from, to, reason string) {
	if m == nil {
		return
	}
	m.fallbackTotal.WithLabelValues(model, from, to, reason).Inc()
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
