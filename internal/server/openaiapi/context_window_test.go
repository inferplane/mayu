package openaiapi

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/inferplane/inferplane/internal/config"
	"github.com/inferplane/inferplane/internal/live"
	"github.com/inferplane/inferplane/internal/pricing"
	"github.com/inferplane/inferplane/internal/router"
	"github.com/inferplane/inferplane/providers"
	"github.com/inferplane/inferplane/providers/testing/mockprovider"
)

// GET /v1/models (OpenAI shape) advertises a declared window under both the
// context_window and max_model_len (vLLM spelling) keys, and omits both when
// undeclared.
func TestOpenAIModelsAdvertiseContextWindow(t *testing.T) {
	build := func(window int64) *router.Router {
		provs := map[string]providers.Provider{"p": mockprovider.New("m1")}
		models := map[string]config.ModelConfig{
			"m1": {Targets: []config.Target{{Provider: "p", Model: "m1"}}, ContextWindow: window},
		}
		h := &live.Holder{}
		h.Swap(live.NewState(provs, models, pricing.New(pricing.OnMissingAllow, nil), map[string]string{"p": "p"}))
		return router.New(h)
	}
	rec := httptest.NewRecorder()
	NewModelsHandler(build(202752)).ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	var out struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Data[0]["context_window"] != float64(202752) || out.Data[0]["max_model_len"] != float64(202752) {
		t.Fatalf("want both window keys: %+v", out.Data[0])
	}

	rec = httptest.NewRecorder()
	NewModelsHandler(build(0)).ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	out.Data = nil
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if _, ok := out.Data[0]["context_window"]; ok {
		t.Fatalf("undeclared window must be omitted: %+v", out.Data[0])
	}
}
