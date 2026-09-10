package bedrock

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/inferplane/inferplane/api/v1alpha1"
	"github.com/inferplane/inferplane/internal/config"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/live"
	"github.com/inferplane/inferplane/internal/policy"
	"github.com/inferplane/inferplane/internal/router"
	"github.com/inferplane/inferplane/providers"
	"github.com/inferplane/inferplane/providers/testing/mockprovider"
)

// Use the real Bedrock provider and converter, stopping only at its SDK seam.
// An optimistic structural declaration must not override known built-in loss.
type routingContractProvider struct{ *provider }

func (*routingContractProvider) SupportsIngress(string) bool { return true }
func init() {
	providers.Register("task3-converse-contract", func(providers.Config) (providers.Provider, error) {
		return &routingContractProvider{&provider{conv: &fakeConverser{}}}, nil
	})
	providers.Register("task3-safe-contract", func(providers.Config) (providers.Provider, error) { return mockprovider.New("premium"), nil })
}

func TestRequestRoutingConverseTransportContract(t *testing.T) {
	tests := []struct {
		name, protocol, body string
		privacy, safe        bool
		system               string
		max                  int64
		stop                 []string
		tool, choice         string
	}{
		{name: "invalid tool and forced choice", protocol: "anthropic", privacy: true, body: `{"messages":[{"role":"user","content":"person@example.test"}],"max_tokens":32,"tools":[{"name":"mcp__aws-sdk-v3__getObject","input_schema":{"type":"object"}}],"tool_choice":{"type":"tool","name":"mcp__aws-sdk-v3__getObject"}}`, max: 32},
		{name: "oversized tool", protocol: "anthropic", privacy: true, body: `{"messages":[{"role":"user","content":"person@example.test"}],"tools":[{"name":"` + strings.Repeat("a", 65) + `","input_schema":{"type":"object"}}]}`},
		{name: "missing schema", protocol: "anthropic", privacy: true, body: `{"messages":[{"role":"user","content":"person@example.test"}],"tools":[{"name":"lookup"}]}`},
		{name: "missing forced tool", protocol: "anthropic", privacy: true, body: `{"messages":[{"role":"user","content":"person@example.test"}],"tools":[{"name":"lookup","input_schema":{"type":"object"}}],"tool_choice":{"type":"tool","name":"absent"}}`, tool: "lookup"},
		{name: "safe tool and choice", protocol: "anthropic", privacy: true, safe: true, body: `{"messages":[{"role":"user","content":"person@example.test"}],"max_tokens":32,"tools":[{"name":"mcp__aws_sdk_v3__getObject","input_schema":{"type":"object"}}],"tool_choice":{"type":"tool","name":"mcp__aws_sdk_v3__getObject"}}`, max: 32, tool: "mcp__aws_sdk_v3__getObject", choice: "mcp__aws_sdk_v3__getObject"},
		{name: "OpenAI system", protocol: "openai", body: `{"messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hello"}]}`},
		{name: "OpenAI developer", protocol: "openai", body: `{"messages":[{"role":"developer","content":"be brief"},{"role":"user","content":"hello"}]}`},
		{name: "OpenAI output limit", protocol: "openai", body: `{"messages":[{"role":"user","content":"hello"}],"max_completion_tokens":32}`},
		{name: "OpenAI stop", protocol: "openai", body: `{"messages":[{"role":"user","content":"hello"}],"stop":["END"]}`},
		{name: "OpenAI combined overlay case", protocol: "openai", body: `{"messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hello"}],"max_completion_tokens":32,"stop":["END"]}`},
		{name: "OpenAI privacy ceiling", protocol: "openai", privacy: true, body: `{"messages":[{"role":"system","content":"be brief"},{"role":"user","content":"person@example.test"}],"max_completion_tokens":32,"stop":["END"]}`},
		{name: "safe OpenAI text and legacy max_tokens", protocol: "openai", safe: true, body: `{"messages":[{"role":"user","content":"hello"}],"max_tokens":32}`, max: 32},
		{name: "safe Anthropic system budget stop", protocol: "anthropic", privacy: true, safe: true, body: `{"system":"be brief","messages":[{"role":"user","content":"person@example.test"}],"max_tokens":32,"stop_sequences":["END"]}`, system: "be brief", max: 32, stop: []string{"END"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := []byte(tt.body)
			before := bytes.Clone(raw)
			// These expectations independently establish what the real converter keeps
			// and loses, including all three safe controls. No Name-only provider mock.
			verify := func(cr ConverseRequest) {
				t.Helper()
				if cr.System != tt.system {
					t.Fatalf("converter system=%q, want %q", cr.System, tt.system)
				}
				max, _ := cr.Inference["maxTokens"].(int64)
				if max != tt.max {
					t.Fatalf("converter max=%d, want %d", max, tt.max)
				}
				stop, _ := cr.Inference["stopSequences"].([]string)
				if !reflect.DeepEqual(stop, tt.stop) {
					t.Fatalf("converter stop=%v want %v", stop, tt.stop)
				}
				if tt.tool == "" {
					if len(cr.Tools) != 0 {
						t.Fatalf("converter unexpectedly retained tools: %+v", cr.Tools)
					}
				} else if len(cr.Tools) != 1 || cr.Tools[0].Name != tt.tool {
					t.Fatalf("converter tools=%+v", cr.Tools)
				}
				if tt.choice == "" {
					if cr.ToolChoice.Type != "" {
						t.Fatalf("converter choice=%+v", cr.ToolChoice)
					}
				} else if cr.ToolChoice.Type != "tool" || cr.ToolChoice.Name != tt.choice {
					t.Fatalf("converter choice=%+v", cr.ToolChoice)
				}
			}
			converted, err := toConverseRequest(raw)
			if err != nil {
				t.Fatal(err)
			}
			verify(converted)
			cfg := &config.Config{
				Providers: map[string]config.ProviderConfig{"safe": {Type: "task3-safe-contract", DataBoundary: "external"}, "private": {Type: "task3-converse-contract", DataBoundary: "internal"}},
				Models:    map[string]config.ModelConfig{"premium": {Targets: []config.Target{{Provider: "safe", Model: "premium"}}}, "private": {Targets: []config.Target{{Provider: "private", Model: "routing-contract-model"}}, ContextWindow: 10000, Capabilities: []string{"tools"}}},
				Pricing:   config.PricingConfig{Overrides: map[string]map[string]config.RateConfig{"safe": {"premium": {Free: true}}, "private": {"routing-contract-model": {Free: true}}}},
			}
			st, _, err := live.BuildState(cfg)
			if err != nil {
				t.Fatal(err)
			}
			holder := &live.Holder{}
			holder.Swap(st)
			r := router.New(holder)
			doc := &policy.Policy{Name: "contract", Rules: []policy.Rule{{Name: "context", Routing: &policy.Routing{Context: &policy.Context{Mode: v1alpha1.Enforce, FromModels: []string{"premium"}, SimpleModel: "private", ComplexModel: "private", MaxSimpleInputTokens: 10000}}}}}
			if tt.privacy {
				doc.Rules = []policy.Rule{{Name: "privacy", SensitiveData: &policy.SensitiveData{OnDetected: v1alpha1.InternalOnly, OnUninspectable: v1alpha1.InternalOnly, InternalModels: []string{"private"}}}}
			}
			r.SetRoutingPolicyLookup(func(string, string) ([]*policy.Policy, error) { return []*policy.Policy{doc}, nil })
			chain, _, err := r.ResolveChain("premium")
			if err != nil {
				t.Fatal(err)
			}
			in := router.RequestRoutingInput{Principal: keystore.Principal{Team: "t", AllowedModels: []string{"*"}}, RequestedModel: "premium", Model: "premium", Protocol: tt.protocol, RawBody: raw, State: st, Chain: chain}
			got, err := r.RouteRequest(context.Background(), in)
			if !bytes.Equal(raw, before) {
				t.Fatal("routing mutated original bytes")
			}
			if !tt.safe {
				if tt.privacy {
					if err == nil || len(got.Chain) != 0 {
						t.Fatalf("privacy admitted lossy Converse: %+v %v", got, err)
					}
				} else if err != nil || got.Model != "premium" || got.Decision.Reason != "context_unavailable" {
					t.Fatalf("context changed safe route: %+v %v", got, err)
				}
				return
			}
			if err != nil || got.Model != "private" || len(got.Chain) != 1 {
				t.Fatalf("safe converter control refused: %+v %v", got, err)
			}
			ct := got.Chain[0]
			_, err = ct.Provider.Complete(context.Background(), &providers.ProxyRequest{RawBody: raw, Upstream: ct.Upstream, IngressProtocol: tt.protocol})
			if err != nil {
				t.Fatal(err)
			}
			cp := ct.Provider.(*routingContractProvider)
			recorded := cp.conv.(*fakeConverser)
			if recorded.gotModelID != "routing-contract-model" {
				t.Fatal("control never reached actual Converse transport")
			}
			verify(recorded.gotReq)
		})
	}
}
