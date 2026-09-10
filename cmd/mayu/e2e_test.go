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
	"time"

	"github.com/inferplane/inferplane/internal/alert"
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

// reset clears the recorded request so a later assertion can tell whether
// THIS round's request hit this upstream, not merely whether it ever did
// across a multi-scenario test (D7 region-lock e2e).
func (u *anthropicUpstream) reset() {
	u.mu.Lock()
	u.lastAPIKey = ""
	u.mu.Unlock()
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
		// Every configured route needs a rate or the gateway refuses to boot
		// (ADR-030 fail-closed money guard). Tests that assert specific costs
		// override this block with their own figures.
		cfg["pricing"] = map[string]any{
			"overrides": map[string]any{
				"up": map[string]any{
					"claude-test": map[string]any{"input_per_mtok": 1.0, "output_per_mtok": 1.0},
				},
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

// waitRecentBudgetAlert waits for the notifier's published delivery result.
// The webhook handler records receipt BEFORE responding; deliver records Recent
// only AFTER Do returns and the response body is drained/closed. Receipt alone
// therefore cannot synchronize an assertion against the admin API.
func waitRecentBudgetAlert(t *testing.T, adminURL, team, keyID string, threshold float64) {
	t.Helper()
	// Retain the existing eight-second bound (webhook timeout is two seconds).
	// The context also bounds each HTTP request; the ticker only paces polling.
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	var recent struct {
		Fires []alert.Fire `json:"fires"`
	}
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, adminURL+"/admin/alerts/recent", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+e2eAdminToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET /admin/alerts/recent: %v (last fires: %+v)", err, recent.Fires)
		}
		err = json.NewDecoder(resp.Body).Decode(&recent)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || err != nil {
			t.Fatalf("GET /admin/alerts/recent: status %d, decode: %v", resp.StatusCode, err)
		}
		for _, fire := range recent.Fires {
			if fire.Team == team && fire.KeyID == keyID && fire.Threshold == threshold {
				if !fire.Delivered || fire.Error != "" {
					t.Fatalf("matching alert delivery failed: %+v", fire)
				}
				return
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("/admin/alerts/recent missing team=%q key_id=%q threshold=%v: %+v", team, keyID, threshold, recent.Fires)
		case <-ticker.C:
		}
	}
}

// TestE2EBudgetAlertFires (D5b, ADR-017): a team crossing a configured budget
// threshold fires a webhook POST and shows up in /admin/alerts/recent.
func TestE2EBudgetAlertFires(t *testing.T) {
	up := newAnthropicUpstream(t)

	var mu sync.Mutex
	var gotFires []map[string]any
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		json.NewDecoder(r.Body).Decode(&payload)
		mu.Lock()
		gotFires = append(gotFires, payload)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer webhook.Close()

	t.Setenv("E2E_ALERT_WEBHOOK", webhook.URL)
	dataURL, adminURL, _ := bootGateway(t, func(cfg map[string]any, dir string) {
		withAnthropicProvider(up.srv.URL)(cfg, dir)
		// Cost of one request (10 in + 5 out tokens @ $1/token override) = $15.
		// A $20 monthly budget puts that at ratio 0.75 — crosses 0.5, not 1.0.
		cfg["teams"] = map[string]any{
			"alerted": map[string]any{
				"budget": map[string]any{"usd_per_month": 20.0, "on_exceeded": "warn"},
			},
		}
		cfg["pricing"] = map[string]any{
			"overrides": map[string]any{
				"up": map[string]any{
					"claude-test": map[string]any{"input_per_mtok": 1000000.0, "output_per_mtok": 1000000.0},
				},
			},
		}
		cfg["budget_alerts"] = map[string]any{
			"webhook_url_ref": map[string]any{"env": "E2E_ALERT_WEBHOOK"},
			"thresholds":      []any{0.5, 1.0},
			"timeout":         "2s",
		}
	})

	_, key := createKey(t, adminURL, "alerted", []string{"claude-test"})
	resp := postMessages(t, dataURL, key, "claude-test")
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("request: status %d, want 200", resp.StatusCode)
	}

	waitRecentBudgetAlert(t, adminURL, "alerted", "", 0.5)
	mu.Lock()
	fires := append([]map[string]any{}, gotFires...)
	mu.Unlock()
	if len(fires) != 1 {
		t.Fatalf("webhook received %d POSTs, want 1: %+v", len(fires), fires)
	}
	if fires[0]["event"] != "budget_alert" || fires[0]["team"] != "alerted" {
		t.Fatalf("webhook payload = %+v", fires[0])
	}
	if got, want := fires[0]["threshold"], 0.5; got != want {
		t.Fatalf("threshold = %v, want %v", got, want)
	}
}

// TestE2EKeyBudgetAlertFires (ADR-017 per-key follow-up): proves the
// gov.SetKeyBudgetNotify(notifier.ObserveKey) wiring in gateway.go actually
// matters — if that one line were omitted, every internal/alert and
// internal/governance unit test would still pass (they exercise Notifier and
// Governor in isolation), but this real end-to-end request would never fire a
// key-scoped webhook. The team carries NO team budget, proving the fire is
// genuinely key-scoped rather than a team fire that happens to also have a
// key_id.
func TestE2EKeyBudgetAlertFires(t *testing.T) {
	up := newAnthropicUpstream(t)

	var mu sync.Mutex
	var gotFires []map[string]any
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		json.NewDecoder(r.Body).Decode(&payload)
		mu.Lock()
		gotFires = append(gotFires, payload)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer webhook.Close()

	t.Setenv("E2E_KEY_ALERT_WEBHOOK", webhook.URL)
	dataURL, adminURL, _ := bootGateway(t, func(cfg map[string]any, dir string) {
		withAnthropicProvider(up.srv.URL)(cfg, dir)
		// No team budget configured for "unbudgeted" — only the KEY carries one.
		cfg["teams"] = map[string]any{"unbudgeted": map[string]any{}}
		cfg["pricing"] = map[string]any{
			"overrides": map[string]any{
				"up": map[string]any{
					"claude-test": map[string]any{"input_per_mtok": 1000000.0, "output_per_mtok": 1000000.0},
				},
			},
		}
		cfg["budget_alerts"] = map[string]any{
			"webhook_url_ref": map[string]any{"env": "E2E_KEY_ALERT_WEBHOOK"},
			"thresholds":      []any{0.5, 1.0},
			"timeout":         "2s",
		}
	})

	// createKey's shared helper carries no budget param (other e2e tests use
	// it and shouldn't grow an unused one) — build the create-key request
	// inline with budget_usd_micros included.
	body, _ := json.Marshal(map[string]any{
		"team": "unbudgeted", "allowed_models": []string{"claude-test"},
		"budget_usd_micros": 20_000_000, // $20 — one $15 request crosses 0.5, not 1.0
	})
	req, _ := http.NewRequest(http.MethodPost, adminURL+"/admin/keys", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+e2eAdminToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	var created struct {
		KeyID     string `json:"key_id"`
		Plaintext string `json:"plaintext"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("create key: decode: %v", err)
	}
	resp.Body.Close()
	if created.KeyID == "" || created.Plaintext == "" {
		t.Fatalf("create key: unexpected payload %+v", created)
	}

	mresp := postMessages(t, dataURL, created.Plaintext, "claude-test")
	io.Copy(io.Discard, mresp.Body)
	mresp.Body.Close()
	if mresp.StatusCode != http.StatusOK {
		t.Fatalf("request: status %d, want 200", mresp.StatusCode)
	}

	waitRecentBudgetAlert(t, adminURL, "unbudgeted", created.KeyID, 0.5)
	mu.Lock()
	fires := append([]map[string]any{}, gotFires...)
	mu.Unlock()
	if len(fires) != 1 {
		t.Fatalf("webhook received %d POSTs, want 1: %+v", len(fires), fires)
	}
	if fires[0]["event"] != "budget_alert" || fires[0]["team"] != "unbudgeted" || fires[0]["key_id"] != created.KeyID {
		t.Fatalf("webhook payload = %+v, want key_id=%q", fires[0], created.KeyID)
	}
	if got, want := fires[0]["threshold"], 0.5; got != want {
		t.Fatalf("threshold = %v, want %v", got, want)
	}
}

// TestE2EProviderHealthCheckReportsCapability (ADR-014 deferred item): a
// configured provider_health_check block flips the provider_auto_health
// capability, mirroring TestE2ECapabilitiesReportsTeamsRecords's exact shape.
func TestE2EProviderHealthCheckReportsCapability(t *testing.T) {
	up := newAnthropicUpstream(t)
	_, adminURL, _ := bootGateway(t, func(cfg map[string]any, dir string) {
		withAnthropicProvider(up.srv.URL)(cfg, dir)
		cfg["provider_health_check"] = map[string]any{"interval": "50ms"}
	})

	req, _ := http.NewRequest(http.MethodGet, adminURL+"/admin/capabilities", nil)
	req.Header.Set("Authorization", "Bearer "+e2eAdminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var caps struct {
		ProviderAutoHealth bool `json:"provider_auto_health"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&caps); err != nil {
		t.Fatal(err)
	}
	if !caps.ProviderAutoHealth {
		t.Fatal("capabilities.provider_auto_health = false, want true (provider_health_check is configured)")
	}
}

// TestE2EProviderHealthCheckPopulatesStatus proves the background prober
// actually runs against the real assembled gateway (not just the isolated
// worker unit test) -- GET /admin/providers/health must eventually carry the
// registered provider, with a populated last_probed_at, without ANY manual
// POST /admin/providers/test call. The fixture upstream (newAnthropicUpstream)
// has no /v1/models handler, so the probe result itself may be ok:false (404)
// -- this test asserts the auto-probe fired, not that it succeeded.
func TestE2EProviderHealthCheckPopulatesStatus(t *testing.T) {
	up := newAnthropicUpstream(t)
	_, adminURL, _ := bootGateway(t, func(cfg map[string]any, dir string) {
		withAnthropicProvider(up.srv.URL)(cfg, dir)
		cfg["provider_health_check"] = map[string]any{"interval": "50ms"}
	})

	deadline := time.Now().Add(5 * time.Second)
	for {
		req, _ := http.NewRequest(http.MethodGet, adminURL+"/admin/providers/health", nil)
		req.Header.Set("Authorization", "Bearer "+e2eAdminToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if bytes.Contains(body, []byte(`"last_probed_at":"`)) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no auto-probed provider status within 5s: %s", body)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestE2EBodyLoggingRoundTrip (D4, ADR-018): a full-stack body-logging flow —
// a request is captured, /admin/logs lists it with a body_ref, the body is
// fetchable, viewing it emits a body_accessed record (carrying record_ref, no
// body_ref), deletion tombstones it (410), and `audit verify` passes over the
// whole mixed-event chain.
func TestE2EBodyLoggingRoundTrip(t *testing.T) {
	up := newAnthropicUpstream(t)
	// A 32-byte AES key as 64 hex chars (the shape key_ref must resolve to).
	t.Setenv("E2E_BODY_KEY", strings.Repeat("ab", 32))
	var auditPath string
	dataURL, adminURL, shutdown := bootGateway(t, func(cfg map[string]any, dir string) {
		withAnthropicProvider(up.srv.URL)(cfg, dir)
		cfg["audit"].(map[string]any)["log_bodies"] = map[string]any{
			"key_ref": map[string]any{"env": "E2E_BODY_KEY"},
		}
		auditPath = cfg["audit"].(map[string]any)["sinks"].([]any)[0].(map[string]any)["path"].(string)
	})

	adminGET := func(path string) (int, []byte) {
		req, _ := http.NewRequest(http.MethodGet, adminURL+path, nil)
		req.Header.Set("Authorization", "Bearer "+e2eAdminToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}

	_, key := createKey(t, adminURL, "demo", []string{"claude-test"})
	r := postMessages(t, dataURL, key, "claude-test")
	io.Copy(io.Discard, r.Body)
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("request: status %d", r.StatusCode)
	}

	// /admin/logs lists the request with a body_ref (poll briefly — the body
	// capture + analytics ingest are async through the audit sink).
	var ref string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		code, body := adminGET("/admin/logs")
		if code == http.StatusOK {
			var out struct {
				Events []struct {
					BodyRef string `json:"body_ref"`
				} `json:"events"`
			}
			json.Unmarshal(body, &out)
			if len(out.Events) > 0 && out.Events[0].BodyRef != "" {
				ref = out.Events[0].BodyRef
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if ref == "" {
		t.Fatal("/admin/logs never surfaced a body_ref")
	}

	// The body is fetchable and carries the captured request.
	code, body := adminGET("/admin/bodies/" + ref)
	if code != http.StatusOK {
		t.Fatalf("GET /admin/bodies/%s = %d: %s", ref, code, body)
	}
	if !bytes.Contains(body, []byte("hello")) {
		t.Fatalf("fetched body missing the request content: %s", body)
	}

	// DELETE, then a second GET must be the 410 tombstone (never 500).
	delReq, _ := http.NewRequest(http.MethodDelete, adminURL+"/admin/bodies/"+ref, nil)
	delReq.Header.Set("Authorization", "Bearer "+e2eAdminToken)
	delResp, err := http.DefaultClient.Do(delReq)
	if err != nil {
		t.Fatalf("DELETE /admin/bodies/%s: %v", ref, err)
	}
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE = %d, want 204", delResp.StatusCode)
	}
	if code, _ := adminGET("/admin/bodies/" + ref); code != http.StatusGone {
		t.Fatalf("GET after DELETE = %d, want 410 (tombstone)", code)
	}

	shutdown() // drain the audit writer before reading the file sink

	raw, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("read audit sink: %v", err)
	}
	// body_accessed (from the successful GET) and body_deleted are in the
	// chain, carry record_ref, and NEVER a body_ref (§4.7 anti-recursion).
	for _, event := range []string{`"event":"body_accessed"`, `"event":"body_deleted"`} {
		if !bytes.Contains(raw, []byte(event)) {
			t.Fatalf("audit chain missing %s:\n%s", event, raw)
		}
	}
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if (bytes.Contains(line, []byte(`"event":"body_accessed"`)) || bytes.Contains(line, []byte(`"event":"body_deleted"`))) &&
			bytes.Contains(line, []byte(`"body_ref"`)) {
			t.Fatalf("body_accessed/body_deleted must never carry body_ref: %s", line)
		}
	}
	// The whole mixed-event chain still verifies.
	res, err := audit.Verify(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !res.OK {
		t.Fatalf("mixed-event chain not valid: %+v", res)
	}
}

// dailyBudgetConfig declares two teams that differ ONLY in which budget window
// they cap, plus a non-UTC budget_timezone, so one boot exercises both the day
// window's enforcement and the byte-identical-when-unset guarantee.
func dailyBudgetConfig(upstreamURL string) func(cfg map[string]any, dir string) {
	return func(cfg map[string]any, dir string) {
		withAnthropicProvider(upstreamURL)(cfg, dir)
		cfg["budget_timezone"] = "Asia/Seoul"
		cfg["teams"] = map[string]any{
			// First request costs ~(10 in + 5 out tokens) at $1M/MTok ⇒ way over
			// a $0.000001 daily budget; block kicks in on request 2. No monthly
			// cap at all, so a 402 here can only come from the DAY window.
			"daily-only": map[string]any{
				"budget": map[string]any{"usd_per_day": 0.000001, "on_exceeded": "block"},
			},
			// The control: monthly only. Its /v1/usage must carry no day field.
			"monthly-only": map[string]any{
				"budget": map[string]any{"usd_per_month": 1000.0, "on_exceeded": "block"},
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

// TestE2EDailyBudgetBlocks is the end-to-end pin for the whole daily-budget
// chain: config file → governance.ConfigTeam → Limits → TeamPolicy → PreCheck's
// day branch → 402, plus budget_timezone reaching the Governor.
//
// It exists because every layer below has its own unit test and NONE of them
// covers cmd/mayu's wiring: dropping `BudgetUSDPerDay: tc.Budget.USDPerDay`
// from the ConfigTeam literal, or the gov.SetBudgetTimezone call, leaves the
// entire package suite green while the feature is silently dead.
func TestE2EDailyBudgetBlocks(t *testing.T) {
	up := newAnthropicUpstream(t)
	dataURL, adminURL, _ := bootGateway(t, dailyBudgetConfig(up.srv.URL))

	t.Run("daily budget blocks with no monthly cap", func(t *testing.T) {
		_, key := createKey(t, adminURL, "daily-only", []string{"claude-test"})
		r1 := postMessages(t, dataURL, key, "claude-test")
		io.Copy(io.Discard, r1.Body)
		r1.Body.Close()
		if r1.StatusCode != http.StatusOK {
			t.Fatalf("first request: status %d, want 200 (daily budget not yet spent)", r1.StatusCode)
		}
		r2 := postMessages(t, dataURL, key, "claude-test")
		body, _ := io.ReadAll(r2.Body)
		r2.Body.Close()
		if r2.StatusCode != http.StatusPaymentRequired {
			t.Fatalf("second request: status %d, want 402 (daily budget exhausted): %s", r2.StatusCode, body)
		}
		if !strings.Contains(string(body), "daily budget exceeded") {
			t.Fatalf("402 must name the DAILY window, got: %s", body)
		}
		// The reset date is rendered in the configured operator timezone, so it
		// is tomorrow in Asia/Seoul — not necessarily tomorrow in UTC.
		kst, err := time.LoadLocation("Asia/Seoul")
		if err != nil {
			t.Fatalf("LoadLocation: %v (time/tzdata must be imported)", err)
		}
		wantDate := time.Now().In(kst).AddDate(0, 0, 1).Format("2006-01-02")
		if !strings.Contains(string(body), "resets "+wantDate) {
			t.Fatalf("402 reset date should be %s (tomorrow in Asia/Seoul), got: %s", wantDate, body)
		}

		usage := getUsage(t, dataURL, key)
		day, ok := usage["team_budget_day"].(map[string]any)
		if !ok {
			t.Fatalf("/v1/usage must report team_budget_day: %+v", usage)
		}
		if day["window"] != "calendar-day" {
			t.Fatalf("team_budget_day window = %v, want calendar-day", day["window"])
		}
		if day["limit_usd_micros"].(float64) != 1 {
			t.Fatalf("team_budget_day limit = %v, want 1 µUSD", day["limit_usd_micros"])
		}
		// The reset INSTANT is what distinguishes the configured zone from UTC
		// even on a day when the two calendar dates agree.
		resets, err := time.Parse(time.RFC3339, day["resets_at"].(string))
		if err != nil {
			t.Fatalf("resets_at %v: %v", day["resets_at"], err)
		}
		n := time.Now().In(kst)
		wantResets := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, kst).AddDate(0, 0, 1)
		if !resets.Equal(wantResets) {
			t.Fatalf("team_budget_day resets_at = %s, want the next Asia/Seoul midnight %s", resets.Format(time.RFC3339), wantResets.Format(time.RFC3339))
		}
		if _, present := usage["team_budget"]; present {
			t.Fatalf("a team with no monthly cap must not report team_budget: %+v", usage)
		}
	})

	t.Run("a monthly-only team reports no day field", func(t *testing.T) {
		_, key := createKey(t, adminURL, "monthly-only", []string{"claude-test"})
		r := postMessages(t, dataURL, key, "claude-test")
		io.Copy(io.Discard, r.Body)
		r.Body.Close()
		if r.StatusCode != http.StatusOK {
			t.Fatalf("status %d, want 200", r.StatusCode)
		}
		usage := getUsage(t, dataURL, key)
		if _, present := usage["team_budget_day"]; present {
			t.Fatalf("usd_per_day unset must leave /v1/usage byte-identical (no team_budget_day): %+v", usage)
		}
		month, ok := usage["team_budget"].(map[string]any)
		if !ok || month["window"] != "calendar-month" {
			t.Fatalf("monthly field must be unchanged: %+v", usage)
		}
	})
}

func getUsage(t *testing.T, dataURL, virtualKey string) map[string]any {
	t.Helper()
	req, err := http.NewRequest("GET", dataURL+"/v1/usage", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("x-api-key", virtualKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/usage: status %d", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}
