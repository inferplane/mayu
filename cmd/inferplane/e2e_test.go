package main

// Full-stack E2E tests (plan tasks 2-3): boot the assembled gateway from a real
// config file against httptest upstreams and drive it over real sockets —
// admin key issuance → data-plane traffic → governance → audit. Secrets enter
// only via env refs (t.Setenv), never inline (§7).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const (
	e2eUpstreamKey = "sk-upstream-secret-do-not-leak"
	e2eAdminToken  = "admin-token-do-not-leak"
)

// bootGateway writes a config (base skeleton + mutate), boots the gateway, and
// tears it down with the test. Returns base URLs for both planes.
func bootGateway(t *testing.T, mutate func(cfg map[string]any, dir string)) (dataURL, adminURL string) {
	t.Helper()
	t.Setenv("E2E_UPSTREAM_KEY", e2eUpstreamKey)
	t.Setenv("E2E_ADMIN_TOKEN", e2eAdminToken)
	cfgPath := writeTestConfig(t, func(cfg map[string]any, dir string) {
		cfg["server"].(map[string]any)["admin_auth"] = map[string]any{
			"token_refs": []any{map[string]any{"env": "E2E_ADMIN_TOKEN"}},
		}
		if mutate != nil {
			mutate(cfg, dir)
		}
	})
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
	return "http://" + g.DataAddr(), "http://" + g.AdminAddr()
}

// anthropicUpstream fakes the Anthropic Messages API. It records the last
// x-api-key it saw and returns a fixed message with usage.
type anthropicUpstream struct {
	srv *httptest.Server

	mu         sync.Mutex
	lastAPIKey string
}

func newAnthropicUpstream(t *testing.T) *anthropicUpstream {
	t.Helper()
	u := &anthropicUpstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.lastAPIKey = r.Header.Get("x-api-key")
		u.mu.Unlock()
		switch r.URL.Path {
		case "/v1/messages":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"msg_e2e","type":"message","role":"assistant","model":"claude-test",`+
				`"content":[{"type":"text","text":"hello from upstream"}],"stop_reason":"end_turn",`+
				`"usage":{"input_tokens":10,"output_tokens":5}}`)
		case "/v1/messages/count_tokens":
			http.Error(w, `{"type":"error"}`, http.StatusInternalServerError) // forces the estimate fallback
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *anthropicUpstream) apiKey() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.lastAPIKey
}

// withAnthropicProvider routes model "claude-test" to an anthropic-type
// provider pointed at the fake upstream (gateway credential via env ref).
func withAnthropicProvider(upstreamURL string) func(cfg map[string]any, dir string) {
	return func(cfg map[string]any, dir string) {
		cfg["providers"] = map[string]any{
			"up": map[string]any{
				"type":        "anthropic",
				"base_url":    upstreamURL,
				"api_key_ref": map[string]any{"env": "E2E_UPSTREAM_KEY"},
			},
		}
		cfg["models"] = map[string]any{
			"claude-test": map[string]any{
				"targets": []any{map[string]any{"provider": "up", "model": "claude-test"}},
			},
		}
	}
}

// createKey issues a virtual key through the admin API and returns
// (key_id, plaintext).
func createKey(t *testing.T, adminURL, team string, models []string) (string, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"team": team, "allowed_models": models})
	req, _ := http.NewRequest(http.MethodPost, adminURL+"/admin/keys", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+e2eAdminToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create key: status %d: %s", resp.StatusCode, b)
	}
	var out struct {
		KeyID     string `json:"key_id"`
		Plaintext string `json:"plaintext"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("create key: decode: %v", err)
	}
	if out.KeyID == "" || !strings.HasPrefix(out.Plaintext, "ik_") {
		t.Fatalf("create key: unexpected payload key_id=%q plaintext_prefix=%q", out.KeyID, out.Plaintext[:min(len(out.Plaintext), 3)])
	}
	return out.KeyID, out.Plaintext
}

// postMessages sends a Messages request with the given virtual key.
func postMessages(t *testing.T, dataURL, virtualKey, model string) *http.Response {
	t.Helper()
	body := fmt.Sprintf(`{"model":%q,"max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`, model)
	req, _ := http.NewRequest(http.MethodPost, dataURL+"/v1/messages", strings.NewReader(body))
	req.Header.Set("x-api-key", virtualKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/messages: %v", err)
	}
	return resp
}

func TestE2EMessagesRoundTrip(t *testing.T) {
	up := newAnthropicUpstream(t)
	dataURL, adminURL := bootGateway(t, withAnthropicProvider(up.srv.URL))

	_, plaintext := createKey(t, adminURL, "demo", []string{"claude-test"})

	resp := postMessages(t, dataURL, plaintext, "claude-test")
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("messages: status %d: %s", resp.StatusCode, body)
	}
	if !bytes.Contains(body, []byte("msg_e2e")) {
		t.Fatalf("messages: upstream body not passed through: %s", body)
	}

	// §5.2: upstream must see the gateway's credential, never the client's key.
	if got := up.apiKey(); got != e2eUpstreamKey {
		t.Fatalf("upstream x-api-key = %q, want gateway credential", got)
	}
	if up.apiKey() == plaintext {
		t.Fatal("client virtual key was forwarded upstream")
	}
}

func TestE2ECountTokensNeverNon200(t *testing.T) {
	up := newAnthropicUpstream(t) // its count_tokens endpoint always 500s
	dataURL, adminURL := bootGateway(t, withAnthropicProvider(up.srv.URL))

	_, plaintext := createKey(t, adminURL, "demo", []string{"claude-test"})

	body := `{"model":"claude-test","messages":[{"role":"user","content":"hello world"}]}`
	req, _ := http.NewRequest(http.MethodPost, dataURL+"/v1/messages/count_tokens", strings.NewReader(body))
	req.Header.Set("x-api-key", plaintext)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("count_tokens: %v", err)
	}
	defer resp.Body.Close()
	// The upstream TokenCounter fails (500) — the handler MUST still answer 200
	// with an estimate (a non-200 crashes Claude Code).
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("count_tokens: status %d, want 200 always", resp.StatusCode)
	}
	var out struct {
		InputTokens int64 `json:"input_tokens"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("count_tokens: decode: %v", err)
	}
	if out.InputTokens <= 0 {
		t.Fatalf("count_tokens: input_tokens = %d, want > 0 estimate", out.InputTokens)
	}
}

func TestE2EMetricsLeakFree(t *testing.T) {
	up := newAnthropicUpstream(t)
	dataURL, adminURL := bootGateway(t, withAnthropicProvider(up.srv.URL))

	keyID, plaintext := createKey(t, adminURL, "demo", []string{"claude-test"})
	resp := postMessages(t, dataURL, plaintext, "claude-test")
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	mresp, err := http.Get(adminURL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer mresp.Body.Close()
	if mresp.StatusCode != http.StatusOK {
		t.Fatalf("/metrics status %d", mresp.StatusCode)
	}
	metrics, _ := io.ReadAll(mresp.Body)
	for name, secret := range map[string]string{
		"virtual key plaintext": plaintext,
		"key_id":                keyID,
		"upstream key":          e2eUpstreamKey,
		"admin token":           e2eAdminToken,
	} {
		if bytes.Contains(metrics, []byte(secret)) {
			t.Errorf("/metrics leaks %s", name)
		}
	}
}
