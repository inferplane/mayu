package router

import (
	"context"
	"testing"

	"github.com/inferplane/inferplane/api/v1alpha1"
	"github.com/inferplane/inferplane/internal/sensitivity"
)

func setCompatibility(t *testing.T, in *RequestRoutingInput, fn func(ChainTarget) bool) {
	t.Helper()
	in.Compatible = fn
}

func TestCompatibilityNarrowsLegacyAndLeadingContextTargets(t *testing.T) {
	r, in := routingSetup(t, routingConfig())
	setCompatibility(t, &in, func(ChainTarget) bool { return false })
	got, err := r.RouteRequest(context.Background(), in)
	requireDenied(t, got, err, "no_safe_route")

	installRoutingPolicies(r, contextPolicy("context", v1alpha1.Enforce, "private"))
	// A callback rejecting the preference's leading path must not let another
	// candidate make this preference appear available.
	setCompatibility(t, &in, func(ct ChainTarget) bool { return ct.ProviderName != "private" })
	got, err = r.RouteRequest(context.Background(), in)
	if err != nil || got.Model != "premium" || got.Decision.Reason != "context_unavailable" {
		t.Fatalf("leading candidate ignored callback: %+v %v", got, err)
	}
}

func TestCompatibilityCannotRelaxPrivacyAndAppliesToEveryRetry(t *testing.T) {
	r, in := routingSetup(t, routingConfig())
	protectedInput(&in)
	installRoutingPolicies(r, privatePolicy("privacy", "private"))
	setCompatibility(t, &in, func(ct ChainTarget) bool { return ct.ProviderName != "private2" })
	got, err := r.RouteRequest(context.Background(), in)
	if err != nil || len(got.Chain) != 1 || got.Chain[0].ProviderName != "private" {
		t.Fatalf("callback failed to narrow all retries: %+v %v", got, err)
	}
	setCompatibility(t, &in, func(ct ChainTarget) bool { return ct.ProviderName == "public" })
	got, err = r.RouteRequest(context.Background(), in)
	requireDenied(t, got, err, "no_safe_route")
}

func TestCompatibilityRevalidatesAffinity(t *testing.T) {
	r, in, _, _ := affinitySetup(t)
	first, _ := r.RouteRequest(context.Background(), in)
	recordAffinity(t, r, first, 2)
	in.RawBody = []byte(`{"messages":[{"role":"user","content":"security"}]}`)
	setCompatibility(t, &in, func(ct ChainTarget) bool { return ct.ProviderName != "private2" })
	got, err := r.RouteRequest(context.Background(), in)
	if err != nil || got.Model != "premium" || got.Decision.Reason == "context_affinity" {
		t.Fatalf("pin bypassed current callback: %+v %v", got, err)
	}
}

func TestResponsesCanonicalCompatibilityRequiresCallbackAndInspectableShape(t *testing.T) {
	for _, tt := range []struct {
		name                                        string
		callback, complete, tools, capability, want bool
	}{
		{"text", true, true, false, false, true},
		{"missing approval", false, true, false, false, false},
		{"opaque", true, false, false, false, false},
		{"tools", true, true, true, true, true},
		{"undeclared tools", true, true, true, false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := routingConfig()
			if tt.capability {
				model := cfg.Models["premium"]
				model.Capabilities = []string{"tools"}
				cfg.Models["premium"] = model
			}
			r, in := routingSetup(t, cfg)
			in.Protocol = "responses"
			in.RawBody = []byte(`{"model":"premium","input":"hello"}`)
			r.SetRequestInspector(inspectFunc(func(context.Context, string, []byte) (sensitivity.Result, error) {
				return sensitivity.Result{Complete: tt.complete, HasTools: tt.tools, UserTurns: 1, InputTokens: 10}, nil
			}))
			if tt.callback {
				setCompatibility(t, &in, func(ChainTarget) bool { return true })
			}
			got, err := r.RouteRequest(context.Background(), in)
			if (err == nil) != tt.want || (err != nil && len(got.Chain) != 0) {
				t.Fatalf("canonical compatibility: %+v %v", got, err)
			}
		})
	}
}

func TestResponsesCallbackCannotAuthorizeUnprovenBedrock(t *testing.T) {
	cfg := routingConfig()
	provider := cfg.Providers["public"]
	provider.Type = "request-routing-bedrock"
	cfg.Providers["public"] = provider
	r, in := routingSetup(t, cfg)
	in.Protocol = "responses"
	in.RawBody = []byte(`{"model":"premium","input":"hello"}`)
	setCompatibility(t, &in, func(ChainTarget) bool { return true })
	got, err := r.RouteRequest(context.Background(), in)
	requireDenied(t, got, err, "no_safe_route")
}

func TestResponsesDeclaredCanonicalContractsNeedCallback(t *testing.T) {
	for _, kind := range []string{"request-routing-openai_compatible", "request-routing-capable-custom"} {
		cfg := routingConfig()
		provider := cfg.Providers["public"]
		provider.Type = kind
		cfg.Providers["public"] = provider
		r, in := routingSetup(t, cfg)
		in.Protocol = "responses"
		in.RawBody = []byte(`{"model":"premium","input":"hello"}`)
		in.Compatible = func(ChainTarget) bool { return true }
		got, err := r.RouteRequest(context.Background(), in)
		if err != nil || len(got.Chain) == 0 {
			t.Fatalf("%s contract refused: %+v %v", kind, got, err)
		}
		in.Compatible = nil
		got, err = r.RouteRequest(context.Background(), in)
		requireDenied(t, got, err, "no_safe_route")
	}
}

func TestCompatibilityCannotRestoreExpensiveStrictFallback(t *testing.T) {
	r, in := routingSetup(t, routingConfig())
	setBudgetConstraint(t, r, map[string]string{"premium": "private"})
	in.Compatible = func(ct ChainTarget) bool { return ct.Model == "premium" }
	got, err := r.RouteRequest(context.Background(), in)
	requireDenied(t, got, err, "budget_target_unavailable")
}
