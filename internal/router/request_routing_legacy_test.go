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

func TestRequestRoutingPassiveContextPreservesCompleteChain(t *testing.T) {
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
	if err != nil || !reflect.DeepEqual(got.Chain, in.Chain) {
		t.Fatalf("Shadow changed an existing authorized retry chain: %+v %v", got, err)
	}
}

// The decision's planned target may differ from the model whose context the
// ingress checked before policy routing existed. Preserve both meanings.
func TestRequestRoutingPassiveModelAndChain(t *testing.T) {
	for _, scenario := range []string{"no-rule", "shadow", "unavailable", "unpriced", "unchanged", "ineligible", "inspection-error", "conflict"} {
		for _, alias := range []bool{false, true} {
			t.Run(scenario+map[bool]string{false: "/canonical", true: "/alias"}[alias], func(t *testing.T) {
				cfg := routingConfig()
				cfg.ModelFallbacks["premium"] = "private"
				m := cfg.Models["premium"]
				m.Aliases = []string{"premium-alias"}
				cfg.Models["premium"] = m
				m = cfg.Models["private"]
				m.ContextWindow = 0
				m.Capabilities = nil
				cfg.Models["private"] = m
				delete(cfg.Pricing.Overrides, "private")
				delete(cfg.Pricing.Overrides, "private2")
				if scenario == "unpriced" {
					// Reach the candidate's pricing check, not an earlier
					// physical incompatibility refusal.
					pc := cfg.Providers["private"]
					pc.Type = "request-routing-openai_compatible"
					cfg.Providers["private"] = pc
				}
				r, in := routingSetup(t, cfg)
				in.Protocol = "openai" // existing Anthropic transports are intentionally legacy
				in.AllowedRegions = []string{"eu"}
				in.Chain = FilterRegions(in.Chain, in.AllowedRegions)
				if alias {
					in.Model = "premium-alias"
				}
				wantReason := "unchanged"
				if scenario != "no-rule" {
					mode := v1alpha1.Enforce
					target := "missing"
					switch scenario {
					case "shadow":
						mode = v1alpha1.Shadow
						wantReason = "context_shadow"
					case "unavailable":
						wantReason = "context_unavailable"
					case "unpriced":
						target = "economy"
						wantReason = "context_unavailable"
					case "unchanged":
						target = "premium"
						wantReason = "context_unchanged"
					case "ineligible":
						in.RawBody = []byte(`{"messages":[{"role":"assistant","content":"history"},{"role":"user","content":"hi"}]}`)
						wantReason = "context_ineligible"
					case "inspection-error":
						in.RawBody = []byte(`{`)
						wantReason = "context_inspection_failed"
					case "conflict":
						wantReason = "context_conflict"
					}
					docs := []*policy.Policy{contextPolicy("optional", mode, target)}
					if scenario == "conflict" {
						docs = append(docs, contextPolicy("different", mode, "premium"))
					}
					installRoutingPolicies(r, docs...)
				}
				got, err := r.RouteRequest(context.Background(), in)
				if err != nil || got.Model != in.Model || got.Decision.SelectedModel != "private" || got.Decision.Reason != wantReason || got.State != in.State || !reflect.DeepEqual(got.Chain, in.Chain) {
					t.Fatalf("passive request model/chain drifted: result=%+v err=%v wantModel=%s wantReason=%s", got, err, in.Model, wantReason)
				}
				got.Chain[0].ProviderName = "mutated"
				if in.Chain[0].ProviderName == "mutated" {
					t.Fatal("passive result borrowed chain storage")
				}
			})
		}
	}
}

// Passive legacy exemptions must never be copied into an actually selected
// model's chain, even when its fallback points back to the original model.
func TestRequestRoutingSelectedFallbackToOriginalStaysStrict(t *testing.T) {
	for _, constraint := range []string{"valid", "physical", "unknown-context", "small-context", "unpriced"} {
		t.Run(constraint, func(t *testing.T) {
			cfg := routingConfig()
			pc := cfg.Providers["private"]
			pc.Type = "request-routing-openai_compatible"
			cfg.Providers["private"] = pc
			pc = cfg.Providers["public"]
			if constraint != "physical" {
				pc.Type = "request-routing-openai_compatible"
			}
			cfg.Providers["public"] = pc
			cfg.ModelFallbacks["economy"] = "premium"
			m := cfg.Models["premium"]
			switch constraint {
			case "unknown-context":
				m.ContextWindow = 0
			case "small-context":
				m.ContextWindow = 1
			case "unpriced":
				delete(cfg.Pricing.Overrides["public"], "premium-upstream")
			}
			cfg.Models["premium"] = m
			r, in := routingSetup(t, cfg)
			in.Protocol = "openai"
			installRoutingPolicies(r, contextPolicy("select", v1alpha1.Enforce, "economy"))
			got, err := r.RouteRequest(context.Background(), in)
			want := 1
			if constraint == "valid" {
				want = 2
			}
			if err != nil || got.Model != "economy" || got.Decision.Reason != "context_selected" || len(got.Chain) != want || got.Chain[0].ProviderName != "private" {
				t.Fatalf("selected fallback inherited passive exemption: %+v %v", got, err)
			}
			if constraint == "valid" && got.Chain[1].Model != "premium" {
				t.Fatal("valid selected-chain fallback control was lost")
			}
		})
	}
}
