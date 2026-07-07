package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// analyticsModeBTestDSN skips the test if no local test Postgres is configured
// — Mode B integration is exercised here against a real instance, but the
// zero-dependency default path (this whole test file) must never require one.
func analyticsModeBTestDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("INFERPLANE_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("INFERPLANE_TEST_PG_DSN not set; skipping Mode B gateway integration test")
	}
	return dsn
}

func getAdmin(t *testing.T, url, token string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return resp
}

func TestGatewayAnalyticsModeBEndToEnd(t *testing.T) {
	dsn := analyticsModeBTestDSN(t)
	t.Setenv("E2E_MODE_B_PG_DSN", dsn)
	t.Setenv("E2E_MODE_B_ADMIN_TOKEN", "mode-b-admin-token")

	auditDir := t.TempDir()
	record := `{"schema_version":1,"event":"request_completed","id":"01MODEB","ts":"2026-07-07T10:00:00Z","instance":"x","principal":{"key_id":"k","team":"alpha"},"request":{"ingress":"anthropic","model_requested":"m1","model_resolved":"m1"},"usage":{"input_tokens":10,"output_tokens":5},"cost":{"amount_usd_micros":777},"prev_hash":"sha256:genesis"}` + "\n"
	if err := os.WriteFile(filepath.Join(auditDir, "seg-a.jsonl"), []byte(record), 0o600); err != nil {
		t.Fatal(err)
	}

	cfgPath := writeTestConfig(t, func(cfg map[string]any, dir string) {
		cfg["server"].(map[string]any)["admin_auth"] = map[string]any{
			"token_refs": []any{map[string]any{"env": "E2E_MODE_B_ADMIN_TOKEN"}},
		}
		cfg["analytics"] = map[string]any{
			"mode_b": map[string]any{
				"dsn_ref":              map[string]any{"env": "E2E_MODE_B_PG_DSN"},
				"aggregated_audit_dir": auditDir,
				"poll_interval":        "1s",
				"lease_ttl":            "5s",
			},
		}
	})

	g, err := newGateway(cfgPath)
	if err != nil {
		t.Fatalf("newGateway: %v", err)
	}
	// Truncate any state a previous test run left in the shared test Postgres.
	if err := g.pgstoreQ.Rebuild(context.Background()); err != nil {
		t.Fatalf("pre-test Rebuild: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	serveErr := make(chan error, 1)
	go func() { defer wg.Done(); serveErr <- g.serve(ctx) }()

	adminURL := "http://" + g.AdminAddr()
	token := "mode-b-admin-token"

	deadline := time.Now().Add(10 * time.Second)
	var lastIngestTS string
	for time.Now().Before(deadline) {
		resp := getAdmin(t, adminURL+"/admin/analytics/health", token)
		var h struct {
			LastIngestTS string `json:"last_ingest_ts"`
			Mode         string `json:"mode"`
		}
		json.NewDecoder(resp.Body).Decode(&h)
		resp.Body.Close()
		if h.LastIngestTS != "" {
			lastIngestTS = h.LastIngestTS
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if lastIngestTS == "" {
		t.Fatal("aggregator never ingested the fixture segment within the deadline")
	}

	resp := getAdmin(t, adminURL+"/admin/analytics/summary", token)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"cost_micros":777`) {
		t.Fatalf("summary = %d %s, want the fixture's cost reflected", resp.StatusCode, body)
	}

	resp = getAdmin(t, adminURL+"/admin/capabilities", token)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `"analytics_index":"B"`) {
		t.Fatalf("capabilities = %s, want analytics_index:\"B\"", body)
	}

	cancel()
	select {
	case err := <-serveErr:
		if err != nil {
			t.Errorf("serve: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("g.serve did not return within 10s of cancel — aggregator goroutine leak")
	}
	wg.Wait()
}

func TestGatewayAnalyticsModeAbsentStaysModeA(t *testing.T) {
	// Zero-dependency default: with no analytics.mode_b block, the gateway must
	// never touch Postgres (ADR-015 §5's acceptance criterion) and capabilities
	// must report "off" (no file sink → no analytics at all, matching Phase 1a).
	cfgPath := writeTestConfig(t, nil)
	g, err := newGateway(cfgPath)
	if err != nil {
		t.Fatalf("newGateway: %v", err)
	}
	if g.pgstoreQ != nil {
		t.Fatal("pgstoreQ must be nil with no analytics.mode_b configured")
	}
	t.Cleanup(func() { g.store.Close(); g.aud.Close() })
}
