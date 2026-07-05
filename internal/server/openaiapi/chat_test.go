package openaiapi

import (
	"context"
	"iter"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/inferplane/inferplane/internal/budget"
	"github.com/inferplane/inferplane/internal/config"
	"github.com/inferplane/inferplane/internal/governance"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/limiter"
	"github.com/inferplane/inferplane/internal/live"
	"github.com/inferplane/inferplane/internal/metrics"
	"github.com/inferplane/inferplane/internal/openai"
	"github.com/inferplane/inferplane/internal/principal"
	"github.com/inferplane/inferplane/internal/router"
	"github.com/inferplane/inferplane/pkg/schema"
	"github.com/inferplane/inferplane/providers"
	"github.com/inferplane/inferplane/providers/testing/mockprovider"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func testRouter() *router.Router {
	provs := map[string]providers.Provider{"p": mockprovider.New("gpt-x")}
	models := map[string]config.ModelConfig{
		"gpt-x": {Targets: []config.Target{{Provider: "p", Model: "gpt-x"}}},
	}
	return router.New(holderFor(provs, models))
}

func TestChatNonStreamingConvertsMockCanonicalToOpenAI(t *testing.T) {
	// mockprovider returns canonical (Anthropic) response with id "msg_mock";
	// its wire is "anthropic", so the OpenAI ingress must CONVERT to OpenAI shape.
	h := NewChatHandler(testRouter())
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-x","messages":[{"role":"user","content":"hi"}]}`))
	ctx := principal.With(req.Context(), keystore.Principal{KeyID: "ik", Team: "t", AllowedModels: []string{"*"}})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req.WithContext(ctx))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"chat.completion"`) {
		t.Fatalf("response not converted to OpenAI shape: %s", body)
	}
	if !strings.Contains(body, `"finish_reason"`) {
		t.Fatalf("missing finish_reason: %s", body)
	}
}

// TestKeyPolicyOfMapsAllFields guards keyPolicyOf against a future field
// added to KeyOptions or KeyPolicy without updating the mapping (this
// function is duplicated in internal/server/anthropicapi/messages.go —
// governance stays a leaf and does not import keystore, so each ingress
// package maps its own Principal → KeyPolicy; this test only proves THIS
// copy is correct).
func TestKeyPolicyOfMapsAllFields(t *testing.T) {
	p := keystore.Principal{KeyOptions: keystore.KeyOptions{RPM: 60, TPM: 1000, BudgetUSDMicros: 5_000_000}}
	got := keyPolicyOf(p)
	want := governance.KeyPolicy{RatePerMin: 60, TokensPerMinute: 1000, BudgetMicrosPerMonth: 5_000_000}
	if got != want {
		t.Fatalf("keyPolicyOf(%+v) = %+v, want %+v", p.KeyOptions, got, want)
	}
}

func TestChatGovernorKeyBudgetBlocks402EvenForUngovernedTeam(t *testing.T) {
	bud := budget.NewMemory()
	// No TeamPolicy entry for "platform-eng" at all — the team is ungoverned;
	// only the key's own budget (§8 D2) must still be enforced.
	gov := governance.NewGovernor(nil, limiter.NewMemory(), bud, nil)
	bud.Debit("budget:key:ik_over", 1_500_000, 30*24*time.Hour) // over the key's 1M cap

	h := NewChatHandlerFull(testRouter(), nil, gov)
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-x","messages":[{"role":"user","content":"hi"}]}`))
	ctx := principal.With(req.Context(), keystore.Principal{
		KeyID: "ik_over", Team: "platform-eng", AllowedModels: []string{"*"},
		KeyOptions: keystore.KeyOptions{BudgetUSDMicros: 1_000_000},
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req.WithContext(ctx))
	if rec.Code != 402 {
		t.Fatalf("key-budget-exhausted request must be 402, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestChatRecordsRequestMetric(t *testing.T) {
	m := metrics.New()
	h := NewChatHandlerMetrics(testRouter(), nil, nil, m)
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-x","messages":[{"role":"user","content":"hi"}]}`))
	ctx := principal.With(req.Context(), keystore.Principal{KeyID: "ik", Team: "t", AllowedModels: []string{"*"}})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req.WithContext(ctx))
	if rec.Code != 200 {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	got, err := testutil.GatherAndCount(m.Registry(), "inferplane_requests_total")
	if err != nil {
		t.Fatal(err)
	}
	if got == 0 {
		t.Fatal("inferplane_requests_total not recorded")
	}
}

func TestChatUnknownModel404(t *testing.T) {
	h := NewChatHandler(testRouter())
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"nope","messages":[]}`))
	ctx := principal.With(req.Context(), keystore.Principal{AllowedModels: []string{"*"}})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req.WithContext(ctx))
	if rec.Code != 404 {
		t.Fatalf("want 404, got %d", rec.Code)
	}
}

func TestChat404DoesNotLeakModelLabel(t *testing.T) {
	m := metrics.New()
	h := NewChatHandlerMetrics(testRouter(), nil, nil, m)
	// 50 distinct unknown model names must NOT create 50 distinct metric series.
	for i := 0; i < 50; i++ {
		body := `{"model":"attacker-` + strconv.Itoa(i) + `","messages":[]}`
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
		ctx := principal.With(req.Context(), keystore.Principal{AllowedModels: []string{"*"}})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req.WithContext(ctx))
		if rec.Code != 404 {
			t.Fatalf("want 404, got %d", rec.Code)
		}
	}
	// inferplane_requests_total must have a BOUNDED number of series (the sentinel),
	// not 50.
	n := testutil.CollectAndCount(m.Registry(), "inferplane_requests_total")
	if n > 2 { // sentinel "_rejected" (+ possibly the zero-init series) — must be small, NOT ~50
		t.Fatalf("unbounded model label cardinality: %d series for 50 distinct unknown models", n)
	}
}

func TestChatAllowListBlocks403(t *testing.T) {
	h := NewChatHandler(testRouter())
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"gpt-x","messages":[]}`))
	ctx := principal.With(req.Context(), keystore.Principal{AllowedModels: []string{"other"}})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req.WithContext(ctx))
	if rec.Code != 403 {
		t.Fatalf("want 403, got %d", rec.Code)
	}
}

