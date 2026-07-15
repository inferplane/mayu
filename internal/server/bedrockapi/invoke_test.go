package bedrockapi

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/inferplane/inferplane/internal/budget"
	"github.com/inferplane/inferplane/internal/config"
	"github.com/inferplane/inferplane/internal/governance"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/limiter"
	"github.com/inferplane/inferplane/internal/principal"
	"github.com/inferplane/inferplane/internal/router"
	"github.com/inferplane/inferplane/pkg/schema"
	"github.com/inferplane/inferplane/providers"
)

func allowAll(req *http.Request) *http.Request {
	return req.WithContext(principal.With(req.Context(),
		keystore.Principal{AllowedModels: []string{"*"}}))
}

func ptrStr(s string) *string { return &s }

// captureProvider records the ProxyRequest it receives so tests can assert
// the ingress forwarded RawBody verbatim (§4.4). Name() is "mock" so it
// passes the servesBedrockIngress filter (test-only allowance).
type captureProvider struct {
	last *providers.ProxyRequest
}

func (c *captureProvider) Name() string               { return "mock" }
func (c *captureProvider) Models() []schema.ModelInfo { return nil }
func (c *captureProvider) Complete(_ context.Context, req *providers.ProxyRequest) (*providers.ProxyResponse, error) {
	c.last = req
	in, out := int64(10), int64(5)
	resp := &schema.ChatResponse{
		ID: "msg_capture", Type: "message", Role: "assistant", Model: "claude-x",
		Content:    []schema.ContentBlock{{Type: "text", Text: ptrStr("ok")}},
		StopReason: ptrStr("end_turn"),
		Usage:      &schema.Usage{InputTokens: &in, OutputTokens: &out},
	}
	raw, _ := json.Marshal(resp)
	return &providers.ProxyResponse{StatusCode: 200, RawBody: raw, Parsed: resp}, nil
}
func (c *captureProvider) Stream(context.Context, *providers.ProxyRequest) (iter.Seq2[*providers.StreamEvent, error], error) {
	return nil, errors.New("unused")
}

// nonBedrockProvider simulates an anthropic-direct target: its Name() fails
// the servesBedrockIngress filter, so the Bedrock ingress must 404 rather
// than send it a Bedrock-shaped (model-less) body it cannot serve.
type nonBedrockProvider struct{}

func (nonBedrockProvider) Name() string               { return "anthropic" }
func (nonBedrockProvider) Models() []schema.ModelInfo { return nil }
func (nonBedrockProvider) Complete(context.Context, *providers.ProxyRequest) (*providers.ProxyResponse, error) {
	return nil, errors.New("must never be reached from the bedrock ingress")
}
func (nonBedrockProvider) Stream(context.Context, *providers.ProxyRequest) (iter.Seq2[*providers.StreamEvent, error], error) {
	return nil, errors.New("must never be reached from the bedrock ingress")
}

// upstreamErrProvider returns an Anthropic-SHAPED UpstreamError body, exactly
// what providers/bedrock's upstreamError() synthesizes — the ingress must
// re-wrap it into AWS shape, never tee it verbatim.
type upstreamErrProvider struct{}

