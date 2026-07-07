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
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/inferplane/inferplane/internal/audit"
)

const (
	e2eUpstreamKey = "sk-upstream-secret-do-not-leak"
	e2eAdminToken  = "admin-token-do-not-leak"
)

// bootGateway writes a config (base skeleton + mutate), boots the gateway, and
// tears it down with the test. Returns base URLs for both planes plus an
// idempotent shutdown func — tests that inspect post-drain state (the audit
// file sink) call it explicitly; everyone else relies on the cleanup.
func bootGateway(t *testing.T, mutate func(cfg map[string]any, dir string)) (dataURL, adminURL string, shutdown func()) {
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
	var once sync.Once
	shutdown = func() {
		once.Do(func() {
			cancel()
			if err := <-done; err != nil {
				t.Errorf("serve: %v", err)
			}
		})
	}
	t.Cleanup(shutdown)
	return "http://" + g.DataAddr(), "http://" + g.AdminAddr(), shutdown
}

// anthropicUpstream fakes the Anthropic Messages API. It records the last
// x-api-key / Authorization header it saw and returns a fixed message with
// usage.
type anthropicUpstream struct {
	srv *httptest.Server

	mu            sync.Mutex
	lastAPIKey    string
	lastAuthorize string
}

func newAnthropicUpstream(t *testing.T) *anthropicUpstream {
	t.Helper()
	u := &anthropicUpstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.lastAPIKey = r.Header.Get("x-api-key")
		u.lastAuthorize = r.Header.Get("Authorization")
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

// e2eSSEBody is the exact byte stream the streaming upstream emits — the
// gateway must tee it verbatim to the client (cache invariant §4.4: same
// protocol on both sides ⇒ byte-identical passthrough).
const e2eSSEBody = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_sse","type":"message","role":"assistant","model":"claude-test","content":[],"usage":{"input_tokens":10,"output_tokens":1}}}` + "\n\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n" +
	"event: content_block_stop\n" +
	`data: {"type":"content_block_stop","index":0}` + "\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}` + "\n\n" +
	"event: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n\n"

// newStreamingUpstream fakes an Anthropic upstream that always streams.
func newStreamingUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, e2eSSEBody)
	}))
	t.Cleanup(srv.Close)
	return srv
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
	dataURL, adminURL, _ := bootGateway(t, withAnthropicProvider(up.srv.URL))

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
	dataURL, adminURL, _ := bootGateway(t, withAnthropicProvider(up.srv.URL))

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
	dataURL, adminURL, _ := bootGateway(t, withAnthropicProvider(up.srv.URL))

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

func TestE2EStreamingSSE(t *testing.T) {
	up := newStreamingUpstream(t)
	dataURL, adminURL, _ := bootGateway(t, withAnthropicProvider(up.URL))

	_, plaintext := createKey(t, adminURL, "demo", []string{"claude-test"})

	body := `{"model":"claude-test","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hello"}]}`
	req, _ := http.NewRequest(http.MethodPost, dataURL+"/v1/messages", strings.NewReader(body))
	req.Header.Set("x-api-key", plaintext)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream: status %d: %s", resp.StatusCode, got)
	}
	// Same-protocol passthrough must be byte-exact (§4.4).
	if string(got) != e2eSSEBody {
		t.Fatalf("stream not teed verbatim:\n got: %q\nwant: %q", got, e2eSSEBody)
	}
}

// govConfig adds teams exercising the block/warn paths plus a pricing override
// for the mock model — without it, pricing.on_missing=allow prices unknown
// models at 0 µUSD and the budget path could never trigger.
func govConfig(upstreamURL string) func(cfg map[string]any, dir string) {
	return func(cfg map[string]any, dir string) {
		withAnthropicProvider(upstreamURL)(cfg, dir)
		cfg["teams"] = map[string]any{
			"ratelimited": map[string]any{
				"rate_limit": map[string]any{"requests_per_minute": 1},
			},
			"broke": map[string]any{
				// First request costs ~ (10 in + 5 out tokens) at $1M/MTok ⇒ way
				// over a $0.000001 monthly budget; block kicks in on request 2.
				"budget": map[string]any{"usd_per_month": 0.000001, "on_exceeded": "block"},
			},
			"warned": map[string]any{
				"budget": map[string]any{"usd_per_month": 0.000001, "on_exceeded": "warn"},
			},
		}
		cfg["pricing"] = map[string]any{
			"overrides": map[string]any{
				"up": map[string]any{
					"claude-test": map[string]any{"input_per_mtok": 1000000.0, "output_per_mtok": 1000000.0},
				},
			},
		}
	}
}

