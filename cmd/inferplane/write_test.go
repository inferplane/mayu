package main

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/inferplane/inferplane/internal/providerstore"
	"github.com/inferplane/inferplane/internal/server/configapi"
)

// newServedPstoreGateway is newPstoreGateway plus a live accept loop — needed
// for a real Authorization-header round-trip rather than just ResolveChain/DB
// state. It must NOT also register newPstoreGateway's manual
// store/aud/pstore.Close() cleanup: g.serve's own shutdown already closes all
// three (gateway.go), so stacking both panics on a double-close of the audit
// writer's channel.
func newServedPstoreGateway(t *testing.T) (*gateway, *anthropicUpstream) {
	t.Helper()
	up := newAnthropicUpstream(t)
	t.Setenv("E2E_UPSTREAM_KEY", e2eUpstreamKey)
	t.Setenv("E2E_ADMIN_TOKEN", e2eAdminToken)
	dir := t.TempDir()
	cfgPath := dir + "/config.json"
	rewriteConfig(t, cfgPath, pstoreConfig(dir, up.srv.URL))
	g, err := newGateway(cfgPath)
	if err != nil {
		t.Fatalf("newGateway: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- g.serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
	})
	return g, up
}

// pstoreConfig is reloadBaseConfig plus a provider_store block, so newGateway
// seeds the DB from the file topology and the DB becomes authoritative.
func pstoreConfig(dir, upstreamURL string) string {
	return `{
	  "server": {"listen":"127.0.0.1:0","admin_listen":"127.0.0.1:0","drain_grace":"2s",
	    "admin_auth":{"token_refs":[{"env":"E2E_ADMIN_TOKEN"}]}},
	  "key_store":{"type":"sqlite","path":"` + dir + `/keys.db"},
	  "provider_store":{"type":"sqlite","path":"` + dir + `/providers.db"},
	  "audit":{"buffer":{"path":"` + dir + `/audit.wal"},"sinks":[{"type":"stdout"}]},
	  "providers":{"up":{"type":"anthropic","base_url":"` + upstreamURL + `","api_key_ref":{"env":"E2E_UPSTREAM_KEY"}}},
	  "models":{"m":{"targets":[{"provider":"up","model":"m"}]}},
	  "pricing":{"on_missing":"allow"}
	}`
}

func newPstoreGateway(t *testing.T) *gateway {
	t.Helper()
	up := newAnthropicUpstream(t)
	t.Setenv("E2E_UPSTREAM_KEY", e2eUpstreamKey)
	t.Setenv("E2E_ADMIN_TOKEN", e2eAdminToken)
	dir := t.TempDir()
	cfgPath := dir + "/config.json"
	rewriteConfig(t, cfgPath, pstoreConfig(dir, up.srv.URL))
	g, err := newGateway(cfgPath)
	if err != nil {
		t.Fatalf("newGateway: %v", err)
	}
	t.Cleanup(func() { g.store.Close(); g.aud.Close(); g.pstore.Close() })
	return g
}

// TestWriteProviderAndModelAppliesTopology: a UI write registers a provider and
// a route; ResolveChain sees the new generation (build-once-swap-once).
func TestWriteProviderAndModelAppliesTopology(t *testing.T) {
	g := newPstoreGateway(t)
	ctx := context.Background()

	// m2 does not resolve yet.
	if _, _, err := g.router.ResolveChain("m2"); err == nil {
		t.Fatal("m2 should not resolve before the write")
	}
	if err := g.WriteProvider(ctx, providerstore.ProviderRow{
		Name: "up2", Type: "anthropic", BaseURL: "https://up2.example", APIKeyRefEnv: "E2E_UPSTREAM_KEY",
	}); err != nil {
		t.Fatalf("WriteProvider: %v", err)
	}
	if err := g.WriteModel(ctx, "m2", providerstore.ModelRoute{Targets: []providerstore.Target{{Provider: "up2", Model: "m2up"}}}); err != nil {
		t.Fatalf("WriteModel: %v", err)
	}
	chain, _, err := g.router.ResolveChain("m2")
	if err != nil || len(chain) != 1 || chain[0].Upstream != "m2up" {
		t.Fatalf("m2 after write: %v %+v", err, chain)
	}
	// Persisted in the DB.
	if _, err := g.pstore.GetProvider(ctx, "up2"); err != nil {
		t.Fatalf("up2 not persisted: %v", err)
	}
}

