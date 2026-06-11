package metrics

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestTokenUsageCounter(t *testing.T) {
	m := New()
	m.ObserveTokenUsage("input", "claude-sonnet-4-6", "anthropic-direct", "platform-eng", 1200)
	m.ObserveTokenUsage("output", "claude-sonnet-4-6", "anthropic-direct", "platform-eng", 850)
	got := testutil.ToFloat64(m.tokenUsage.WithLabelValues("input", "claude-sonnet-4-6", "anthropic-direct", "platform-eng"))
	if got != 1200 {
		t.Fatalf("input token usage = %v, want 1200", got)
	}
}

func TestRequestsTotalAndExposition(t *testing.T) {
	m := New()
	m.ObserveRequest("anthropic", "claude-sonnet-4-6", "anthropic-direct", "platform-eng", 200, 1.5, 0.4)
	// gather and confirm the metric names are present with GenAI naming
	out := gather(t, m)
	for _, want := range []string{
		"gen_ai_client_token_usage_total",
		"gen_ai_server_request_duration_seconds",
		"gen_ai_server_time_to_first_token_seconds",
		"inferplane_requests_total",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("metric %q not exposed in:\n%s", want, out)
		}
	}
}

func TestCircuitStateGauge(t *testing.T) {
	m := New()
	m.SetCircuitState("anthropic-direct", 2) // open
	got := testutil.ToFloat64(m.circuitState.WithLabelValues("anthropic-direct"))
	if got != 2 {
		t.Fatalf("circuit state = %v, want 2", got)
	}
}

func gather(t *testing.T, m *Metrics) string {
	t.Helper()
	mfs, err := m.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	for _, mf := range mfs {
		sb.WriteString(mf.GetName())
		sb.WriteString("\n")
	}
	_ = prometheus.NewRegistry
	return sb.String()
}
