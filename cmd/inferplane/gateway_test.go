package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/inferplane/inferplane/internal/keystore"
)

// writeTestConfig writes a minimal valid gateway config into a temp dir and
// returns its path. mutate (optional) edits the config map before marshaling —
// tests add providers/models/teams on top of this skeleton. Secrets are only
// ever referenced via env refs (never inline), matching the §7 mandate.
func writeTestConfig(t *testing.T, mutate func(cfg map[string]any, dir string)) string {
	t.Helper()
	dir := t.TempDir()
	cfg := map[string]any{
		"server": map[string]any{
			"listen":       "127.0.0.1:0",
			"admin_listen": "127.0.0.1:0",
			"drain_grace":  "2s",
		},
		"key_store": map[string]any{"type": "sqlite", "path": filepath.Join(dir, "keys.db")},
		"audit": map[string]any{
			"buffer": map[string]any{"path": filepath.Join(dir, "audit.wal")},
			"sinks":  []any{map[string]any{"type": "file", "path": filepath.Join(dir, "audit.jsonl")}},
		},
	}
	if mutate != nil {
		mutate(cfg, dir)
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	p := filepath.Join(dir, "config.json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return p
}

// waitHTTP polls url until it returns wantStatus or the deadline passes.
func waitHTTP(t *testing.T, url string, wantStatus int) {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Second} // a hung listener must fail the poll, not the runner
	deadline := time.Now().Add(5 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == wantStatus {
				return
			}
			lastErr = fmt.Errorf("status %d (want %d)", resp.StatusCode, wantStatus)
		} else {
			lastErr = err
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("GET %s never returned %d: %v", url, wantStatus, lastErr)
}

func TestGatewayBootsAndShutsDown(t *testing.T) {
	cfgPath := writeTestConfig(t, nil)

	g, err := newGateway(cfgPath)
	if err != nil {
		t.Fatalf("newGateway: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- g.serve(ctx) }()

	// Admin plane: /healthz lives on the admin mux only.
	waitHTTP(t, "http://"+g.AdminAddr()+"/healthz", http.StatusOK)
	// Data plane: /v1/models without a key → 401 proves listener + auth stack.
	waitHTTP(t, "http://"+g.DataAddr()+"/v1/models", http.StatusUnauthorized)

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned error on shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("gateway did not shut down within drain grace")
	}
}

// authedGet issues an authenticated GET and returns the status code.
func authedGet(t *testing.T, url, key string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("x-api-key", key)
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// bootAndStop starts g, waits for the admin plane to be ready, then shuts it
// down and waits for serve to return — a helper so the store-wipe test can
// boot the SAME config twice against the same on-disk store path.
func bootAndStop(t *testing.T, g *gateway) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- g.serve(ctx) }()
	waitHTTP(t, "http://"+g.AdminAddr()+"/healthz", http.StatusOK)
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("gateway did not shut down within drain grace")
		}
	})
}

// ADR-023: declarative virtual keys. A key declared in config must be usable
// immediately at boot, and must remain usable across a wipe of the key-store
// file (the ephemeral-container restart scenario this feature exists for).

func TestGateway_BootstrapsDeclaredVirtualKey(t *testing.T) {
	t.Setenv("INFERPLANE_VKEY_BOOT", "sk-declarative-key-0123456789")
	cfgPath := writeTestConfig(t, func(cfg map[string]any, dir string) {
		cfg["teams"] = map[string]any{"demo": map[string]any{"allowed_models": []string{"*"}}}
		cfg["virtual_keys"] = []map[string]any{{
			"team": "demo", "key_ref": map[string]string{"env": "INFERPLANE_VKEY_BOOT"}, "allowed_models": []string{"*"},
		}}
	})
	g, err := newGateway(cfgPath)
	if err != nil {
		t.Fatalf("newGateway: %v", err)
	}
	bootAndStop(t, g)

	if got := authedGet(t, "http://"+g.DataAddr()+"/v1/models", "sk-declarative-key-0123456789"); got != http.StatusOK {
		t.Fatalf("declared virtual key did not authenticate: status %d", got)
	}
}

