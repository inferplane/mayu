package router

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/inferplane/inferplane/api/v1alpha1"
	"github.com/inferplane/inferplane/internal/policy"
)

func TestExtendedContextKeywordsExcludeNonUserInstructions(t *testing.T) {
	for _, mode := range []string{"legacy", "normal", "stability", "both"} {
		for _, tt := range []struct {
			name, protocol, raw string
			latestKeyword       bool
		}{
			{"system", "anthropic", `{"system":"security architecture","messages":[{"role":"user","content":"hello"}]}`, false},
			{"tool definition", "anthropic", `{"tools":[{"name":"review","description":"security architecture","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"hello"}]}`, false},
			{"old history", "openai", `{"messages":[{"role":"user","content":"security architecture"},{"role":"assistant","content":"done"},{"role":"user","content":"hello"}]}`, false},
			{"tool result", "anthropic", `{"messages":[{"role":"user","content":[{"type":"text","text":"hello"},{"type":"tool_result","tool_use_id":"call","content":"security architecture"}]}]}`, false},
			{"responses instructions", "responses", `{"model":"premium","instructions":"security architecture","input":"hello"}`, false},
			{"latest user", "anthropic", `{"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"done"},{"role":"user","content":"review SECURITY"}]}`, true},
			{"responses latest user", "responses", `{"model":"premium","input":"review SECURITY"}`, true},
		} {
			t.Run(mode+"/"+tt.name, func(t *testing.T) {
				r, in := routingSetup(t, routingConfig())
				in.Protocol, in.RawBody = tt.protocol, []byte(tt.raw)
				in.Compatible = func(ChainTarget) bool { return true }
				p := contextPolicy("context", v1alpha1.Shadow, "economy")
				c := p.Rules[0].Routing.Context
				c.MaxSimpleInputTokens = 10000
				if mode == "normal" || mode == "both" {
					c.NormalModel, c.MaxNormalInputTokens = "private", 20000
				}
				if mode == "stability" || mode == "both" {
					c.Stability = &policy.ContextStability{MinHold: time.Minute, MinRequests: 3, SessionTTL: time.Hour}
				}
				installRoutingPolicies(r, p)
				got, err := r.RouteRequest(context.Background(), in)
				want := "economy"
				if mode == "legacy" || tt.latestKeyword {
					want = "premium"
				}
				if err != nil || got.Decision.ProposedModel != want {
					t.Fatalf("keyword selection got %+v %v, want proposal %s", got, err, want)
				}
			})
		}
	}
}

func TestExtendedContextSizeStillIncludesFullRequest(t *testing.T) {
	for _, mode := range []string{"normal", "stability"} {
		t.Run(mode, func(t *testing.T) {
			r, in := routingSetup(t, routingConfig())
			in.RawBody = []byte(`{"system":"` + strings.Repeat("padding ", 150) + `","messages":[{"role":"user","content":"hello"}]}`)
			p := contextPolicy("context", v1alpha1.Enforce, "economy")
			c := p.Rules[0].Routing.Context
			c.MaxSimpleInputTokens = 200
			want := "premium"
			if mode == "normal" {
				c.NormalModel, c.MaxNormalInputTokens = "private", 5000
				want = "private"
			} else {
				c.Stability = &policy.ContextStability{MinHold: time.Minute, MinRequests: 3, SessionTTL: time.Hour}
			}
			installRoutingPolicies(r, p)
			got, err := r.RouteRequest(context.Background(), in)
			if err != nil || got.Model != want {
				t.Fatalf("system content disappeared from size threshold: %+v %v", got, err)
			}
		})
	}
}

func TestExtendedContextCapacityStillIncludesFullRequest(t *testing.T) {
	cfg := routingConfig()
	model := cfg.Models["economy"]
	model.ContextWindow = 200
	cfg.Models["economy"] = model
	r, in := routingSetup(t, cfg)
	in.RawBody = []byte(`{"system":"` + strings.Repeat("padding ", 150) + `","messages":[{"role":"user","content":"hello"}]}`)
	p := contextPolicy("context", v1alpha1.Enforce, "economy")
	c := p.Rules[0].Routing.Context
	c.NormalModel, c.MaxSimpleInputTokens, c.MaxNormalInputTokens = "private", 10000, 20000
	installRoutingPolicies(r, p)
	got, err := r.RouteRequest(context.Background(), in)
	if err != nil || got.Model != "premium" || got.Decision.ProposedModel != "economy" || got.Decision.Reason != "context_unavailable" {
		t.Fatalf("candidate capacity ignored full input: %+v %v", got, err)
	}
}
