package metrics

import (
	"fmt"
	"strings"
	"testing"
)

func TestRoutingDecisionCardinality(t *testing.T) {
	m := New()
	for i := 0; i < 100; i++ {
		m.ObserveRoutingDecision("configured-team", fmt.Sprintf("person%d@example.test", i), fmt.Sprintf("private-prompt-%d", i))
	}
	m.ObserveRoutingDecision("configured-team", "Shadow", "context_shadow")
	for _, reason := range []string{"context_affinity", "mask_failed", "budget_target", "budget_target_unavailable"} {
		m.ObserveRoutingDecision("configured-team", "Enforce", reason)
	}
	families, err := m.Registry().Gather()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range families {
		if f.GetName() != "inferplane_routing_decisions_total" {
			continue
		}
		found = true
		if len(f.Metric) != 6 {
			t.Fatalf("unbounded reason/mode series: %d", len(f.Metric))
		}
		for _, metric := range f.Metric {
			if len(metric.Label) != 3 {
				t.Fatalf("routing labels = %v", metric.Label)
			}
			for _, label := range metric.Label {
				switch label.GetName() {
				case "team", "mode", "reason":
				default:
					t.Fatalf("unsafe dimension: %s", label.GetName())
				}
				if strings.Contains(label.GetValue(), "@") || strings.Contains(label.GetValue(), "prompt") {
					t.Fatal("raw value entered metric")
				}
			}
		}
	}
	if !found {
		t.Fatal("missing routing counter")
	}
	var disabled *Metrics
	disabled.ObserveRoutingDecision("team", "Enforce", "context_selected")
}