func TestGateway_DeclaredVirtualKeySurvivesStoreWipe(t *testing.T) {
	t.Setenv("INFERPLANE_VKEY_WIPE", "sk-declarative-key-0123456789")
	var keysDBPath string
	cfgPath := writeTestConfig(t, func(cfg map[string]any, dir string) {
		cfg["teams"] = map[string]any{"demo": map[string]any{"allowed_models": []string{"*"}}}
		cfg["virtual_keys"] = []map[string]any{{
			"team": "demo", "key_ref": map[string]string{"env": "INFERPLANE_VKEY_WIPE"}, "allowed_models": []string{"*"},
		}}
		keysDBPath = cfg["key_store"].(map[string]any)["path"].(string)
	})

	g1, err := newGateway(cfgPath)
	if err != nil {
		t.Fatalf("newGateway (boot 1): %v", err)
	}
	bootAndStop(t, g1)
	if got := authedGet(t, "http://"+g1.DataAddr()+"/v1/models", "sk-declarative-key-0123456789"); got != http.StatusOK {
		t.Fatalf("boot 1: declared key did not authenticate: status %d", got)
	}

	if err := os.Remove(keysDBPath); err != nil {
		t.Fatalf("wipe keys.db: %v", err)
	}

	g2, err := newGateway(cfgPath)
	if err != nil {
		t.Fatalf("newGateway (boot 2, after store wipe): %v", err)
	}
	bootAndStop(t, g2)
	if got := authedGet(t, "http://"+g2.DataAddr()+"/v1/models", "sk-declarative-key-0123456789"); got != http.StatusOK {
		t.Fatal("declared virtual key did not survive a wiped-and-recreated key store")
	}
}

func TestGateway_RejectsVirtualKeyForUnknownTeam(t *testing.T) {
	t.Setenv("INFERPLANE_VKEY_UNKNOWNTEAM", "sk-declarative-key-0123456789")
	cfgPath := writeTestConfig(t, func(cfg map[string]any, dir string) {
		cfg["virtual_keys"] = []map[string]any{{
			"team": "never-declared", "key_ref": map[string]string{"env": "INFERPLANE_VKEY_UNKNOWNTEAM"}, "allowed_models": []string{"*"},
		}}
	})
	if _, err := newGateway(cfgPath); err == nil {
		t.Fatal("a virtual key for a team that exists neither in cfg.Teams nor the key store must be rejected at boot")
	}
}

