package anthropicapi

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/inferplane/inferplane/internal/config"
	"github.com/inferplane/inferplane/internal/router"
	"github.com/inferplane/inferplane/providers"
	"github.com/inferplane/inferplane/providers/testing/mockprovider"
)

// estimatorRouter uses mockprovider which does NOT implement TokenCounter,
// forcing the estimator fallback path.
func estimatorRouter() *router.Router {
	provs := map[string]providers.Provider{"p": mockprovider.New("claude-sonnet-4-6")}
	models := map[string]config.ModelConfig{
		"claude-sonnet-4-6": {Targets: []config.Target{{Provider: "p", Model: "claude-sonnet-4-6"}}},
	}
	return router.New(holderFor(provs, models))
}

func TestCountTokensAlwaysValidJSON(t *testing.T) {
	h := NewCountTokensHandler(estimatorRouter())
	body := `{"model":"no-such-model","messages":[{"role":"user","content":"hello world"}]}`
	req := httptest.NewRequest("POST", "/v1/messages/count_tokens", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("count_tokens must never return non-200; got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"input_tokens"`) {
		t.Fatalf("missing input_tokens: %s", rec.Body.String())
	}
}

func TestCountTokensEstimatorFallback(t *testing.T) {
	h := NewCountTokensHandler(estimatorRouter())
	body := `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/messages/count_tokens", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"input_tokens"`) {
		t.Fatalf("estimator fallback failed: %d %s", rec.Code, rec.Body.String())
	}
}