func (upstreamErrProvider) Name() string               { return "mock" }
func (upstreamErrProvider) Models() []schema.ModelInfo { return nil }
func (upstreamErrProvider) Complete(context.Context, *providers.ProxyRequest) (*providers.ProxyResponse, error) {
	return nil, &providers.UpstreamError{StatusCode: 429, Body: []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"bedrock upstream error (ThrottlingException)"}}`)}
}
func (upstreamErrProvider) Stream(context.Context, *providers.ProxyRequest) (iter.Seq2[*providers.StreamEvent, error], error) {
	return nil, errors.New("unused")
}

func invokeReq(modelID, body string) *http.Request {
	req := httptest.NewRequest("POST", "/model/"+modelID+"/invoke", strings.NewReader(body))
	req.SetPathValue("modelId", modelID)
	return req
}

func TestInvokeNonStreamingRoundTripVerbatimBody(t *testing.T) {
	cap := &captureProvider{}
	provs := map[string]providers.Provider{"p": cap}
	models := map[string]config.ModelConfig{
		"claude-x": {Targets: []config.Target{{Provider: "p", Model: "global.anthropic.claude-x-v1:0"}}},
	}
	h := holderFor(provs, models)
	handler := NewInvokeHandler(router.New(h), h, false)

	// A realistic Bedrock-mode client body: no top-level model/stream, carries
	// anthropic_version, and a cache_control block whose bytes must survive.
	body := `{"anthropic_version":"bedrock-2023-05-31","max_tokens":16,"system":[{"type":"text","text":"sys","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, allowAll(invokeReq("global.anthropic.claude-x-v1:0", body)))

	if rec.Code != 200 {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if cap.last == nil {
		t.Fatal("provider never called")
	}
	// §4.4: the ingress must forward the client's bytes UNMODIFIED.
	if string(cap.last.RawBody) != body {
		t.Fatalf("RawBody not verbatim:\n got: %s\nwant: %s", cap.last.RawBody, body)
	}
	if cap.last.Model != "claude-x" {
		t.Fatalf("Model not canonical: %q", cap.last.Model)
	}
	if cap.last.Stream {
		t.Fatal("Stream must be false on the /invoke route")
	}
	if cap.last.IngressProtocol != "bedrock" {
		t.Fatalf("IngressProtocol = %q, want bedrock", cap.last.IngressProtocol)
	}
	// Bedrock duplicates token counts into response headers.
	if got := rec.Header().Get("X-Amzn-Bedrock-Input-Token-Count"); got != "10" {
		t.Fatalf("input token header = %q, want 10", got)
	}
	if got := rec.Header().Get("X-Amzn-Bedrock-Output-Token-Count"); got != "5" {
		t.Fatalf("output token header = %q, want 5", got)
	}
	if !strings.Contains(rec.Body.String(), `"msg_capture"`) {
		t.Fatalf("provider RawBody not teed verbatim: %s", rec.Body.String())
	}
}

func TestInvokeMissingPrincipal401AWSShape(t *testing.T) {
	provs := map[string]providers.Provider{"p": &captureProvider{}}
	models := map[string]config.ModelConfig{
		"claude-x": {Targets: []config.Target{{Provider: "p", Model: "up"}}},
	}
	h := holderFor(provs, models)
	handler := NewInvokeHandler(router.New(h), h, false)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, invokeReq("claude-x", `{"messages":[]}`)) // no principal
	if rec.Code != 401 {
		t.Fatalf("status %d", rec.Code)
	}
	if rec.Header().Get("X-Amzn-ErrorType") == "" {
		t.Fatalf("401 must be AWS-shaped (X-Amzn-ErrorType): %s", rec.Body.String())
	}
}

func TestInvokeDisallowedModel403AWSShape(t *testing.T) {
	provs := map[string]providers.Provider{"p": &captureProvider{}}
	models := map[string]config.ModelConfig{
		"claude-x": {Targets: []config.Target{{Provider: "p", Model: "up"}}},
	}
	h := holderFor(provs, models)
	handler := NewInvokeHandler(router.New(h), h, false)
	req := invokeReq("claude-x", `{"messages":[]}`)
	req = req.WithContext(principal.With(req.Context(),
		keystore.Principal{AllowedModels: []string{"some-other-model"}}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("status %d", rec.Code)
	}
	if rec.Header().Get("X-Amzn-ErrorType") == "" {
		t.Fatalf("403 must be AWS-shaped: %s", rec.Body.String())
	}
}

func TestInvokeUnknownModel404NeverEchoesRawID(t *testing.T) {
	provs := map[string]providers.Provider{"p": &captureProvider{}}
	models := map[string]config.ModelConfig{
		"claude-x": {Targets: []config.Target{{Provider: "p", Model: "up"}}},
	}
	h := holderFor(provs, models)
	handler := NewInvokeHandler(router.New(h), h, false)
	const evil = "attacker-controlled-model-id"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, allowAll(invokeReq(evil, `{"messages":[]}`)))
	if rec.Code != 404 {
		t.Fatalf("status %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), evil) {
		t.Fatalf("raw URL modelId echoed into the error body: %s", rec.Body.String())
	}
}

func TestInvokeNonBedrockTargetFiltered404(t *testing.T) {
	// The model resolves, but its only target is an anthropic-wire provider —
	// the bedrock ingress must not fall through to it (v1 scope: bedrock
	// targets only), because a Bedrock-shaped body has no top-level model.
	provs := map[string]providers.Provider{"p": nonBedrockProvider{}}
	models := map[string]config.ModelConfig{
		"claude-x": {Targets: []config.Target{{Provider: "p", Model: "claude-x"}}},
	}
	h := holderFor(provs, models)
	handler := NewInvokeHandler(router.New(h), h, false)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, allowAll(invokeReq("claude-x", `{"messages":[]}`)))
	if rec.Code != 404 {
		t.Fatalf("non-bedrock-only model must 404 on the bedrock ingress, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestInvokeGovernorQuotaBlocks429AWSShape(t *testing.T) {
	lim := limiter.NewMemory()
	teams := map[string]governance.TeamPolicy{
		"platform-eng": {TokensPerDay: 1000, QuotaExceeded: "block"},
	}
	gov := governance.NewGovernor(teams, lim, budget.NewMemory(), nil)
	lim.DebitQuota("quota:platform-eng", 1000, 24*time.Hour)

	provs := map[string]providers.Provider{"p": &captureProvider{}}
	models := map[string]config.ModelConfig{
		"claude-x": {Targets: []config.Target{{Provider: "p", Model: "up"}}},
	}
	h := holderFor(provs, models)
	handler := NewInvokeHandlerMetrics(router.New(h), h, nil, gov, nil, false)
	req := invokeReq("claude-x", `{"messages":[{"role":"user","content":"hi"}]}`)
	req = req.WithContext(principal.With(req.Context(),
		keystore.Principal{KeyID: "ik", Team: "platform-eng", AllowedModels: []string{"*"}}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 429 {
		t.Fatalf("quota-exhausted request must be 429, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Amzn-ErrorType") == "" {
		t.Fatalf("governance deny must be AWS-shaped on this ingress: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "rate_limit_error") {
		t.Fatalf("anthropic error vocabulary leaked: %s", rec.Body.String())
	}
}

func TestInvokeUpstreamErrorRewrappedAWSShape(t *testing.T) {
	provs := map[string]providers.Provider{"p": upstreamErrProvider{}}
	models := map[string]config.ModelConfig{
		"claude-x": {Targets: []config.Target{{Provider: "p", Model: "up"}}},
	}
	h := holderFor(provs, models)
	handler := NewInvokeHandler(router.New(h), h, false)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, allowAll(invokeReq("claude-x", `{"messages":[]}`)))
	if rec.Code != 429 {
		t.Fatalf("upstream status must be preserved, got %d", rec.Code)
	}
	if rec.Header().Get("X-Amzn-ErrorType") == "" {
		t.Fatal("upstream error must be re-wrapped AWS-shaped, not teed")
	}
	if strings.Contains(rec.Body.String(), `"type":"error"`) || strings.Contains(rec.Body.String(), "rate_limit_error") {
		t.Fatalf("anthropic-shaped upstream body teed verbatim: %s", rec.Body.String())
	}
}
