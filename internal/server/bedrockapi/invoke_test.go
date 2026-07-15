package bedrockapi

import (
	"context"
	"encoding/base64"
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

// streamProvider yields a realistic chunk sequence including one nil-Chunk
// event (a keepalive/unparseable payload per providers/provider.go), which
// the eventstream writer must SKIP rather than emit as base64("null").
type streamProvider struct{}

func (streamProvider) Name() string               { return "mock" }
func (streamProvider) Models() []schema.ModelInfo { return nil }
func (streamProvider) Complete(context.Context, *providers.ProxyRequest) (*providers.ProxyResponse, error) {
	return nil, errors.New("unused")
}
func (streamProvider) Stream(context.Context, *providers.ProxyRequest) (iter.Seq2[*providers.StreamEvent, error], error) {
	in, out := int64(10), int64(5)
	events := []*providers.StreamEvent{
		{Raw: []byte("event: message_start\ndata: {\"type\":\"message_start\"}\n\n"), Chunk: &schema.ChatChunk{Type: "message_start"}},
		{Raw: []byte(": keepalive\n\n"), Chunk: nil}, // must be skipped
		{Raw: []byte("event: message_delta\ndata: {\"type\":\"message_delta\"}\n\n"), Chunk: &schema.ChatChunk{Type: "message_delta", Usage: &schema.Usage{InputTokens: &in, OutputTokens: &out}}},
		{Raw: []byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"), Chunk: &schema.ChatChunk{Type: "message_stop"}},
	}
	return func(yield func(*providers.StreamEvent, error) bool) {
		for _, ev := range events {
			if !yield(ev, nil) {
				return
			}
		}
	}, nil
}

func TestInvokeStreamingEmitsEventstream(t *testing.T) {
	provs := map[string]providers.Provider{"p": streamProvider{}}
	models := map[string]config.ModelConfig{
		"claude-x": {Targets: []config.Target{{Provider: "p", Model: "global.anthropic.claude-x-v1:0"}}},
	}
	h := holderFor(provs, models)
	handler := NewInvokeHandler(router.New(h), h, true)

	req := httptest.NewRequest("POST", "/model/claude-x/invoke-with-response-stream", strings.NewReader(`{"anthropic_version":"bedrock-2023-05-31","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	req.SetPathValue("modelId", "claude-x")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, allowAll(req))

	if rec.Code != 200 {
		t.Fatalf("status %d body %q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/vnd.amazon.eventstream" {
		t.Fatalf("Content-Type = %q — a Bedrock-mode client rejects anything else", got)
	}
	if got := rec.Header().Get("X-Amzn-Bedrock-Content-Type"); got != "application/json" {
		t.Fatalf("X-Amzn-Bedrock-Content-Type = %q", got)
	}
	msgs := decodeAll(t, rec.Body)
	if len(msgs) != 3 {
		t.Fatalf("%d frames, want 3 (nil-Chunk keepalive must be skipped)", len(msgs))
	}
	var types []string
	for _, m := range msgs {
		if got := headerStr(t, m, ":event-type"); got != "chunk" {
			t.Fatalf(":event-type = %q", got)
		}
		var payload struct {
			Bytes string `json:"bytes"`
		}
		if err := json.Unmarshal(m.Payload, &payload); err != nil {
			t.Fatalf("payload: %v", err)
		}
		decoded, err := base64.StdEncoding.DecodeString(payload.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(decoded), "event:") || strings.Contains(string(decoded), "data:") {
			t.Fatalf("SSE text leaked into frame payload: %s", decoded)
		}
		var c schema.ChatChunk
		if err := json.Unmarshal(decoded, &c); err != nil {
			t.Fatalf("frame payload is not a bare event JSON: %s", decoded)
		}
		types = append(types, c.Type)
	}
	if strings.Join(types, ",") != "message_start,message_delta,message_stop" {
		t.Fatalf("frame order: %v", types)
	}
}

// midStreamErrProvider yields one good chunk, then a mid-stream error — after
// the 200 is committed, the failure must surface as an exception frame (the
// status can no longer change) and the audit record must be Partial, never a
// clean completion (H4 gate finding, mirrors anthropicapi's partial path).
type midStreamErrProvider struct{}

func (midStreamErrProvider) Name() string               { return "mock" }
func (midStreamErrProvider) Models() []schema.ModelInfo { return nil }
func (midStreamErrProvider) Complete(context.Context, *providers.ProxyRequest) (*providers.ProxyResponse, error) {
	return nil, errors.New("unused")
}
func (midStreamErrProvider) Stream(context.Context, *providers.ProxyRequest) (iter.Seq2[*providers.StreamEvent, error], error) {
	return func(yield func(*providers.StreamEvent, error) bool) {
		if !yield(&providers.StreamEvent{Chunk: &schema.ChatChunk{Type: "message_start"}}, nil) {
			return
		}
		yield(nil, errors.New("upstream broke"))
	}, nil
}

func TestInvokeStreamingMidStreamErrorEmitsExceptionFrame(t *testing.T) {
	provs := map[string]providers.Provider{"p": midStreamErrProvider{}}
	models := map[string]config.ModelConfig{
		"claude-x": {Targets: []config.Target{{Provider: "p", Model: "up"}}},
	}
	h := holderFor(provs, models)
	handler := NewInvokeHandler(router.New(h), h, true)

	req := httptest.NewRequest("POST", "/model/claude-x/invoke-with-response-stream", strings.NewReader(`{"messages":[]}`))
	req.SetPathValue("modelId", "claude-x")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, allowAll(req))

	if rec.Code != 200 {
		t.Fatalf("status %d (already committed before the break)", rec.Code)
	}
	msgs := decodeAll(t, rec.Body)
	if len(msgs) != 2 {
		t.Fatalf("%d frames, want 2 (one chunk + one exception)", len(msgs))
	}
	if got := headerStr(t, msgs[0], ":message-type"); got != "event" {
		t.Fatalf("frame 1 :message-type = %q", got)
	}
	if got := headerStr(t, msgs[1], ":message-type"); got != "exception" {
		t.Fatalf("frame 2 :message-type = %q, want exception", got)
	}
	if got := headerStr(t, msgs[1], ":exception-type"); got != "internalServerException" {
		t.Fatalf(":exception-type = %q", got)
	}
}
