package main

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/inferplane/inferplane/internal/pricing"
)

// rewriteConfig overwrites the gateway's config file with new JSON (used to
// simulate an operator editing config then sending SIGHUP).
func rewriteConfig(t *testing.T, path, json string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(json), 0o600); err != nil {
		t.Fatal(err)
	}
}

// twoProviderConfig routes model "m" to an httptest upstream via provider "up";
// extra is spliced into providers/models so a reload can add routes.
func reloadBaseConfig(dir, upstreamURL string) string {
	return `{
	  "server": {"listen":"127.0.0.1:0","admin_listen":"127.0.0.1:0","drain_grace":"2s",
	    "admin_auth":{"token_refs":[{"env":"E2E_ADMIN_TOKEN"}]}},
	  "key_store":{"type":"sqlite","path":"` + dir + `/keys.db"},
	  "audit":{"buffer":{"path":"` + dir + `/audit.wal"},"sinks":[{"type":"stdout"}]},
	  "providers":{"up":{"type":"anthropic","base_url":"` + upstreamURL + `","api_key_ref":{"env":"E2E_UPSTREAM_KEY"}}},
	  "models":{"m":{"targets":[{"provider":"up","model":"m"}]}}
	}`
}

func TestReloadAppliesNewGeneration(t *testing.T) {
	up := newAnthropicUpstream(t)
	t.Setenv("E2E_UPSTREAM_KEY", e2eUpstreamKey)
	t.Setenv("E2E_ADMIN_TOKEN", e2eAdminToken)
	dir := t.TempDir()
	cfgPath := dir + "/config.json"
	rewriteConfig(t, cfgPath, reloadBaseConfig(dir, up.srv.URL))

	g, err := newGateway(cfgPath)
	if err != nil {
		t.Fatalf("newGateway: %v", err)
	}
	// Before reload: model "m2" does not resolve.
	if _, _, err := g.router.ResolveChain("m2"); err == nil {
		t.Fatal("m2 should not resolve before reload")
	}
	// Operator adds a second model route + a pricing override, then reloads.
	rewriteConfig(t, cfgPath, `{
	  "server": {"listen":"127.0.0.1:0","admin_listen":"127.0.0.1:0","drain_grace":"2s",
	    "admin_auth":{"token_refs":[{"env":"E2E_ADMIN_TOKEN"}]}},
	  "key_store":{"type":"sqlite","path":"`+dir+`/keys.db"},
	  "audit":{"buffer":{"path":"`+dir+`/audit.wal"},"sinks":[{"type":"stdout"}]},
	  "providers":{"up":{"type":"anthropic","base_url":"`+up.srv.URL+`","api_key_ref":{"env":"E2E_UPSTREAM_KEY"}}},
	  "models":{
	    "m":{"targets":[{"provider":"up","model":"m"}]},
	    "m2":{"targets":[{"provider":"up","model":"m2up"}]}
	  },
	  "pricing":{"on_missing":"allow","overrides":{"up":{"m2up":{"input_per_mtok":3.0}}}}
	}`)
	if err := g.reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	// After reload: m2 resolves, and the pricing table carries the new rate.
	chain, st, err := g.router.ResolveChain("m2")
	if err != nil || len(chain) != 1 || chain[0].Upstream != "m2up" {
		t.Fatalf("m2 after reload: %v %+v", err, chain)
	}
	if cost, _ := st.Pricing().CostUSDMicros("up", "m2up", pricing.Usage{Input: 1_000_000}); cost != 3_000_000 {
		t.Fatalf("new pricing not live after reload: cost=%d", cost)
	}
}

