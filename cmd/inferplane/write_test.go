package main

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/inferplane/inferplane/internal/providerstore"
	"github.com/inferplane/inferplane/internal/server/configapi"
)

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
	if err := g.WriteModel(ctx, "m2", []providerstore.Target{{Provider: "up2", Model: "m2up"}}); err != nil {
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

// TestWriteInvalidTopologyNothingPersisted: a route to a missing provider is
// rejected (ErrInvalidTopology) and nothing is persisted (build-once-swap-once).
func TestWriteInvalidTopologyNothingPersisted(t *testing.T) {
	g := newPstoreGateway(t)
	ctx := context.Background()
	err := g.WriteModel(ctx, "bad", []providerstore.Target{{Provider: "ghost", Model: "x"}})
	if !errors.Is(err, configapi.ErrInvalidTopology) {
		t.Fatalf("route to missing provider = %v, want ErrInvalidTopology", err)
	}
	models, _ := g.pstore.ListModels(ctx)
	if _, ok := models["bad"]; ok {
		t.Fatal("invalid model route must NOT be persisted")
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
