package anthropicapi

import (
	"bytes"
	"context"
	"errors"
	"iter"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/inferplane/inferplane/internal/audit"
	"github.com/inferplane/inferplane/internal/budget"
	"github.com/inferplane/inferplane/internal/config"
	"github.com/inferplane/inferplane/internal/governance"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/limiter"
	"github.com/inferplane/inferplane/internal/pricing"
	"github.com/inferplane/inferplane/internal/principal"
	"github.com/inferplane/inferplane/internal/router"
	"github.com/inferplane/inferplane/pkg/schema"
	"github.com/inferplane/inferplane/providers"
	"github.com/inferplane/inferplane/providers/testing/mockprovider"
)

// allowAll wraps a request with a principal whose allow-list is "*". The
// handler now requires a principal in context (401 otherwise), so the tests
// that don't exercise the allow-list itself inject a permissive one.
func allowAll(req *http.Request) *http.Request {
	return req.WithContext(principal.With(req.Context(),
		keystore.Principal{AllowedModels: []string{"*"}}))
}

func testRouter() *router.Router {
	provs := map[string]providers.Provider{"p": mockprovider.New("claude-sonnet-4-6")}
	models := map[string]config.ModelConfig{
		"claude-sonnet-4-6": {Targets: []config.Target{{Provider: "p", Model: "claude-sonnet-4-6"}}},
	}
	return router.New(provs, models)
}

func TestMessagesNonStreaming(t *testing.T) {
	h := NewMessagesHandler(testRouter())
	req := httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"claude-sonnet-4-6","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, allowAll(req))
	if rec.Code != 200 {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"msg_mock"`) {
		t.Fatalf("body missing mock response: %s", rec.Body.String())
	}
}

func TestMessagesStreamingTee(t *testing.T) {
	h := NewMessagesHandler(testRouter())
	req := httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"claude-sonnet-4-6","stream":true,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, allowAll(req))
	body := rec.Body.String()
	if !strings.Contains(body, "event: message_start") || !strings.Contains(body, "event: message_stop") {
		t.Fatalf("stream not teed verbatim: %s", body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}
}

func TestMessagesUnknownModel(t *testing.T) {
	h := NewMessagesHandler(testRouter())
	req := httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"no-such-model","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, allowAll(req))
	if rec.Code != 404 {
		t.Fatalf("expected 404 for unknown model, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"error"`) {
		t.Fatalf("expected anthropic error body: %s", rec.Body.String())
	}
}

type errStreamProvider struct{}

func (errStreamProvider) Name() string               { return "errstream" }
func (errStreamProvider) Models() []schema.ModelInfo { return nil }
func (errStreamProvider) Complete(context.Context, *providers.ProxyRequest) (*providers.ProxyResponse, error) {
	return nil, errors.New("unused")
}
func (errStreamProvider) Stream(context.Context, *providers.ProxyRequest) (iter.Seq2[*providers.StreamEvent, error], error) {
	return nil, &providers.UpstreamError{StatusCode: 429, Body: []byte(`{"type":"error","error":{"type":"rate_limit_error"}}`), Header: http.Header{}}
}