func TestE2EGovernanceBlocks(t *testing.T) {
	up := newAnthropicUpstream(t)
	dataURL, adminURL, _ := bootGateway(t, govConfig(up.srv.URL))

	t.Run("rate limit 429", func(t *testing.T) {
		_, key := createKey(t, adminURL, "ratelimited", []string{"claude-test"})
		r1 := postMessages(t, dataURL, key, "claude-test")
		io.Copy(io.Discard, r1.Body)
		r1.Body.Close()
		if r1.StatusCode != http.StatusOK {
			t.Fatalf("first request: status %d, want 200", r1.StatusCode)
		}
		r2 := postMessages(t, dataURL, key, "claude-test")
		io.Copy(io.Discard, r2.Body)
		r2.Body.Close()
		if r2.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("second request: status %d, want 429", r2.StatusCode)
		}
	})

	t.Run("budget block 402", func(t *testing.T) {
		_, key := createKey(t, adminURL, "broke", []string{"claude-test"})
		r1 := postMessages(t, dataURL, key, "claude-test")
		io.Copy(io.Discard, r1.Body)
		r1.Body.Close()
		if r1.StatusCode != http.StatusOK {
			t.Fatalf("first request: status %d, want 200 (budget not yet spent)", r1.StatusCode)
		}
		r2 := postMessages(t, dataURL, key, "claude-test")
		io.Copy(io.Discard, r2.Body)
		r2.Body.Close()
		if r2.StatusCode != http.StatusPaymentRequired {
			t.Fatalf("second request: status %d, want 402 (budget exhausted, on_exceeded=block)", r2.StatusCode)
		}
	})

	t.Run("budget warn passes", func(t *testing.T) {
		_, key := createKey(t, adminURL, "warned", []string{"claude-test"})
		for i := 1; i <= 2; i++ {
			r := postMessages(t, dataURL, key, "claude-test")
			io.Copy(io.Discard, r.Body)
			r.Body.Close()
			if r.StatusCode != http.StatusOK {
				t.Fatalf("request %d: status %d, want 200 (on_exceeded=warn must not block)", i, r.StatusCode)
			}
		}
	})
}

func TestE2EAuditChainVerifies(t *testing.T) {
	up := newAnthropicUpstream(t)
	var auditPath string
	dataURL, adminURL, shutdown := bootGateway(t, func(cfg map[string]any, dir string) {
		withAnthropicProvider(up.srv.URL)(cfg, dir)
		auditPath = cfg["audit"].(map[string]any)["sinks"].([]any)[0].(map[string]any)["path"].(string)
	})

	_, key := createKey(t, adminURL, "demo", []string{"claude-test"})
	for i := 0; i < 3; i++ {
		r := postMessages(t, dataURL, key, "claude-test")
		io.Copy(io.Discard, r.Body)
		r.Body.Close()
	}
	shutdown() // drain the audit writer so the file sink is complete

	raw, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("read audit sink: %v", err)
	}
	res, err := audit.Verify(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !res.OK || res.Records < 6 { // 3 requests × (started + completed)
		t.Fatalf("chain not valid: %+v (want OK with ≥6 records)", res)
	}

	// Tamper one byte inside a record and the chain must break.
	tampered := bytes.Replace(raw, []byte(`"team":"demo"`), []byte(`"team":"DEMO"`), 1)
	if bytes.Equal(tampered, raw) {
		t.Fatal("tamper target not found in audit log")
	}
	tres, err := audit.Verify(bytes.NewReader(tampered))
	if err != nil {
		t.Fatalf("verify tampered: %v", err)
	}
	if tres.OK {
		t.Fatal("tampered chain verified OK — tamper-evidence broken")
	}
}

// TestE2EAdminActionsAudited (plan 2026-06-12 task 6): admin key create and
// revoke are governance events — they land in the tamper-evident chain with
// the break-glass identity and auth_method, and the chain still verifies.
func TestE2EAdminActionsAudited(t *testing.T) {
	up := newAnthropicUpstream(t)
	var auditPath string
	_, adminURL, shutdown := bootGateway(t, func(cfg map[string]any, dir string) {
		withAnthropicProvider(up.srv.URL)(cfg, dir)
		auditPath = cfg["audit"].(map[string]any)["sinks"].([]any)[0].(map[string]any)["path"].(string)
	})

	keyID, _ := createKey(t, adminURL, "demo", []string{"*"})
	req, _ := http.NewRequest(http.MethodDelete, adminURL+"/admin/keys/"+keyID, nil)
	req.Header.Set("Authorization", "Bearer "+e2eAdminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke: status %d", resp.StatusCode)
	}
	resp.Body.Close()
	shutdown()

	raw, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	res, err := audit.Verify(bytes.NewReader(raw))
	if err != nil || !res.OK {
		t.Fatalf("chain: %+v %v", res, err)
	}
	for _, want := range []string{
		`"event":"admin_key_created"`,
		`"event":"admin_key_revoked"`,
		`"user":"break-glass"`,
		`"auth_method":"break_glass"`,
	} {
		if !bytes.Contains(raw, []byte(want)) {
			t.Fatalf("audit log missing %s:\n%s", want, raw)
		}
	}
}
