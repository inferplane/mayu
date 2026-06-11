package router

import (
	"testing"

	"github.com/inferplane/inferplane/internal/config"
	"github.com/inferplane/inferplane/providers"
	"github.com/inferplane/inferplane/providers/testing/mockprovider"
)

func TestResolveModel(t *testing.T) {
	provs := map[string]providers.Provider{"anthropic-direct": mockprovider.New("claude-sonnet-4-6")}
	models := map[string]config.ModelConfig{
		"claude-sonnet-4-6": {Targets: []config.Target{{Provider: "anthropic-direct", Model: "claude-sonnet-4-6"}}},
	}
	r := New(provs, models)
	p, upstream, err := r.Resolve("claude-sonnet-4-6")
	if err != nil {
		t.Fatal(err)
	}
	if p.Name() != "mock" || upstream != "claude-sonnet-4-6" {
		t.Fatalf("resolve wrong: %s %s", p.Name(), upstream)
	}
	if _, _, err := r.Resolve("unknown-model"); err == nil {
		t.Fatal("expected error for unknown model")
	}
}

func TestResolveChainSkipsOpenBreaker(t *testing.T) {
	provs := map[string]providers.Provider{
		"a": mockprovider.New("m"), "b": mockprovider.New("m"),
	}
	models := map[string]config.ModelConfig{
		"m": {Targets: []config.Target{{Provider: "a", Model: "m1"}, {Provider: "b", Model: "m2"}}},
	}
	r := New(provs, models)
	// trip provider "a" breaker (5 failures)
	for i := 0; i < 5; i++ {
		r.RecordResult("a", false)
	}
	chain, err := r.ResolveChain("m")
	if err != nil {
		t.Fatal(err)
	}
	// "a" open → chain should start with "b"
	if len(chain) == 0 || chain[0].ProviderName != "b" {
		t.Fatalf("open breaker not skipped: %+v", chain)
	}
}

func TestResolveUnknownProvider(t *testing.T) {
	// model maps to a provider key that isn't in the providers map (config drift).
	provs := map[string]providers.Provider{} // empty
	models := map[string]config.ModelConfig{
		"m": {Targets: []config.Target{{Provider: "ghost", Model: "m"}}},
	}
	r := New(provs, models)
	if _, _, err := r.Resolve("m"); err == nil {
		t.Fatal("expected error when target points at unknown provider")
	}
}