func TestChatNoPrincipal401(t *testing.T) {
	h := NewChatHandler(testRouter())
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"gpt-x","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("want 401, got %d", rec.Code)
	}
}

func TestChatStreamingConvertsMockCanonicalToOpenAI(t *testing.T) {
	// mockprovider streams Anthropic SSE; the OpenAI ingress (anthropic-wire
	// provider) must re-serialize canonical chunks into OpenAI chunk shape and
	// terminate with [DONE].
	h := NewChatHandler(testRouter())
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-x","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	ctx := principal.With(req.Context(), keystore.Principal{KeyID: "ik", Team: "t", AllowedModels: []string{"*"}})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req.WithContext(ctx))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"chat.completion.chunk"`) {
		t.Fatalf("stream not converted to OpenAI chunk shape: %s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("stream missing terminal [DONE]: %s", body)
	}
	// must NOT leak the Anthropic SSE event lines
	if strings.Contains(body, "event: message_start") {
		t.Fatalf("anthropic SSE leaked into openai stream: %s", body)
	}
}

// failProvider always errors, to drive the pre-TTFT fallback to the next target.
type failProvider struct{}

func (failProvider) Name() string               { return "fail" }
func (failProvider) Models() []schema.ModelInfo { return nil }
func (failProvider) Complete(context.Context, *providers.ProxyRequest) (*providers.ProxyResponse, error) {
	return nil, errInjected
}
func (failProvider) Stream(context.Context, *providers.ProxyRequest) (iter.Seq2[*providers.StreamEvent, error], error) {
	return nil, errInjected
}

var errInjected = stringErr("upstream down")

type stringErr string

func (e stringErr) Error() string { return string(e) }

func TestChatNonStreamingFallsBackPreTTFT(t *testing.T) {
	provs := map[string]providers.Provider{
		"bad":  failProvider{},
		"good": mockprovider.New("gpt-x"),
	}
	models := map[string]config.ModelConfig{
		"gpt-x": {Targets: []config.Target{
			{Provider: "bad", Model: "m1"},
			{Provider: "good", Model: "gpt-x"},
		}},
	}
	h := NewChatHandler(router.New(holderFor(provs, models)))
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-x","messages":[{"role":"user","content":"hi"}]}`))
	ctx := principal.With(req.Context(), keystore.Principal{AllowedModels: []string{"*"}})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req.WithContext(ctx))
	if rec.Code != 200 {
		t.Fatalf("fallback should yield 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"chat.completion"`) {
		t.Fatalf("fallback response not converted: %s", rec.Body.String())
	}
	if got := rec.Header().Get("X-Inferplane-Fallback"); got != "good" {
		t.Fatalf("x-inferplane-fallback header = %q, want %q", got, "good")
	}
}

