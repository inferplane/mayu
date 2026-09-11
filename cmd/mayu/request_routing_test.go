package main

import (
	"context"
	"errors"
	"testing"

	"github.com/inferplane/inferplane/api/v1alpha1"
	"github.com/inferplane/inferplane/internal/config"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/live"
	"github.com/inferplane/inferplane/internal/policy"
	"github.com/inferplane/inferplane/internal/pricing"
	"github.com/inferplane/inferplane/internal/router"
	"github.com/inferplane/inferplane/providers"
	"github.com/inferplane/inferplane/providers/testing/mockprovider"
)

// Exercise the exact assembly helper used by newGateway, without opening its
// listeners. A rejected distribution must reach the request routing error gate.
func TestWireRequestRoutingPolicies(t *testing.T) {
	st := live.NewState(map[string]providers.Provider{"p": mockprovider.New("m")}, map[string]config.ModelConfig{"m": {Targets: []config.Target{{Provider: "p", Model: "m"}}}}, pricing.New(pricing.OnMissingAllow, nil), nil)
	holder := &live.Holder{}
	holder.Swap(st)
	r := router.New(holder)
	chain, _, err := r.ResolveChain("m")
	if err != nil {
		t.Fatal(err)
	}
	in := router.RequestRoutingInput{State: st, Chain: chain, RequestedModel: "m", Model: "m", Protocol: "anthropic", RawBody: []byte(`{"messages":[{"role":"user","content":"person@example.test"}]}`), Principal: keystore.Principal{Team: "team", AllowedModels: []string{"*"}, KeyOptions: keystore.KeyOptions{Owner: "user"}}}
	wireRoutingPolicyGates(r, nil)
	if _, err := r.RouteRequest(context.Background(), in); err != nil {
		t.Fatalf("nil policy wiring: %v", err)
	}
	store := policy.NewEmptyStore()
	wireRoutingPolicyGates(r, store)
	doc := v1alpha1.GovernancePolicy{TypeMeta: v1alpha1.TypeMeta{APIVersion: "inferplane.dev/v1alpha1", Kind: "GovernancePolicy"}, Metadata: v1alpha1.ObjectMeta{Name: "privacy", Generation: 1}, Spec: v1alpha1.GovernancePolicySpec{Subject: v1alpha1.Subject{Team: "team", User: "user"}, Rules: []v1alpha1.Rule{{Name: "block", FailurePolicy: v1alpha1.FailClosed, SensitiveData: &v1alpha1.SensitiveDataRule{OnDetected: v1alpha1.Block, OnUninspectable: v1alpha1.Block}}}}}
	if rejected := store.ApplyWire([]v1alpha1.GovernancePolicy{doc}); len(rejected) > 0 {
		t.Fatalf("fixture rejected: %v", rejected)
	}
	got, err := r.RouteRequest(context.Background(), in)
	var denial *router.RequestRoutingError
	if !errors.As(err, &denial) || denial.Reason != "sensitive_blocked" || len(got.Chain) > 0 {
		t.Fatalf("store policy not wired: %+v %v", got, err)
	}
	in.Principal.Owner = "other"
	if _, err := r.RouteRequest(context.Background(), in); err != nil {
		t.Fatalf("user selector not passed: %v", err)
	}
	in.Principal.Owner = "user"
	in.Principal.Team = "other"
	if _, err := r.RouteRequest(context.Background(), in); err != nil {
		t.Fatalf("team selector not passed: %v", err)
	}
	in.Principal.Team = "team"
	doc.Spec.Rules = nil
	if rejected := store.ApplyWire([]v1alpha1.GovernancePolicy{doc}); len(rejected) == 0 {
		t.Fatal("invalid replacement accepted")
	}
	got, err = r.RouteRequest(context.Background(), in)
	if !errors.Is(err, policy.ErrSensitivePolicyRejected) || len(got.Chain) > 0 {
		t.Fatalf("routing lookup rejection lost: %+v %v", got, err)
	}
	doc.Spec.Rules = []v1alpha1.Rule{{Name: "access", FailurePolicy: v1alpha1.FailClosed, ModelAccess: &v1alpha1.ModelAccessRule{Allow: []string{"other"}}}}
	if rejected := store.ApplyWire([]v1alpha1.GovernancePolicy{doc}); len(rejected) > 0 {
		t.Fatalf("valid replacement rejected: %v", rejected)
	}
	if r.Allows(in.Principal, "m") {
		t.Fatal("existing model-access wiring lost")
	}
}