func TestMessagesStreamingUpstreamErrorTeed(t *testing.T) {
	provs := map[string]providers.Provider{"p": errStreamProvider{}}
	models := map[string]config.ModelConfig{"m": {Targets: []config.Target{{Provider: "p", Model: "m"}}}}
	h := NewMessagesHandler(router.New(provs, models))
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"m","stream":true,"messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, allowAll(req))
	if rec.Code != 429 {
		t.Fatalf("expected upstream 429 teed, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "rate_limit_error") {
		t.Fatalf("upstream error body not teed: %s", rec.Body.String())
	}
}

type headerProvider struct{}

func (headerProvider) Name() string               { return "hdr" }
func (headerProvider) Models() []schema.ModelInfo { return nil }
func (headerProvider) Complete(context.Context, *providers.ProxyRequest) (*providers.ProxyResponse, error) {
	return &providers.ProxyResponse{
		StatusCode: 200,
		Headers:    http.Header{"Request-Id": {"req_123"}, "Anthropic-Ratelimit-Requests-Remaining": {"42"}, "Content-Type": {"application/json"}},
		RawBody:    []byte(`{"id":"msg_x","type":"message","role":"assistant","model":"m","content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`),
	}, nil
}
func (headerProvider) Stream(context.Context, *providers.ProxyRequest) (iter.Seq2[*providers.StreamEvent, error], error) {
	return nil, errors.New("unused")
}

func TestMessagesNonStreamingTeesUpstreamHeaders(t *testing.T) {
	provs := map[string]providers.Provider{"p": headerProvider{}}
	models := map[string]config.ModelConfig{"m": {Targets: []config.Target{{Provider: "p", Model: "m"}}}}
	h := NewMessagesHandler(router.New(provs, models))
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"m","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, allowAll(req))
	if rec.Header().Get("Request-Id") != "req_123" {
		t.Fatalf("request-id not teed: %q", rec.Header().Get("Request-Id"))
	}
	if rec.Header().Get("Anthropic-Ratelimit-Requests-Remaining") != "42" {
		t.Fatalf("ratelimit header not teed")
	}
}

type retryStreamProvider struct{}

func (retryStreamProvider) Name() string               { return "retry" }
func (retryStreamProvider) Models() []schema.ModelInfo { return nil }
func (retryStreamProvider) Complete(context.Context, *providers.ProxyRequest) (*providers.ProxyResponse, error) {
	return nil, errors.New("unused")
}
func (retryStreamProvider) Stream(context.Context, *providers.ProxyRequest) (iter.Seq2[*providers.StreamEvent, error], error) {
	return nil, &providers.UpstreamError{StatusCode: 429, Body: []byte(`{"type":"error"}`), Header: http.Header{"Retry-After": {"30"}}}
}

func TestMessagesStreamingErrorTeesHeaders(t *testing.T) {
	provs := map[string]providers.Provider{"p": retryStreamProvider{}}
	models := map[string]config.ModelConfig{"m": {Targets: []config.Target{{Provider: "p", Model: "m"}}}}
	h := NewMessagesHandler(router.New(provs, models))
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"m","stream":true,"messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, allowAll(req))
	if rec.Code != 429 || rec.Header().Get("Retry-After") != "30" {
		t.Fatalf("streaming error headers not teed: code=%d retry-after=%q", rec.Code, rec.Header().Get("Retry-After"))
	}
}

func TestMessagesEnforcesAllowList(t *testing.T) {
	h := NewMessagesHandler(testRouter())
	req := httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}`))
	ctx := principal.With(req.Context(), keystore.Principal{KeyID: "ik", Team: "t", AllowedModels: []string{"qwen-coder"}})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req.WithContext(ctx))
	if rec.Code != 403 {
		t.Fatalf("allow-list violation must be 403, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestMessagesAllowsListedModel(t *testing.T) {
	h := NewMessagesHandler(testRouter())
	req := httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}`))
	ctx := principal.With(req.Context(), keystore.Principal{KeyID: "ik", Team: "t", AllowedModels: []string{"*"}})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req.WithContext(ctx))
	if rec.Code != 200 {
		t.Fatalf("listed model should pass, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestMessagesNoPrincipal401(t *testing.T) {
	h := NewMessagesHandler(testRouter())
	req := httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"claude-sonnet-4-6","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req) // no principal injected
	if rec.Code != 401 {
		t.Fatalf("missing principal must be 401, got %d", rec.Code)
	}
}

func TestMessages404UnknownModelAudited(t *testing.T) {
	var buf bytes.Buffer
	w, _ := audit.NewWriter("i", filepath.Join(t.TempDir(), "a.wal"), []audit.Sink{audit.NewWriterSink("b", &buf, true)})
	h := NewMessagesHandlerWithAudit(testRouter(), w)
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"ghost-model","messages":[]}`))
	ctx := principal.With(req.Context(), keystore.Principal{KeyID: "ik", Team: "t", AllowedModels: []string{"*"}})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req.WithContext(ctx))
	w.Close()
	if rec.Code != 404 {
		t.Fatalf("want 404, got %d", rec.Code)
	}
	if !strings.Contains(buf.String(), `"status":404`) {
		t.Fatalf("404 must be audited: %s", buf.String())
	}
}

// govPricing keys the rate table by (config-provider-name, upstream-model),
// matching how the router's ResolveProvider returns the pricing provider name.
// testRouter() uses provider config name "p" and upstream "claude-sonnet-4-6".
func govPricing() *pricing.Table {
	return pricing.New(pricing.OnMissingAllow, map[pricing.Key]pricing.Rate{
		{Provider: "p", Model: "claude-sonnet-4-6"}: {InputPerMTok: 1_000_000, OutputPerMTok: 1_000_000},
	})
}

func TestMessagesGovernorQuotaBlocks429(t *testing.T) {
	lim := limiter.NewMemory()
	teams := map[string]governance.TeamPolicy{
		"platform-eng": {TokensPerDay: 1000, QuotaExceeded: "block"},
	}
	gov := governance.NewGovernor(teams, lim, budget.NewMemory(), govPricing())
	// Exhaust the team's daily token quota so the pre-check blocks.
	lim.DebitQuota("quota:platform-eng", 1000, 24*time.Hour)

	h := NewMessagesHandlerFull(testRouter(), nil, gov)
	req := httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}`))
	ctx := principal.With(req.Context(), keystore.Principal{KeyID: "ik", Team: "platform-eng", AllowedModels: []string{"*"}})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req.WithContext(ctx))
	if rec.Code != 429 {
		t.Fatalf("quota-exhausted request must be 429, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "rate_limit_error") {
		t.Fatalf("expected anthropic rate_limit_error body: %s", rec.Body.String())
	}
}

