package router

import (
	"context"
	"testing"
)

func TestRequestRoutingOriginalRecoveryKeepsMetadataExemption(t *testing.T) {
	for _, metadata := range []string{"unknown context", "unpriced", "both"} {
		t.Run(metadata, func(t *testing.T) {
			cfg := routingConfig()
			if metadata != "unpriced" {
				mc := cfg.Models["private"]
				mc.ContextWindow = 0
				cfg.Models["private"] = mc
			}
			if metadata != "unknown context" {
				delete(cfg.Pricing.Overrides["private"], "private-upstream")
				delete(cfg.Pricing.Overrides["private2"], "private-retry")
			}
			r, in := routingSetup(t, cfg)
			in.Model, in.RequestedModel = "private-alias", "private-alias"
			protectedInput(&in)
			installRoutingPolicies(r, privatePolicy("p", "private"))
			for _, open := range []bool{false, true} {
				if open {
					for _, name := range []string{"private", "private2"} {
						id, _ := in.State.Identity(name)
						for i := 0; i < 5; i++ {
							r.brk.RecordFailure(id)
						}
					}
				}
				in.Chain, in.State, _ = r.ResolveChain("private")
				if open {
					for _, ct := range in.Chain {
						if ct.DataBoundary == "internal" {
							t.Fatal("fixture did not open both internal breakers")
						}
					}
				}
				got, err := r.RouteRequest(context.Background(), in)
				if err != nil || got.Model != "private" || len(got.Chain) != 2 {
					t.Fatalf("original lost with open=%v: %+v %v", open, got, err)
				}
				for _, ct := range got.Chain {
					if ct.DataBoundary != "internal" || ct.Model != "private" {
						t.Fatalf("unsafe recovery target: %+v", ct)
					}
				}
			}
			in.Chain = nil
			if got, err := r.RouteRequest(context.Background(), in); err != nil || len(got.Chain) != 2 {
				t.Fatalf("empty-chain original recovery: %+v %v", got, err)
			}
			// The same unavailable metadata cannot be excused for a different model.
			in.Model, in.RequestedModel = "premium", "premium"
			in.Chain, in.State, _ = r.ResolveChain("premium")
			got, err := r.RouteRequest(context.Background(), in)
			requireDenied(t, got, err, "no_safe_route")
		})
	}
}

// All genuine fallback alternatives must remain strict even when recovering an
// original that has no declared context or price.
func TestRequestRoutingOriginalRecoveryFiltersAlternativeMetadata(t *testing.T) {
	cfg := routingConfig()
	mc := cfg.Models["private"]
	mc.ContextWindow = 0
	cfg.Models["private"] = mc
	cfg.ModelFallbacks["private"] = "economy"
	mc = cfg.Models["economy"]
	mc.ContextWindow = 0
	cfg.Models["economy"] = mc
	r, in := routingSetup(t, cfg)
	in.Model, in.RequestedModel = "private", "private"
	protectedInput(&in)
	in.Chain = nil
	installRoutingPolicies(r, privatePolicy("p", "private", "economy"))
	got, err := r.RouteRequest(context.Background(), in)
	if err != nil || len(got.Chain) != 2 {
		t.Fatalf("metadata exemption escaped original: %+v %v", got, err)
	}
	for _, ct := range got.Chain {
		if ct.Model != "private" {
			t.Fatalf("unknown-context alternative survived: %+v", ct)
		}
	}
}

func TestRequestRoutingContextKeepsOriginalWhenLeadingPathLosesFields(t *testing.T) {
	for _, leadingLossy := range []bool{true, false} {
		cfg := routingConfig()
		pc := cfg.Providers["public"]
		pc.Type = "request-routing-openai_compatible"
		cfg.Providers["public"] = pc
		for _, name := range []string{"private", "private2"} {
			pc := cfg.Providers[name]
			pc.Type = "request-routing-bedrock"
			cfg.Providers[name] = pc
		}
		if !leadingLossy {
			mc := cfg.Models["private"]
			mc.Targets[0], mc.Targets[1] = mc.Targets[1], mc.Targets[0]
			cfg.Models["private"] = mc
		}
		r, in := routingSetup(t, cfg)
		in.Protocol = "openai"
		in.RawBody = []byte(`{"messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hello"}],"max_completion_tokens":32,"stop":["END"]}`)
		installRoutingPolicies(r, contextPolicy("context", "Enforce", "private"))
		got, err := r.RouteRequest(context.Background(), in)
		want := "private"
		if leadingLossy {
			want = "premium"
		}
		if err != nil || got.Model != want {
			t.Fatalf("leadingLossy=%v: got %+v %v", leadingLossy, got, err)
		}
		for _, ct := range got.Chain {
			if ct.Provider.Name() == "bedrock" {
				t.Fatal("lossy Converse attempt survived")
			}
		}
	}
}

func TestRequestRoutingContextCanPromoteCompatibleRegionalFallback(t *testing.T) {
	cfg := routingConfig()
	mc := cfg.Models["economy"]
	mc.Targets = append(mc.Targets, cfg.Models["private"].Targets[1])
	cfg.Models["economy"] = mc
	r, in := routingSetup(t, cfg)
	in.AllowedRegions = []string{"us"}
	installRoutingPolicies(r, contextPolicy("context", "Enforce", "economy"))
	got, err := r.RouteRequest(context.Background(), in)
	if err != nil || got.Model != "economy" || len(got.Chain) != 1 || got.Chain[0].ProviderName != "public" {
		t.Fatalf("compatible regional fallback refused: %+v %v", got, err)
	}
}