func TestGateway_AcceptsVirtualKeyForDBOnlyTeam(t *testing.T) {
	t.Setenv("INFERPLANE_VKEY_DBTEAM", "sk-declarative-key-0123456789")
	var keysDBPath string
	cfgPath := writeTestConfig(t, func(cfg map[string]any, dir string) {
		cfg["virtual_keys"] = []map[string]any{{
			"team": "db-only-team", "key_ref": map[string]string{"env": "INFERPLANE_VKEY_DBTEAM"}, "allowed_models": []string{"*"},
		}}
		keysDBPath = cfg["key_store"].(map[string]any)["path"].(string)
	})

	// Pre-create the team directly in the key store (as if via the admin API /
	// ADR-016 DB-authoritative path) — it is deliberately absent from cfg.Teams.
	pre, err := keystore.OpenSQLite(keysDBPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := pre.UpsertTeam(context.Background(), keystore.TeamRecord{Name: "db-only-team"}); err != nil {
		t.Fatal(err)
	}
	pre.Close()

	g, err := newGateway(cfgPath)
	if err != nil {
		t.Fatalf("a virtual key for a DB-only team (ADR-016) must be accepted: %v", err)
	}
	bootAndStop(t, g)
	if got := authedGet(t, "http://"+g.DataAddr()+"/v1/models", "sk-declarative-key-0123456789"); got != http.StatusOK {
		t.Fatal("declared virtual key for a DB-only team did not authenticate")
	}
}

// TestGateway_RevokedDeclaredKeyStaysRevokedAcrossReboot pins an H4 code-gate
// finding: EnsureKey's upsert never un-revokes a row (decision #2), so a
// declared key that was revoked out-of-band must still fail to authenticate
// after a reboot re-declares it — the boot bootstrap must not resurrect it.
func TestGateway_RevokedDeclaredKeyStaysRevokedAcrossReboot(t *testing.T) {
	t.Setenv("INFERPLANE_VKEY_REVOKED", "sk-declarative-key-0123456789")
	cfgPath := writeTestConfig(t, func(cfg map[string]any, dir string) {
		cfg["teams"] = map[string]any{"demo": map[string]any{"allowed_models": []string{"*"}}}
		cfg["virtual_keys"] = []map[string]any{{
			"team": "demo", "key_ref": map[string]string{"env": "INFERPLANE_VKEY_REVOKED"}, "allowed_models": []string{"*"},
		}}
	})

	g1, err := newGateway(cfgPath)
	if err != nil {
		t.Fatalf("newGateway (boot 1): %v", err)
	}
	bootAndStop(t, g1)
	if got := authedGet(t, "http://"+g1.DataAddr()+"/v1/models", "sk-declarative-key-0123456789"); got != http.StatusOK {
		t.Fatalf("boot 1: declared key did not authenticate: status %d", got)
	}

	p, err := g1.store.Resolve(context.Background(), "sk-declarative-key-0123456789")
	if err != nil {
		t.Fatal(err)
	}
	if err := g1.store.Revoke(context.Background(), p.KeyID); err != nil {
		t.Fatal(err)
	}

	g2, err := newGateway(cfgPath)
	if err != nil {
		t.Fatalf("newGateway (boot 2, after revoke): %v", err)
	}
	bootAndStop(t, g2)
	if got := authedGet(t, "http://"+g2.DataAddr()+"/v1/models", "sk-declarative-key-0123456789"); got == http.StatusOK {
		t.Fatal("a revoked declared key must not be resurrected by config re-declaration on reboot")
	}
}

// --- Bedrock passthrough ingress (ADR-024): route registration e2e ---

// TestGateway_BedrockRoutesMounted proves the three /model/{modelId}/... routes
// are mounted and reach the bedrockapi handlers: an AUTHENTICATED request for
// an unknown model must get the handler's AWS-shaped error (X-Amzn-ErrorType
// present), not the mux's default plain-text 404. (An unauthenticated probe
// can't prove this — KeyAuth 401s every path, mounted or not.)
func TestGateway_BedrockRoutesMounted(t *testing.T) {
	var keysDBPath string
	cfgPath := writeTestConfig(t, func(cfg map[string]any, dir string) {
		keysDBPath = cfg["key_store"].(map[string]any)["path"].(string)
	})
	pre, err := keystore.OpenSQLite(keysDBPath)
	if err != nil {
		t.Fatal(err)
	}
	plaintext, _, err := pre.Create(context.Background(), "demo", []string{"*"})
	if err != nil {
		t.Fatal(err)
	}
	pre.Close()

	g, err := newGateway(cfgPath)
	if err != nil {
		t.Fatalf("newGateway: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- g.serve(ctx) }()
	waitHTTP(t, "http://"+g.AdminAddr()+"/healthz", http.StatusOK)
	defer func() {
		cancel()
		<-done
	}()

	for _, path := range []string{
		"/model/never-registered-v1:0/invoke",
		"/model/never-registered-v1:0/invoke-with-response-stream",
	} {
		req, _ := http.NewRequest("POST", "http://"+g.DataAddr()+path, strings.NewReader(`{"messages":[]}`))
		req.Header.Set("x-api-key", plaintext)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s: got %d, want 404 for an unknown model", path, resp.StatusCode)
		}
		if resp.Header.Get("X-Amzn-ErrorType") == "" {
			t.Fatalf("%s: 404 lacks X-Amzn-ErrorType — the route is not reaching the bedrockapi handler (plain mux 404)", path)
		}
	}
}

// TestGateway_BedrockCountTokensNeverFails: with a valid key, count-tokens
// must return 200 {"inputTokens":N} even for an unknown model and a garbage
// body — Claude Code's /context crashes on any non-200.
func TestGateway_BedrockCountTokensNeverFails(t *testing.T) {
	var keysDBPath string
	cfgPath := writeTestConfig(t, func(cfg map[string]any, dir string) {
		keysDBPath = cfg["key_store"].(map[string]any)["path"].(string)
	})
	pre, err := keystore.OpenSQLite(keysDBPath)
	if err != nil {
		t.Fatal(err)
	}
	plaintext, _, err := pre.Create(context.Background(), "demo", []string{"*"})
	if err != nil {
		t.Fatal(err)
	}
	pre.Close()

	g, err := newGateway(cfgPath)
	if err != nil {
		t.Fatalf("newGateway: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- g.serve(ctx) }()
	waitHTTP(t, "http://"+g.AdminAddr()+"/healthz", http.StatusOK)
	defer func() {
		cancel()
		<-done
	}()

	req, _ := http.NewRequest("POST", "http://"+g.DataAddr()+"/model/never-registered/count-tokens", strings.NewReader(`{broken json`))
	req.Header.Set("x-api-key", plaintext)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("count-tokens returned %d — any non-200 crashes Claude Code's /context", resp.StatusCode)
	}
	var out struct {
		InputTokens *int64 `json:"inputTokens"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.InputTokens == nil || *out.InputTokens < 1 {
		t.Fatalf("response not {\"inputTokens\">=1}: err=%v out=%+v", err, out)
	}
}