func TestReloadRollsBackOnBadConfig(t *testing.T) {
	up := newAnthropicUpstream(t)
	t.Setenv("E2E_UPSTREAM_KEY", e2eUpstreamKey)
	t.Setenv("E2E_ADMIN_TOKEN", e2eAdminToken)
	dir := t.TempDir()
	cfgPath := dir + "/config.json"
	rewriteConfig(t, cfgPath, reloadBaseConfig(dir, up.srv.URL))
	g, err := newGateway(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	// route → missing provider (loads, but BuildState validation rejects it).
	rewriteConfig(t, cfgPath, `{
	  "server": {"listen":"127.0.0.1:0","admin_listen":"127.0.0.1:0",
	    "admin_auth":{"token_refs":[{"env":"E2E_ADMIN_TOKEN"}]}},
	  "key_store":{"type":"sqlite","path":"`+dir+`/keys.db"},
	  "audit":{"buffer":{"path":"`+dir+`/audit.wal"},"sinks":[{"type":"stdout"}]},
	  "providers":{"up":{"type":"anthropic","base_url":"`+up.srv.URL+`","api_key_ref":{"env":"E2E_UPSTREAM_KEY"}}},
	  "models":{"m":{"targets":[{"provider":"ghost","model":"m"}]}}
	}`)
	if err := g.reload(); err == nil {
		t.Fatal("reload with route→missing provider must error")
	}
	// Old generation still serves: model "m" still resolves to "up".
	if _, _, err := g.router.ResolveChain("m"); err != nil {
		t.Fatalf("old generation lost after failed reload: %v", err)
	}

	// Unparseable config also rolls back.
	rewriteConfig(t, cfgPath, `{ not valid json`)
	if err := g.reload(); err == nil {
		t.Fatal("reload with unparseable config must error")
	}
	if _, _, err := g.router.ResolveChain("m"); err != nil {
		t.Fatalf("old generation lost after unparseable reload: %v", err)
	}
}

func TestReloadPreservesGovernanceAndStores(t *testing.T) {
	up := newAnthropicUpstream(t)
	t.Setenv("E2E_UPSTREAM_KEY", e2eUpstreamKey)
	t.Setenv("E2E_ADMIN_TOKEN", e2eAdminToken)
	dir := t.TempDir()
	cfgPath := dir + "/config.json"
	rewriteConfig(t, cfgPath, reloadBaseConfig(dir, up.srv.URL))
	g, err := newGateway(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	store, aud := g.store, g.aud
	if err := g.reload(); err != nil {
		t.Fatal(err)
	}
	// The stateful components are the SAME instances — reload rebuilt only the
	// topology generation, never the keystore/audit (counters/chain continuous).
	if g.store != store || g.aud != aud {
		t.Fatal("reload must not rebuild the keystore or audit writer")
	}
}

func TestConcurrentReloadsSerialize(t *testing.T) {
	up := newAnthropicUpstream(t)
	t.Setenv("E2E_UPSTREAM_KEY", e2eUpstreamKey)
	t.Setenv("E2E_ADMIN_TOKEN", e2eAdminToken)
	dir := t.TempDir()
	cfgPath := dir + "/config.json"
	rewriteConfig(t, cfgPath, reloadBaseConfig(dir, up.srv.URL))
	g, err := newGateway(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func() {
			for j := 0; j < 20; j++ {
				_ = g.reload()
				_, _, _ = g.router.ResolveChain("m")
			}
			done <- struct{}{}
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
	// Final state is consistent (m still resolves).
	if _, _, err := g.router.ResolveChain("m"); err != nil {
		t.Fatalf("inconsistent after concurrent reloads: %v", err)
	}
}

// TestReloadWorkerTriggerAndLifecycle: the worker reloads on a trigger and
// exits cleanly when its context is canceled (no goroutine leak).
func TestReloadWorkerTriggerAndLifecycle(t *testing.T) {
	up := newAnthropicUpstream(t)
	t.Setenv("E2E_UPSTREAM_KEY", e2eUpstreamKey)
	t.Setenv("E2E_ADMIN_TOKEN", e2eAdminToken)
	dir := t.TempDir()
	cfgPath := dir + "/config.json"
	rewriteConfig(t, cfgPath, reloadBaseConfig(dir, up.srv.URL))
	g, err := newGateway(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	trigger := make(chan os.Signal, 1)
	done := make(chan struct{})
	go g.reloadWorker(ctx, trigger, done)

	// Add a route, fire the trigger, and the worker reloads it.
	rewriteConfig(t, cfgPath, `{
	  "server": {"listen":"127.0.0.1:0","admin_listen":"127.0.0.1:0",
	    "admin_auth":{"token_refs":[{"env":"E2E_ADMIN_TOKEN"}]}},
	  "key_store":{"type":"sqlite","path":"`+dir+`/keys.db"},
	  "audit":{"buffer":{"path":"`+dir+`/audit.wal"},"sinks":[{"type":"stdout"}]},
	  "providers":{"up":{"type":"anthropic","base_url":"`+up.srv.URL+`","api_key_ref":{"env":"E2E_UPSTREAM_KEY"}}},
	  "models":{"m":{"targets":[{"provider":"up","model":"m"}]},"mX":{"targets":[{"provider":"up","model":"mX"}]}}
	}`)
	trigger <- syscall.SIGHUP
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, err := g.router.ResolveChain("mX"); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, _, err := g.router.ResolveChain("mX"); err != nil {
		t.Fatal("worker did not apply the triggered reload")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not exit on ctx cancel (goroutine leak)")
	}
}
