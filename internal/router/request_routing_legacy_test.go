package router

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/inferplane/inferplane/api/v1alpha1"
	"github.com/inferplane/inferplane/internal/config"
	"github.com/inferplane/inferplane/internal/policy"
	"github.com/inferplane/inferplane/internal/sensitivity"
)

type legacyInspectorTrap struct{ t *testing.T }

func (i legacyInspectorTrap) Inspect(context.Context, string, []byte) (sensitivity.Result, error) {
	i.t.Fatal("no matching new rule must not inspect content")
	return sensitivity.Result{}, nil
}

// Adding the ingress policy seam must not silently apply its new physical,
// capability or pricing requirements to existing configurations.
func TestRequestRoutingNoNewRulesPreservesLegacyChain(t *testing.T) {
	for _, mode := range []string{"disabled", "empty", "ordinary", "unmatched-context"} {
		t.Run(mode, func(t *testing.T) {
			cfg := routingConfig()
			m := cfg.Models["premium"]
			m.Targets = cfg.Models["private"].Targets
			m.ContextWindow = 0
			m.Capabilities = nil
			cfg.Models["premium"] = m
			cfg.Pricing = config.PricingConfig{}
			r, in := routingSetup(t, cfg)
			in.Protocol = "openai" // direct Anthropic paths remain legacy behavior
			in.RawBody = []byte(`{"messages":[{"role":"user","content":"person@example.test"}],"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}]}`)
			r.SetRequestInspector(legacyInspectorTrap{t})
			lookups := 0
			if mode != "disabled" {
				r.SetRoutingPolicyLookup(func(string, string) ([]*policy.Policy, error) {
					lookups++
					switch mode {
					case "ordinary":
						return []*policy.Policy{{Name: "old", Rules: []policy.Rule{{Name: "old-rule"}}}}, nil
					case "unmatched-context":
						p := contextPolicy("other-model", v1alpha1.Enforce, "economy")
						p.Rules[0].Routing.Context.FromModels = []string{"economy"}
						return []*policy.Policy{p}, nil
					}
					return nil, nil
				})
			}
			got, err := r.RouteRequest(context.Background(), in)
			if err != nil || !reflect.DeepEqual(got.Chain, in.Chain) || got.State != in.State || got.Model != "premium" {
				t.Fatalf("no-rule path changed existing targets: %+v %v", got, err)
			}
			if got.Decision.Reason != "unchanged" || got.Decision.Inspection != "not_inspected" || len(got.Decision.Policies) != 0 {
				t.Fatalf("no-rule path acquired policy evidence: %+v", got.Decision)
			}
			if mode != "disabled" && lookups != 1 {
				t.Fatal("policy lookup skipped or repeated")
			}
			got.Chain[0].ProviderName = "mutated"
			if in.Chain[0].ProviderName == "mutated" {
				t.Fatal("legacy result aliases borrowed chain")
			}
		})
	}
}

func TestRequestRoutingNoNewRulesRetainsGuards(t *testing.T) {
	cfg := routingConfig()
	cfg.ModelFallbacks["premium"] = "private"
	r, in := routingSetup(t, cfg)
	in.Protocol = "openai"
	r.SetRequestInspector(legacyInspectorTrap{t})
	// Legacy physical compatibility is permitted, but the existing per-target
	// model/region boundaries cannot be bypassed by the no-rule shortcut.
	in.Principal.AllowedModels = []string{"premium"}
	got, err := r.RouteRequest(context.Background(), in)
	if err != nil || len(got.Chain) != 1 || got.Chain[0].Model != "premium" {
		t.Fatalf("no-rule RBAC filter: %+v %v", got, err)
	}
	in.Principal.AllowedModels = []string{"*"}
	in.AllowedRegions = []string{"eu"}
	got, err = r.RouteRequest(context.Background(), in)
	if err != nil || len(got.Chain) != 2 {
		t.Fatalf("no-rule region filter: %+v %v", got, err)
	}
	for _, ct := range got.Chain {
		if ct.Region != "eu" {
			t.Fatal("no-rule path widened region access")
		}
	}
	in.Principal.AllowedModels = []string{"private"}
	got, err = r.RouteRequest(context.Background(), in)
	requireDenied(t, got, err, "model_forbidden")
	in.Principal.AllowedModels = []string{"*"}
	r.SetRoutingPolicyLookup(func(string, string) ([]*policy.Policy, error) { return nil, policy.ErrSensitivePolicyRejected })
	got, err = r.RouteRequest(context.Background(), in)
	requireDenied(t, got, err, "policy_lookup_failed")
	if !errors.Is(err, policy.ErrSensitivePolicyRejected) {
		t.Fatal("distribution rejection lost")
	}
}

