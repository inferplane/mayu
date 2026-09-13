package controlplane

import (
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/inferplane/inferplane/api/v1alpha1"
	"github.com/inferplane/inferplane/internal/policy"
)

func TestLedgerTotalsSaturateInsteadOfWrappingAuthority(t *testing.T) {
	l := &ruleLedger{
		spent:     map[string]int64{"one": math.MaxInt64, "two": math.MaxInt64},
		allowance: map[string]int64{},
	}
	spent, _ := l.totals(time.Now(), nil, "")
	if spent != math.MaxInt64 {
		t.Fatalf("overflow made spent authority reusable: %d", spent)
	}
}

func TestStrictSoftSwitchBudgetDoesNotClampHardTotalLease(t *testing.T) {
	doc := v1alpha1.GovernancePolicy{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: v1alpha1.KindGovernancePolicy},
		Metadata: v1alpha1.ObjectMeta{Name: "team", Generation: 1},
		Spec: v1alpha1.GovernancePolicySpec{Subject: v1alpha1.Subject{Team: "alpha"}, Rules: []v1alpha1.Rule{
			{Name: "switch", FailurePolicy: v1alpha1.FailOpen, Budget: &v1alpha1.BudgetRule{LimitMilliUSD: 100}},
			{Name: "total", FailurePolicy: v1alpha1.FailClosed, Budget: &v1alpha1.BudgetRule{LimitMilliUSD: 1000, HardCap: true}},
			{Name: "route", FailurePolicy: v1alpha1.FailOpen, Routing: &v1alpha1.RoutingRule{BudgetTiers: &v1alpha1.BudgetTiersRule{
				BudgetRef: "switch", EnforceTargets: true,
				Tiers: []v1alpha1.BudgetTier{{ThresholdPercent: 100, Substitute: map[string]string{"premium": "economy"}}},
			}}},
		}},
	}
	s, err := NewServer("", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.applyWire([]v1alpha1.GovernancePolicy{doc}, nil); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	s.Mount(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()
	first := doSync(t, ts.URL, "", policy.SyncRequest{Dataplane: "dp"})
	if len(first.Leases) != 1 || first.Leases[0].Rule != "total" || !first.Leases[0].HardCap {
		t.Fatalf("switch threshold became admission authority: %+v", first.Leases)
	}
	next := doSync(t, ts.URL, "", policy.SyncRequest{Dataplane: "dp", Reports: []policy.ConsumptionReport{{
		Policy: "team", Rule: "switch", Team: "alpha", SpentMicroUSD: 100000,
	}}})
	if len(next.ActiveTiers) != 1 || !next.ActiveTiers[0].EnforceTargets || next.ActiveTiers[0].Substitute["premium"] != "economy" {
		t.Fatalf("strict constraint not distributed: %+v", next.ActiveTiers)
	}
}