// ── openai-wire provider: tee verbatim (no conversion) ───────────────────────

// oaiWireProvider mimics openai_compatible: its native wire is "openai", so the
// ingress must forward Raw/RawBody verbatim instead of converting from canonical.
type oaiWireProvider struct{}

func (oaiWireProvider) Name() string               { return "openai_compatible" }
func (oaiWireProvider) Models() []schema.ModelInfo { return nil }

func (oaiWireProvider) Complete(_ context.Context, _ *providers.ProxyRequest) (*providers.ProxyResponse, error) {
	raw := []byte(`{"id":"verbatim","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`)
	parsed, _ := openai.ResponseToCanonical(raw)
	return &providers.ProxyResponse{StatusCode: 200, RawBody: raw, Parsed: parsed}, nil
}

func (oaiWireProvider) Stream(_ context.Context, _ *providers.ProxyRequest) (iter.Seq2[*providers.StreamEvent, error], error) {
	events := []*providers.StreamEvent{
		{Raw: []byte("data: {\"id\":\"v\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")},
		{Raw: []byte("data: [DONE]\n\n")},
	}
	return func(yield func(*providers.StreamEvent, error) bool) {
		for _, ev := range events {
			if !yield(ev, nil) {
				return
			}
		}
	}, nil
}

func oaiWireRouter() *router.Router {
	provs := map[string]providers.Provider{"p": oaiWireProvider{}}
	models := map[string]config.ModelConfig{
		"qwen": {Targets: []config.Target{{Provider: "p", Model: "qwen"}}},
	}
	return router.New(holderFor(provs, models))
}

func TestChatTeesOpenAIProviderVerbatim(t *testing.T) {
	h := NewChatHandler(oaiWireRouter())
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"qwen","messages":[{"role":"user","content":"hi"}]}`))
	ctx := principal.With(req.Context(), keystore.Principal{KeyID: "ik", Team: "t", AllowedModels: []string{"*"}})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req.WithContext(ctx))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"verbatim"`) {
		t.Fatalf("openai-wire provider body not teed verbatim: %s", rec.Body.String())
	}
}

func TestChatTeesOpenAIProviderStreamVerbatim(t *testing.T) {
	h := NewChatHandler(oaiWireRouter())
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"qwen","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	ctx := principal.With(req.Context(), keystore.Principal{KeyID: "ik", Team: "t", AllowedModels: []string{"*"}})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req.WithContext(ctx))
	body := rec.Body.String()
	if !strings.Contains(body, `"v"`) || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("openai-wire stream not teed verbatim: %s", body)
	}
}

func holderFor(provs map[string]providers.Provider, models map[string]config.ModelConfig) *live.Holder {
	ids := make(map[string]string, len(provs))
	for n := range provs {
		ids[n] = n
	}
	h := &live.Holder{}
	h.Swap(live.NewState(provs, models, nil, ids))
	return h
}
