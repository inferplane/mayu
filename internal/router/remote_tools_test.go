package router

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/inferplane/inferplane/providers"
	"github.com/inferplane/inferplane/providers/testing/mockprovider"
)

type remoteToolsProviderSpy struct {
	providers.Provider
	calls atomic.Int64
}

func (*remoteToolsProviderSpy) Name() string { return "openai_responses" }
func (*remoteToolsProviderSpy) SupportsIngress(protocol string) bool {
	return protocol == "responses"
}
func (p *remoteToolsProviderSpy) Complete(ctx context.Context, req *providers.ProxyRequest) (*providers.ProxyResponse, error) {
	p.calls.Add(1)
	return p.Provider.Complete(ctx, req)
}

func init() {
	providers.Register("request-routing-remote-tools-spy", func(providers.Config) (providers.Provider, error) {
		return &remoteToolsProviderSpy{Provider: mockprovider.New("upstream")}, nil
	})
}

func TestInternalPrivacyRefusesRemoteToolsButPreservesOpaqueImages(t *testing.T) {
	for _, tt := range []struct {
		name, raw     string
		privacy, deny bool
	}{
		{"remote MCP", `{"model":"premium","input":"hello","tools":[{"type":"mcp","server_url":"https://remote.example.test"}]}`, true, true},
		{"nested remote MCP", `{"model":"premium","input":"hello","tools":[{"type":"namespace","name":"tools","tools":[{"type":"mcp","server_url":"https://remote.example.test"}]}]}`, true, true},
		{"hosted tool", `{"model":"premium","input":"hello","tools":[{"type":"web_search"}]}`, true, true},
		{"opaque image", `{"model":"premium","input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,AA=="}]}]}`, true, false},
		{"no privacy restriction", `{"model":"premium","input":"hello","tools":[{"type":"mcp","server_url":"https://remote.example.test"}]}`, false, false},
	} {
		for _, count := range []bool{false, true} {
			t.Run(tt.name+map[bool]string{false: "/generation", true: "/count"}[count], func(t *testing.T) {
				cfg := routingConfig()
				for name, provider := range cfg.Providers {
					provider.Type = "request-routing-remote-tools-spy"
					cfg.Providers[name] = provider
				}
				for name, model := range cfg.Models {
					model.Capabilities = []string{"tools", "vision"}
					cfg.Models[name] = model
				}
				r, in := routingSetup(t, cfg)
				in.Protocol, in.RawBody, in.CountOnly = "responses", []byte(tt.raw), count
				in.Compatible = func(ChainTarget) bool { return true }
				if tt.privacy {
					installRoutingPolicies(r, privatePolicy("privacy", "private"))
				}
				got, err := r.RouteRequest(context.Background(), in)
				// Exercise every advertised target against a local spy. A
				// forbidden chain must be empty even if a caller ignores err.
				for _, target := range got.Chain {
					_, callErr := target.Provider.Complete(context.Background(), &providers.ProxyRequest{
						Model: target.Model, Upstream: target.Upstream, RawBody: in.RawBody, IngressProtocol: in.Protocol,
					})
					if callErr != nil {
						t.Fatal(callErr)
					}
				}
				var calls int64
				for name := range cfg.Providers {
					provider, _ := in.State.Provider(name)
					calls += provider.(*remoteToolsProviderSpy).calls.Load()
				}
				if tt.deny {
					if calls != 0 {
						t.Fatalf("remote-tool request advertised an egress chain: %d provider calls", calls)
					}
					requireDenied(t, got, err, "no_safe_route")
				} else {
					if err != nil || calls == 0 {
						t.Fatalf("existing allowed native behavior changed: %+v %v", got, err)
					}
					if tt.privacy {
						for _, target := range got.Chain {
							if target.DataBoundary != "internal" || target.Model != "private" {
								t.Fatalf("opaque image escaped internal constraint: %+v", target)
							}
						}
					}
				}
			})
		}
	}
}
