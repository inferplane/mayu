package policy

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/inferplane/inferplane/api/v1alpha1"
)

const routingBudgetRules = `[
 {"name":"switch","failurePolicy":"FailOpen","budget":{"limitMilliUSD":100}},
 {"name":"total","failurePolicy":"FailClosed","budget":{"limitMilliUSD":1000,"hardCap":true,"adminContact":"budget-ops"}},
 {"name":"cutover","failurePolicy":"FailOpen","routing":{"budgetTiers":{"budgetRef":"switch","enforceTargets":true,"tiers":[{"thresholdPercent":100,"substitute":{"premium":"cheap"}}]}}}
]`

func routingThreshold(t *testing.T, limits TeamLimits, field string) int64 {
	t.Helper()
	raw, err := json.Marshal(limits)
	if err != nil {
		t.Fatal(err)
	}
	// TeamLimits also has bool/string fields; inspect only this numeric field.
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	value, exists := fields[field]
	if !exists {
		t.Fatalf("TeamLimits lacks routing accounting field %s", field)
	}
	var threshold int64
	if err := json.Unmarshal(value, &threshold); err != nil {
		t.Fatal(err)
	}
	return threshold
}

func TestRoutingBudgetSeparatesSoftSwitchFromIndependentHardCap(t *testing.T) {
	for _, day := range []bool{false, true} {
		d := routingDoc(t, routingBudgetRules)
		if day {
			d.Spec.Rules[0].Budget.Period = v1alpha1.PeriodCalendarDay
			d.Spec.Rules[1].Budget.Period = v1alpha1.PeriodCalendarDay
		}
		for _, reverse := range []bool{false, true} {
			if reverse {
				d.Spec.Rules[0], d.Spec.Rules[1] = d.Spec.Rules[1], d.Spec.Rules[0]
			}
			s := NewEmptyStore()
			if rejected := s.ApplyWire([]v1alpha1.GovernancePolicy{d}); len(rejected) != 0 {
				t.Fatal(rejected)
			}
			limits, ok := s.TeamLimits("eng")
			if !ok {
				t.Fatal("budget policy disappeared")
			}
			if day {
				if limits.BudgetMicrosPerDay != 1000000 || !limits.BudgetDayHard || limits.AdminContactDay != "budget-ops" {
					t.Fatalf("switch threshold weakened daily total cap: %+v", limits)
				}
				if routingThreshold(t, limits, "RoutingBudgetMicrosPerDay") != 100000 ||
					routingThreshold(t, limits, "RoutingBudgetMicrosPerMonth") != 0 || limits.BudgetMicrosPerMonth != 0 {
					t.Fatalf("day accounting leaked into month: %+v", limits)
				}
			} else {
				if limits.BudgetMicrosPerMonth != 1000000 || !limits.BudgetHard || limits.AdminContact != "budget-ops" {
					t.Fatalf("switch threshold weakened monthly total cap: %+v", limits)
				}
				if routingThreshold(t, limits, "RoutingBudgetMicrosPerMonth") != 100000 ||
					routingThreshold(t, limits, "RoutingBudgetMicrosPerDay") != 0 || limits.BudgetMicrosPerDay != 0 {
					t.Fatalf("month accounting leaked into day: %+v", limits)
				}
			}
		}
	}
}

func TestRoutingBudgetOnlyStillContributesAccountingEntry(t *testing.T) {
	d := routingDoc(t, routingBudgetRules)
	d.Spec.Rules = []v1alpha1.Rule{d.Spec.Rules[0], d.Spec.Rules[2]}
	s := NewEmptyStore()
	if rejected := s.ApplyWire([]v1alpha1.GovernancePolicy{d}); len(rejected) != 0 {
		t.Fatal(rejected)
	}
	got, ok := s.TeamLimits("eng")
	if !ok || got.BudgetMicrosPerMonth != 0 || got.BudgetHard || got.AdminContact != "" ||
		routingThreshold(t, got, "RoutingBudgetMicrosPerMonth") != 100000 {
		t.Fatalf("routing-only rule must contribute accounting without an admission cap: %+v, %v", got, ok)
	}
}

