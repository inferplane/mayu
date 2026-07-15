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
