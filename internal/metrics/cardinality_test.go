package metrics

import (
	"strings"
	"testing"
)

// forbiddenLabelName reports whether a Prometheus label NAME would carry a
// raw user id — keystore.Principal.Owner is an admin-supplied ≤256-byte free
// string or an OIDC sub, the same unbounded-cardinality class this codebase
// already bars key_id from (requirements §A5). key_id is included because it
// is the same invariant.
func forbiddenLabelName(name string) bool {
	switch strings.ToLower(name) {
	case "user", "owner", "user_id", "subject", "sub", "principal", "key_id":
		return true
	}
	return false
}

// TestNoMetricCarriesAUserLabel gathers the registry and walks EVERY metric
// family and EVERY label pair, failing on any forbidden label name. The walk
// is structural on purpose: a hand-maintained list of "metrics we checked"
// catches nothing a future author forgets to add to the list, while a
// registry walk catches a user-labelled series no matter which collector
// grows it.
func TestNoMetricCarriesAUserLabel(t *testing.T) {
	m := New()
	// Record something on every collector so every family actually appears
	// in the gather — Prometheus emits a labeled family only once it has at
	// least one child series, and a guard that walked 0 families would pass
	// vacuously.
	m.ObserveTokenUsage("input", "model-a", "prov-a", "team-a", 10)
	m.ObserveRequest("anthropic", "model-a", "prov-a", "team-a", 200, 1.0, 0.2)
	m.ObserveFallback("model-a", "prov-a", "prov-b", "error")
	m.SetCircuitState("prov-a", 1)
	m.SetQuotaUtilization("team-a", "day", 0.5)
	m.SetBudgetUtilization("team-a", 0.5)
	m.AddBudgetSpend("team-a", "model-a", "total", 1.5)
	m.IncPricingMiss("prov-a", "model-a")
	m.IncAuditFailure("file")
	m.SetAuditBufferUtilization(0.1)
	m.ObservePIIMask("team-a", 3)
	m.IncAnchorFailure()
	m.IncUsageWindowDropped()
	m.ObserveRoutingDecision("team-a", "Shadow", "context_shadow")

	mfs, err := m.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	if len(mfs) == 0 {
		t.Fatal("registry gather returned 0 metric families — the guard walked nothing")
	}
	labelPairs := 0
	families := make([]string, 0, len(mfs))
	for _, mf := range mfs {
		families = append(families, mf.GetName())
		for _, met := range mf.GetMetric() {
			for _, lp := range met.GetLabel() {
				labelPairs++
				if forbiddenLabelName(lp.GetName()) {
					t.Errorf("metric family %q carries label %q: a raw user id (or key id) must never be a Prometheus label — unbounded cardinality (requirements §A5)", mf.GetName(), lp.GetName())
				}
			}
		}
	}
	if labelPairs == 0 {
		t.Fatal("inspected 0 label pairs — the guard passed vacuously")
	}
	t.Logf("walked %d metric families (%s), inspected %d label pairs",
		len(mfs), strings.Join(families, ", "), labelPairs)
}

// TestBudgetUtilizationTakesTeamOnly pins SetBudgetUtilization's gauge to
// exactly the label set {"team"}, asserted on the GATHERED family — so a
// future SetBudgetUtilization(team, user, ratio) overload changes this test
// rather than shipping a user-labelled series silently.
func TestBudgetUtilizationTakesTeamOnly(t *testing.T) {
	m := New()
	m.SetBudgetUtilization("team-a", 0.42)

	mfs, err := m.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	const family = "inferplane_budget_utilization_ratio"
	found := false
	for _, mf := range mfs {
		if mf.GetName() != family {
			continue
		}
		found = true
		for _, met := range mf.GetMetric() {
			names := make([]string, 0, len(met.GetLabel()))
			for _, lp := range met.GetLabel() {
				names = append(names, lp.GetName())
			}
			if len(names) != 1 || names[0] != "team" {
				t.Fatalf("%s label names = %v, want exactly [team]", family, names)
			}
		}
	}
	if !found {
		t.Fatalf("%s not present in gather after a write — the assertion never ran", family)
	}
}
