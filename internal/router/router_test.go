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
