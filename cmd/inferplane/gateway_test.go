package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
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
	deadline := time.Now().Add(5 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
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
