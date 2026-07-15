package bedrockapi

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/inferplane/inferplane/internal/config"
	"github.com/inferplane/inferplane/internal/router"
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
