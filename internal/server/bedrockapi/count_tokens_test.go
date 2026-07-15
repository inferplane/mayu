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

	"github.com/inferplane/inferplane/internal/config"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/principal"
	"github.com/inferplane/inferplane/internal/router"
	"github.com/inferplane/inferplane/pkg/schema"
	"github.com/inferplane/inferplane/providers"
)

// AWS wire shape (API_runtime_CountTokens, confirmed 2026-07-16):
//   POST /model/{modelId}/count-tokens
//   request  {"input":{"invokeModel":{"body":"<base64 of the InvokeModel body JSON>"}}}
//   response {"inputTokens": <int>}
// Claude Code's /context calls this and CRASHES on any non-200 — the same
// never-non-200 mandate as anthropicapi's count_tokens.

func countTokensReq(modelID, body string) *http.Request {
	req := httptest.NewRequest("POST", "/model/"+modelID+"/count-tokens", strings.NewReader(body))
	req.SetPathValue("modelId", modelID)
	return req
}

func countTokensBody(t *testing.T, inner string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"input": map[string]any{
			"invokeModel": map[string]any{
				"body": base64.StdEncoding.EncodeToString([]byte(inner)),
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func assert200InputTokens(t *testing.T, rec *httptest.ResponseRecorder) int64 {
	t.Helper()
	if rec.Code != 200 {
		t.Fatalf("count-tokens must NEVER return non-200 (Claude Code crashes), got %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		InputTokens *int64 `json:"inputTokens"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.InputTokens == nil {
		t.Fatalf("response must be AWS-shaped {\"inputTokens\": N}: %s", rec.Body.String())
	}
	if *out.InputTokens < 1 {
		t.Fatalf("inputTokens must be >= 1, got %d", *out.InputTokens)
	}
	return *out.InputTokens
}

func newCountTokensTestHandler() *CountTokensHandler {
	provs := map[string]providers.Provider{"p": &captureProvider{}}
	models := map[string]config.ModelConfig{
		"claude-x": {Targets: []config.Target{{Provider: "p", Model: "global.anthropic.claude-x-v1:0"}}},
	}
	h := holderFor(provs, models)
	return NewCountTokensHandler(router.New(h), h)
}

func TestCountTokensHappyPath(t *testing.T) {
	h := newCountTokensTestHandler()
	inner := `{"anthropic_version":"bedrock-2023-05-31","messages":[{"role":"user","content":"hello world, count my tokens please"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, allowAll(countTokensReq("global.anthropic.claude-x-v1:0", countTokensBody(t, inner))))
	assert200InputTokens(t, rec)
}

func TestCountTokensBrokenBodyStill200(t *testing.T) {
	h := newCountTokensTestHandler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, allowAll(countTokensReq("claude-x", `{not json at all`)))
	assert200InputTokens(t, rec)
}

func TestCountTokensBadBase64Still200(t *testing.T) {
	h := newCountTokensTestHandler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, allowAll(countTokensReq("claude-x", `{"input":{"invokeModel":{"body":"%%%not-base64%%%"}}}`)))
	assert200InputTokens(t, rec)
}

func TestCountTokensUnknownModelStill200(t *testing.T) {
	h := newCountTokensTestHandler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, allowAll(countTokensReq("never-registered-model", countTokensBody(t, `{"messages":[{"role":"user","content":"hi"}]}`))))
	assert200InputTokens(t, rec)
}

func TestCountTokensNoPrincipalStill200(t *testing.T) {
	// Even an unauthenticated-context call must not crash the client; KeyAuth
	// normally guards the route, but the handler itself stays fail-open-to-200.
	h := newCountTokensTestHandler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, countTokensReq("claude-x", countTokensBody(t, `{"messages":[{"role":"user","content":"hi"}]}`)))
	assert200InputTokens(t, rec)
}

// tcProvider implements providers.TokenCounter with a distinctive count so
// tests can tell a real upstream count (777) from the local estimate.
type tcProvider struct{ called bool }

func (t *tcProvider) Name() string               { return "mock" }
func (t *tcProvider) Models() []schema.ModelInfo { return nil }
func (t *tcProvider) Complete(context.Context, *providers.ProxyRequest) (*providers.ProxyResponse, error) {
	return nil, errors.New("unused")
}
func (t *tcProvider) Stream(context.Context, *providers.ProxyRequest) (iter.Seq2[*providers.StreamEvent, error], error) {
	return nil, errors.New("unused")
}
func (t *tcProvider) CountTokens(context.Context, *providers.ProxyRequest) (int64, error) {
	t.called = true
	return 777, nil
}

// A key must not trigger a real upstream CountTokens call for a model outside
// its allow-list (CI review HIGH finding): the handler falls back to the local
// estimate — still 200 — and the upstream counter is never called.
func TestCountTokensDisallowedModelSkipsUpstream(t *testing.T) {
	tc := &tcProvider{}
	provs := map[string]providers.Provider{"p": tc}
	models := map[string]config.ModelConfig{
		"claude-x": {Targets: []config.Target{{Provider: "p", Model: "up"}}},
	}
	h := holderFor(provs, models)
	handler := NewCountTokensHandler(router.New(h), h)

	req := countTokensReq("claude-x", countTokensBody(t, `{"messages":[{"role":"user","content":"hi"}]}`))
	req = req.WithContext(principal.With(req.Context(),
		keystore.Principal{AllowedModels: []string{"some-other-model"}}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	got := assert200InputTokens(t, rec)
	if tc.called {
		t.Fatal("upstream CountTokens must not be called for a model outside the key's allow-list")
	}
	if got == 777 {
		t.Fatal("response leaked the upstream count for a disallowed model")
	}
}

// The allowed-key path still reaches the real upstream counter.
func TestCountTokensAllowedModelUsesUpstream(t *testing.T) {
	tc := &tcProvider{}
	provs := map[string]providers.Provider{"p": tc}
	models := map[string]config.ModelConfig{
		"claude-x": {Targets: []config.Target{{Provider: "p", Model: "up"}}},
	}
	h := holderFor(provs, models)
	handler := NewCountTokensHandler(router.New(h), h)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, allowAll(countTokensReq("claude-x", countTokensBody(t, `{"messages":[{"role":"user","content":"hi"}]}`))))
	if got := assert200InputTokens(t, rec); got != 777 {
		t.Fatalf("allowed key should get the upstream count 777, got %d", got)
	}
	if !tc.called {
		t.Fatal("upstream CountTokens was not called for an allowed model")
	}
}