// TestWriteProviderAuthHeaderBearerWiredToUpstream pins PR #13 review Finding
// 2: a provider registered through the write API with auth_header:"bearer"
// must actually send Authorization: Bearer <key> upstream (and NOT
// x-api-key) — exercising the full write -> ResolveProviders -> BuildState ->
// live.State path, not just the DTO/DB layers TestParseProviderWriteCarries
// AuthHeader and the providerstore/config unit tests already cover.
func TestWriteProviderAuthHeaderBearerWiredToUpstream(t *testing.T) {
	g, up := newServedPstoreGateway(t)
	ctx := context.Background()

	if err := g.WriteProvider(ctx, providerstore.ProviderRow{
		Name: "or", Type: "anthropic", BaseURL: up.srv.URL, APIKeyRefEnv: "E2E_UPSTREAM_KEY", AuthHeader: "bearer",
	}); err != nil {
		t.Fatalf("WriteProvider: %v", err)
	}
	if err := g.WriteModel(ctx, "m-bearer", providerstore.ModelRoute{Targets: []providerstore.Target{{Provider: "or", Model: "claude-test"}}}); err != nil {
		t.Fatalf("WriteModel: %v", err)
	}

	_, virtualKey := createKey(t, "http://"+g.AdminAddr(), "team", []string{"*"})
	resp := postMessages(t, "http://"+g.DataAddr(), virtualKey, "m-bearer")
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("POST /v1/messages: status %d", resp.StatusCode)
	}
	if up.lastAuthorize != "Bearer "+e2eUpstreamKey {
		t.Fatalf("Authorization header = %q, want %q", up.lastAuthorize, "Bearer "+e2eUpstreamKey)
	}
	if up.apiKey() != "" {
		t.Fatalf("x-api-key must NOT be sent when auth_header is bearer, got %q", up.apiKey())
	}
}

// TestWriteInvalidTopologyNothingPersisted: a route to a missing provider is
// rejected (ErrInvalidTopology) and nothing is persisted (build-once-swap-once).
func TestWriteInvalidTopologyNothingPersisted(t *testing.T) {
	g := newPstoreGateway(t)
	ctx := context.Background()
	err := g.WriteModel(ctx, "bad", providerstore.ModelRoute{Targets: []providerstore.Target{{Provider: "ghost", Model: "x"}}})
	if !errors.Is(err, configapi.ErrInvalidTopology) {
		t.Fatalf("route to missing provider = %v, want ErrInvalidTopology", err)
	}
	models, _ := g.pstore.ListModels(ctx)
	if _, ok := models["bad"]; ok {
		t.Fatal("invalid model route must NOT be persisted")
	}
}

// TestWriteModelAliasCollisionRejected (ADR-021 follow-up): a UI-written
// model's alias colliding with an existing model NAME is rejected at
// writeMutation (config.ValidateModelAliases on the candidate effective
// config), matching the file-config path's validateModelAliases guard —
// nothing is persisted on rejection.
func TestWriteModelAliasCollisionRejected(t *testing.T) {
	g := newPstoreGateway(t)
	ctx := context.Background()
	// pstoreConfig seeds model "m" — an alias equal to "m" must collide.
	err := g.WriteModel(ctx, "n", providerstore.ModelRoute{
		Aliases: []string{"m"},
		Targets: []providerstore.Target{{Provider: "up", Model: "x"}},
	})
	if !errors.Is(err, configapi.ErrInvalidTopology) {
		t.Fatalf("alias colliding with a model name = %v, want ErrInvalidTopology", err)
	}
	models, _ := g.pstore.ListModels(ctx)
	if _, ok := models["n"]; ok {
		t.Fatal("model with a colliding alias must NOT be persisted")
	}
}

// TestWriteModelAliasAppliesTopology: a UI write registers a model alias, and
// the alias resolves to the canonical model through the live topology
// (Router.Canonical), the same path a config-file alias takes.
func TestWriteModelAliasAppliesTopology(t *testing.T) {
	g := newPstoreGateway(t)
	ctx := context.Background()
	if err := g.WriteModel(ctx, "m2", providerstore.ModelRoute{
		Aliases: []string{"apac.m2"},
		Targets: []providerstore.Target{{Provider: "up", Model: "m2up"}},
	}); err != nil {
		t.Fatalf("WriteModel: %v", err)
	}
	if got := g.router.Canonical("apac.m2"); got != "m2" {
		t.Fatalf("Canonical(apac.m2) = %q, want %q", got, "m2")
	}
}

// TestDeleteProviderWithLiveRouteRejected pins gate G3: deleting a provider a
// model still routes to is rejected by the candidate BuildState; the provider
// stays in the store.
func TestDeleteProviderWithLiveRouteRejected(t *testing.T) {
	g := newPstoreGateway(t)
	ctx := context.Background()
	// "up" is referenced by the seeded model "m".
	err := g.DeleteProvider(ctx, "up")
	if !errors.Is(err, configapi.ErrInvalidTopology) {
		t.Fatalf("delete provider with live route = %v, want ErrInvalidTopology", err)
	}
	if _, err := g.pstore.GetProvider(ctx, "up"); err != nil {
		t.Fatalf("provider must remain after a rejected delete: %v", err)
	}
}

