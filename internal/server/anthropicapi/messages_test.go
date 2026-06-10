package anthropicapi

import (
	"context"
	"errors"
	"iter"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/inferplane/inferplane/internal/config"
	"github.com/inferplane/inferplane/internal/router"
	"github.com/inferplane/inferplane/pkg/schema"
	"github.com/inferplane/inferplane/providers"
	"github.com/inferplane/inferplane/providers/testing/mockprovider"
)

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
	h.ServeHTTP(rec, req)
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
	h.ServeHTTP(rec, req)
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
	h.ServeHTTP(rec, req)
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
	h.ServeHTTP(rec, req)
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
	h.ServeHTTP(rec, req)
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
	h.ServeHTTP(rec, req)
	if rec.Code != 429 || rec.Header().Get("Retry-After") != "30" {
		t.Fatalf("streaming error headers not teed: code=%d retry-after=%q", rec.Code, rec.Header().Get("Retry-After"))
	}
}