func TestRoutingBudgetLegacySoftAndReferencedHardKeepAdmissionSemantics(t *testing.T) {
	for _, strictHard := range []bool{false, true} {
		d := routingDoc(t, routingBudgetRules)
		if strictHard {
			d.Spec.Rules[0].Budget.HardCap = true
			d.Spec.Rules[0].FailurePolicy = v1alpha1.FailClosed
		} else {
			d.Spec.Rules[2].Routing.BudgetTiers.EnforceTargets = false
			d.Spec.Rules[2].Routing.BudgetTiers.Tiers[0].ThresholdPercent = 99
		}
		s := NewEmptyStore()
		if rejected := s.ApplyWire([]v1alpha1.GovernancePolicy{d}); len(rejected) != 0 {
			t.Fatal(rejected)
		}
		got, _ := s.TeamLimits("eng")
		if got.BudgetMicrosPerMonth != 100000 || got.BudgetHard != strictHard ||
			routingThreshold(t, got, "RoutingBudgetMicrosPerMonth") != 0 {
			t.Fatalf("legacy soft or referenced hard budget lost existing semantics: %+v", got)
		}
	}
}

func TestRoutingBudgetReferencesStayDocumentLocal(t *testing.T) {
	switching := routingDoc(t, routingBudgetRules)
	ordinary := routingDoc(t, `[{"name":"switch","failurePolicy":"FailClosed","budget":{"limitMilliUSD":50,"hardCap":true}}]`)
	ordinary.Metadata.Name = "ordinary"
	for _, docs := range [][]v1alpha1.GovernancePolicy{{switching, ordinary}, {ordinary, switching}} {
		s := NewEmptyStore()
		if rejected := s.ApplyWire(docs); len(rejected) != 0 {
			t.Fatal(rejected)
		}
		got, _ := s.TeamLimits("eng")
		if got.BudgetMicrosPerMonth != 50000 || !got.BudgetHard ||
			routingThreshold(t, got, "RoutingBudgetMicrosPerMonth") != 100000 {
			t.Fatalf("strict reference reclassified another document's budget: %+v", got)
		}
	}
}

func TestRoutingBudgetStrictUserSubjectsRefused(t *testing.T) {
	for _, sub := range []v1alpha1.Subject{{User: "u"}, {Team: "eng", User: "u"}} {
		d := routingDoc(t, routingBudgetRules)
		d.Spec.Subject = sub
		if _, err := FromV1Alpha1(&d); err == nil || !strings.Contains(err.Error(), "team-only") {
			t.Fatalf("strict user budget tier not explicitly refused: %v", err)
		}
		if rejected := NewEmptyStore().ApplyWire([]v1alpha1.GovernancePolicy{d}); len(rejected) != 1 {
			t.Fatalf("user strict tier accepted by data plane: %v", rejected)
		}
	}
}

func TestIsRoutingOnlyBudgetRequiresSameDocumentSoftStrictReference(t *testing.T) {
	d := routingDoc(t, routingBudgetRules)
	p, err := FromV1Alpha1(&d)
	if err != nil {
		t.Fatal(err)
	}
	if !IsRoutingOnlyBudget(p, "switch") || IsRoutingOnlyBudget(p, "total") ||
		IsRoutingOnlyBudget(p, "cutover") || IsRoutingOnlyBudget(p, "missing") ||
		IsRoutingOnlyBudget(nil, "switch") {
		t.Fatal("helper did not identify exactly the referenced soft budget")
	}
	p.Rules[2].Routing.BudgetTiers.EnforceTargets = false
	if IsRoutingOnlyBudget(p, "switch") {
		t.Fatal("legacy substitution disabled admission")
	}
	p.Rules[2].Routing.BudgetTiers.EnforceTargets = true
	p.Rules[0].Budget.HardCap = true
	if IsRoutingOnlyBudget(p, "switch") {
		t.Fatal("strict routing disabled a referenced hard cap")
	}
}

func TestRoutingBudgetMultipleThresholdsAndWindowsStayIndependent(t *testing.T) {
	d := routingDoc(t, routingBudgetRules)
	p, err := FromV1Alpha1(&d)
	if err != nil {
		t.Fatal(err)
	}
	other := clonePolicy(p)
	other.Name = "other"
	other.Rules[0].Budget.LimitMicroUSD = 50000
	other.Rules[1].Budget.Period = v1alpha1.PeriodCalendarDay
	other.Rules[1].Budget.LimitMicroUSD = 250000
	day := clonePolicy(p)
	day.Name = "day"
	day.Rules = []Rule{day.Rules[0], day.Rules[2]}
	day.Rules[0].Budget.Period = v1alpha1.PeriodCalendarDay
	day.Rules[0].Budget.LimitMicroUSD = 25000
	got := mergeTeamLimits([]*Policy{p, other, day})["eng"]
	if got.RoutingBudgetMicrosPerMonth != 50000 || got.RoutingBudgetMicrosPerDay != 25000 ||
		got.BudgetMicrosPerMonth != 1000000 || got.BudgetMicrosPerDay != 250000 ||
		!got.BudgetHard || !got.BudgetDayHard {
		t.Fatalf("switching budgets were mixed with independent admission windows: %+v", got)
	}
}