func TestRequestRoutingMatchingRulesStillGatePhysicalCompatibility(t *testing.T) {
	r, in := routingSetup(t, routingConfig())
	in.Protocol = "openai"
	protectedInput(&in)
	installRoutingPolicies(r, privatePolicy("privacy", "private"), contextPolicy("matching", v1alpha1.Shadow, "economy"))
	got, err := r.RouteRequest(context.Background(), in)
	requireDenied(t, got, err, "no_safe_route")
}

func TestRequestRoutingContextOnlyPreservesOriginalTransport(t *testing.T) {
	for _, mode := range []v1alpha1.ContextMode{v1alpha1.Shadow, v1alpha1.Enforce} {
		t.Run(string(mode), func(t *testing.T) {
			cfg := routingConfig()
			m := cfg.Models["premium"]
			m.ContextWindow = 0
			m.Capabilities = nil
			cfg.Models["premium"] = m
			r, in := routingSetup(t, cfg)
			in.Protocol = "openai"
			installRoutingPolicies(r, contextPolicy("optional", mode, "economy"))
			got, err := r.RouteRequest(context.Background(), in)
			reason := "context_shadow"
			if mode == v1alpha1.Enforce {
				reason = "context_unavailable"
			}
			if err != nil || got.Model != "premium" || !reflect.DeepEqual(got.Chain, in.Chain) || got.Decision.Reason != reason || got.Decision.ProposedModel != "economy" {
				t.Fatalf("optional context denied/changed original transport: %+v %v", got, err)
			}
		})
	}
}

func TestRequestRoutingContextOriginalExceptionNeverCoversAlternatives(t *testing.T) {
	cfg := routingConfig()
	pc := cfg.Providers["private"]
	pc.Type = "request-routing-openai_compatible"
	cfg.Providers["private"] = pc
	m := cfg.Models["economy"]
	m.Targets = append(m.Targets, config.Target{Provider: "public", Model: "premium-upstream"})
	cfg.Models["economy"] = m
	cfg.ModelFallbacks["economy"] = "premium"
	r, in := routingSetup(t, cfg)
	in.Protocol = "openai"
	installRoutingPolicies(r, contextPolicy("optional", v1alpha1.Enforce, "economy"))
	got, err := r.RouteRequest(context.Background(), in)
	if err != nil || got.Model != "economy" || len(got.Chain) != 1 || got.Chain[0].ProviderName != "private" {
		t.Fatalf("original-transport exception leaked into chosen model/retries: %+v %v", got, err)
	}
}

func TestRequestRoutingContextOnlyOriginalRetryIsStrict(t *testing.T) {
	cfg := routingConfig()
	m := cfg.Models["premium"]
	m.Targets = append(m.Targets, config.Target{Provider: "private", Model: "private-upstream"}, config.Target{Provider: "private2", Model: "private-retry"})
	cfg.Models["premium"] = m
	pc := cfg.Providers["private2"]
	pc.Type = "request-routing-openai_compatible"
	cfg.Providers["private2"] = pc
	r, in := routingSetup(t, cfg)
	in.Protocol = "openai"
	installRoutingPolicies(r, contextPolicy("optional", v1alpha1.Shadow, "economy"))
	got, err := r.RouteRequest(context.Background(), in)
	if err != nil || len(got.Chain) != 2 || got.Chain[0].ProviderName != "public" || got.Chain[1].ProviderName != "private2" {
		t.Fatalf("Shadow must retain original but refuse incompatible retry: %+v %v", got, err)
	}
}