func TestMessagesGovernorSettlesCostIntoAudit(t *testing.T) {
	var buf bytes.Buffer
	w, err := audit.NewWriter("inst-1", filepath.Join(t.TempDir(), "a.wal"), []audit.Sink{audit.NewWriterSink("buf", &buf, true)})
	if err != nil {
		t.Fatal(err)
	}
	teams := map[string]governance.TeamPolicy{
		"platform-eng": {TokensPerDay: 1_000_000, QuotaExceeded: "block"},
	}
	gov := governance.NewGovernor(teams, limiter.NewMemory(), budget.NewMemory(), govPricing())
	h := NewMessagesHandlerFull(testRouter(), w, gov)
	req := httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}`))
	ctx := principal.With(req.Context(), keystore.Principal{KeyID: "ik", Team: "platform-eng", AllowedModels: []string{"*"}})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req.WithContext(ctx))
	w.Close()
	if rec.Code != 200 {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	// mock provider reports input=10 output=5 → 10*1 + 5*1 = 15 µUSD.
	out := buf.String()
	if !strings.Contains(out, `"amount_usd_micros":15`) {
		t.Fatalf("audit completed record must carry settled cost: %s", out)
	}
	if !strings.Contains(out, `"pricing_missing":false`) {
		t.Fatalf("pricing present → pricing_missing must be false: %s", out)
	}
}

func TestMessagesEmitsTwoPhaseAudit(t *testing.T) {
	var buf bytes.Buffer
	w, err := audit.NewWriter("inst-1", filepath.Join(t.TempDir(), "a.wal"), []audit.Sink{audit.NewWriterSink("buf", &buf, true)})
	if err != nil {
		t.Fatal(err)
	}
	h := NewMessagesHandlerWithAudit(testRouter(), w)
	req := httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}`))
	ctx := principal.With(req.Context(), keystore.Principal{KeyID: "ik_x", Team: "platform-eng", AllowedModels: []string{"*"}})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req.WithContext(ctx))
	w.Close() // flush

	out := buf.String()
	if !strings.Contains(out, `"request_started"`) || !strings.Contains(out, `"request_completed"`) {
		t.Fatalf("expected both audit phases, got: %s", out)
	}
	if !strings.Contains(out, `"ik_x"`) || !strings.Contains(out, `"platform-eng"`) {
		t.Fatalf("audit missing principal: %s", out)
	}
}
