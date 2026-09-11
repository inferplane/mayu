package router

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/inferplane/inferplane/api/v1alpha1"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/policy"
)

func TestRejectedStrictDistributionCannotRestoreExpensiveRoute(t *testing.T) {
	r, in := routingSetup(t, routingConfig())
	var doc v1alpha1.GovernancePolicy
	if err := json.Unmarshal([]byte(`{"apiVersion":"inferplane.dev/v1alpha1","kind":"GovernancePolicy","metadata":{"name":"cost","generation":1},"spec":{"subject":{"team":"team"},"rules":[{"name":"budget","failurePolicy":"FailOpen","budget":{"limitMilliUSD":100}},{"name":"strict","failurePolicy":"FailOpen","routing":{"budgetTiers":{"budgetRef":"budget","enforceTargets":true,"tiers":[{"thresholdPercent":100,"substitute":{"premium":"economy"}}]}}}]}}`), &doc); err != nil {
		t.Fatal(err)
	}
	store := policy.NewEmptyStore()
	r.SetRoutingPolicyLookup(store.MatchingRoutingPolicies)
	constraints := map[string]string{"premium": "economy"}
	r.SetBudgetConstraintGate(func(keystore.Principal) map[string]string { return constraints })
	if rejected := store.ApplyWire([]v1alpha1.GovernancePolicy{doc}); len(rejected) != 0 {
		t.Fatal(rejected)
	}
	valid := doc
	got, err := r.RouteRequest(context.Background(), in)
	if err != nil || got.Model != "economy" {
		t.Fatalf("initial strict route failed: %+v %v", got, err)
	}
	doc.Spec.Rules = nil
	doc.Metadata.Generation = 2
	if rejected := store.ApplyWire([]v1alpha1.GovernancePolicy{doc}); len(rejected) != 1 {
		t.Fatal(rejected)
	}
	constraints = nil // rejected document vanished from the active tier table
	for _, count := range []bool{false, true} {
		in.CountOnly = count
		got, err = r.RouteRequest(context.Background(), in)
		requireDenied(t, got, err, "policy_lookup_failed")
		if !errors.Is(err, policy.ErrSensitivePolicyRejected) {
			t.Fatal("router lost the compatibility rejection cause")
		}
	}
	in.CountOnly = false
	valid.Metadata.Generation = 3
	if rejected := store.ApplyWire([]v1alpha1.GovernancePolicy{valid}); len(rejected) != 0 {
		t.Fatal(rejected)
	}
	constraints = map[string]string{"premium": "economy"}
	got, err = r.RouteRequest(context.Background(), in)
	if err != nil || got.Model != "economy" {
		t.Fatalf("valid strict recovery failed: %+v %v", got, err)
	}
}
