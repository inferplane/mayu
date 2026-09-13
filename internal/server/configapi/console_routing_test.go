package configapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/inferplane/inferplane/api/v1alpha1"
	"github.com/inferplane/inferplane/internal/config"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/live"
	"github.com/inferplane/inferplane/internal/policy"
	"github.com/inferplane/inferplane/internal/providerstore"
	"github.com/inferplane/inferplane/internal/router"
	_ "github.com/inferplane/inferplane/providers/anthropic"
)

// Only adapts Writer's method names to the real SQLite store. Payload parsing,
// replacement semantics, readback, live topology construction and routing are
// production code. No listener or upstream request is needed.
type consoleStoreWriter struct{ *providerstore.SQLiteStore }

func (w consoleStoreWriter) WriteProvider(ctx context.Context, p providerstore.ProviderRow) error {
	return w.UpsertProvider(ctx, p)
}
func (w consoleStoreWriter) WriteModel(ctx context.Context, name string, m providerstore.ModelRoute) error {
	return w.SetModel(ctx, name, m)
}

func TestConsoleReplacementPayloadKeepsRoutingUsable(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("console payload regression needs Node standard library (no npm packages)")
	}
	output, err := exec.Command(node, "../adminui/testdata/routing_forms.cjs", "--payloads").CombinedOutput()
	if err != nil {
		t.Fatalf("run shipped console handlers: %v\n%s", err, output)
	}
	var payloads map[string]struct {
		Path string          `json:"path"`
		Body json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal(output, &payloads); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	store, err := providerstore.OpenSQLite(filepath.Join(t.TempDir(), "topology.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"public": {Type: "anthropic", BaseURL: "https://public.invalid", DataBoundary: "external"},
			"private": {Type: "anthropic", BaseURL: "https://private.invalid", DataBoundary: "internal",
				Region: "us-east-1", APIKeyRef: &config.SecretRef{Env: "PRIVATE_KEY"}, AuthHeader: "bearer"},
		},
		Models: map[string]config.ModelConfig{
			"public": {Targets: []config.Target{{Provider: "public", Model: "upstream"}}},
			"private": {Aliases: []string{"private-alias"}, ContextWindow: 32768,
				Capabilities: []string{"tools", "vision", "reasoning", "structured_output"},
				Targets:      []config.Target{{Provider: "private", Model: "upstream", API: "invoke_model"}, {Provider: "private", Model: "backup"}}},
		},
		Pricing: config.PricingConfig{Overrides: map[string]map[string]config.RateConfig{
			"public": {"upstream": {Free: true}}, "private": {"upstream": {Free: true}, "backup": {Free: true}},
		}},
	}
	if err := providerstore.SeedIfEmpty(ctx, store, cfg); err != nil {
		t.Fatal(err)
	}
	for _, resource := range []string{"provider", "model"} {
		p := payloads[resource]
		rec := httptest.NewRecorder()
		WriteHandler(resource+"s", consoleStoreWriter{store}, nil).ServeHTTP(rec, httptest.NewRequest(http.MethodPut, p.Path, bytes.NewReader(p.Body)))
		if rec.Code != http.StatusNoContent {
			t.Fatalf("%s PUT: %d %s", resource, rec.Code, rec.Body)
		}
	}
	effective, err := providerstore.Overlay(cfg, store)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	Handler(func() View { return ViewFrom(effective.Providers, effective.Models) }).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/config", nil))
	var view View
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	p, m := view.Providers[0], view.Models[0] // sorted: private before public
	if p.DataBoundary != "internal" || p.Auth != "bearer · env:PRIVATE_KEY" || p.Region != "us-east-1" {
		t.Errorf("provider readback lost edit state: %+v", p)
	}
	if m.ContextWindow != 32768 || !slices.Equal(m.Capabilities, []string{"tools", "vision", "reasoning", "structured_output"}) || !slices.Equal(m.Aliases, []string{"private-alias"}) || len(m.Targets) != 2 || m.Targets[0].API != "invoke_model" {
		t.Errorf("model readback lost edit state: %+v", m)
	}
	st, _, err := live.BuildState(effective)
	if err != nil {
		t.Fatal(err)
	}
	holder := &live.Holder{}
	holder.Swap(st)
	r := router.New(holder)
	r.SetRoutingPolicyLookup(func(string, string) ([]*policy.Policy, error) {
		return []*policy.Policy{{Name: "privacy", Rules: []policy.Rule{{
			Name: "internal", SensitiveData: &policy.SensitiveData{
				OnDetected: v1alpha1.InternalOnly, OnUninspectable: v1alpha1.Block,
				InternalModels: []string{"private-alias"},
			},
		}}}}, nil
	})
	chain, _, err := r.ResolveChain("public")
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.RouteRequest(ctx, router.RequestRoutingInput{
		Principal:      keystore.Principal{Team: "engineering", AllowedModels: []string{"*"}},
		RequestedModel: "public", Model: "public", Protocol: "anthropic", State: st, Chain: chain,
		RawBody: []byte(`{"messages":[{"role":"user","content":"person@example.test"}],"max_tokens":32}`),
	})
	if err != nil || got.Model != "private" || len(got.Chain) != 2 {
		t.Fatalf("console save broke protected routing: %+v, %v", got, err)
	}
	for _, target := range got.Chain {
		if target.DataBoundary != "internal" || target.ProviderName != "private" {
			t.Fatalf("unsafe retry after console save: %+v", target)
		}
	}
}