// TestWriteFileRefNonexistentRejected pins the round-2 MINOR: a file ref to a
// path that is not a readable file is rejected at validation (resolve reads it),
// so a path-shaped value never reaches the DB.
func TestWriteFileRefNonexistentRejected(t *testing.T) {
	g := newPstoreGateway(t)
	ctx := context.Background()
	err := g.WriteProvider(ctx, providerstore.ProviderRow{
		Name: "p", Type: "anthropic", APIKeyRefFile: "/nonexistent/secret/path",
	})
	if !errors.Is(err, configapi.ErrInvalidTopology) {
		t.Fatalf("unreadable file ref = %v, want ErrInvalidTopology", err)
	}
	if _, err := g.pstore.GetProvider(ctx, "p"); !errors.Is(err, providerstore.ErrNotFound) {
		t.Fatal("a provider whose ref does not resolve must NOT be persisted")
	}
}

// TestWriteVsReloadSerialized pins gate C3: concurrent reload() (SIGHUP path)
// and UI writes funnel through one reloadMu — no deadlock, race-clean.
func TestWriteVsReloadSerialized(t *testing.T) {
	g := newPstoreGateway(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); _ = g.reload() }()
		go func() {
			defer wg.Done()
			_ = g.WriteProvider(ctx, providerstore.ProviderRow{Name: "up", Type: "anthropic", BaseURL: "https://x", APIKeyRefEnv: "E2E_UPSTREAM_KEY"})
		}()
	}
	wg.Wait()
}

// TestWriteKeepsStatefulComponents: a UI write swaps the topology generation but
// never rebuilds the keystore or audit writer (ADR-006 invariant).
func TestWriteKeepsStatefulComponents(t *testing.T) {
	g := newPstoreGateway(t)
	ctx := context.Background()
	store0, aud0 := g.store, g.aud
	gen0 := g.holder.Load()
	if err := g.WriteProvider(ctx, providerstore.ProviderRow{Name: "up3", Type: "anthropic", BaseURL: "https://x", APIKeyRefEnv: "E2E_UPSTREAM_KEY"}); err != nil {
		t.Fatal(err)
	}
	if g.store != store0 || g.aud != aud0 {
		t.Fatal("UI write must not rebuild the keystore or audit writer")
	}
	if g.holder.Load() == gen0 {
		t.Fatal("UI write must publish a new topology generation")
	}
}

// TestProviderStoreUnsupportedType pins the P4 MINOR: an unimplemented backend
// type is rejected at boot rather than silently using SQLite.
func TestProviderStoreUnsupportedType(t *testing.T) {
	t.Setenv("E2E_ADMIN_TOKEN", e2eAdminToken)
	dir := t.TempDir()
	cfgPath := dir + "/config.json"
	rewriteConfig(t, cfgPath, `{
	  "server": {"listen":"127.0.0.1:0","admin_listen":"127.0.0.1:0","admin_auth":{"token_refs":[{"env":"E2E_ADMIN_TOKEN"}]}},
	  "key_store":{"type":"sqlite","path":"`+dir+`/keys.db"},
	  "provider_store":{"type":"postgres","path":"`+dir+`/p.db"},
	  "audit":{"buffer":{"path":"`+dir+`/audit.wal"},"sinks":[{"type":"stdout"}]}
	}`)
	if _, err := newGateway(cfgPath); err == nil {
		t.Fatal("unsupported provider_store.type must be rejected at boot")
	}
}

// TestOTelInitNonFatal pins ADR-011 T4: an otel block pointing at an unreachable
// collector must NOT fail boot (tracing is best-effort, exporter connects lazily).
func TestOTelInitNonFatal(t *testing.T) {
	t.Setenv("E2E_ADMIN_TOKEN", e2eAdminToken)
	dir := t.TempDir()
	cfgPath := dir + "/config.json"
	rewriteConfig(t, cfgPath, `{
	  "server": {"listen":"127.0.0.1:0","admin_listen":"127.0.0.1:0","admin_auth":{"token_refs":[{"env":"E2E_ADMIN_TOKEN"}]}},
	  "key_store":{"type":"sqlite","path":"`+dir+`/keys.db"},
	  "audit":{"buffer":{"path":"`+dir+`/audit.wal"},"sinks":[{"type":"stdout"}]},
	  "otel":{"endpoint":"127.0.0.1:1","protocol":"http","insecure":true}
	}`)
	g, err := newGateway(cfgPath)
	if err != nil {
		t.Fatalf("otel config with unreachable collector must not fail boot: %v", err)
	}
	if g.otelDown == nil {
		t.Fatal("otelDown should be set when otel is configured")
	}
	g.store.Close()
	g.aud.Close()
}
