package anthropicapi

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/inferplane/inferplane/internal/config"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/live"
	"github.com/inferplane/inferplane/internal/pricing"
	"github.com/inferplane/inferplane/internal/principal"
	"github.com/inferplane/inferplane/internal/router"
	"github.com/inferplane/inferplane/providers"
	"github.com/inferplane/inferplane/providers/testing/mockprovider"
)

func ctxWindowRouter(window int64) *router.Router {
	provs := map[string]providers.Provider{"p": mockprovider.New("claude-x")}
	models := map[string]config.ModelConfig{
		"claude-x": {Targets: []config.Target{{Provider: "p", Model: "claude-x"}}, ContextWindow: window, Aliases: []string{"cx"}},
	}
	ids := map[string]string{"p": "p"}
	h := &live.Holder{}
	h.Swap(live.NewState(provs, models, pricing.New(pricing.OnMissingAllow, nil), ids))
	return router.New(h)
}

// An input whose coarse estimate exceeds the declared window is refused with
// a CLEAR 400 naming both numbers, BEFORE any provider call — the upstream's
// own rejection is an opaque scrubbed ValidationException.
func TestMessagesContextWindowFastFail(t *testing.T) {
	h := NewMessagesHandler(ctxWindowRouter(100)) // 100-token window
	body := `{"model":"claude-x","max_tokens":16,"messages":[{"role":"user","content":"` + strings.Repeat("word ", 400) + `"}]}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	req = req.WithContext(principal.With(req.Context(), keystore.Principal{Team: "t", AllowedModels: []string{"*"}}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "100-token context window") {
		t.Fatalf("error must name the declared window: %s", rec.Body.String())
	}
}

// No declared window (0) = no gateway pre-flight: the same oversized body goes
// through to the provider (mock answers 200).
func TestMessagesNoWindowNoPreflight(t *testing.T) {
	h := NewMessagesHandler(ctxWindowRouter(0))
	body := `{"model":"claude-x","max_tokens":16,"messages":[{"role":"user","content":"` + strings.Repeat("word ", 400) + `"}]}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	req = req.WithContext(principal.With(req.Context(), keystore.Principal{Team: "t", AllowedModels: []string{"*"}}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("undeclared window must not pre-flight: %d %s", rec.Code, rec.Body.String())
	}
}

// GET /v1/models advertises the declared window (gateway extension) and omits
// it when undeclared.
func TestModelsAdvertiseContextWindow(t *testing.T) {
	h := NewModelsHandler(ctxWindowRouter(872000))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	var out struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Data) != 1 || out.Data[0]["context_window"] != float64(872000) {
		t.Fatalf("models entry must carry context_window: %+v", out.Data)
	}

	h = NewModelsHandler(ctxWindowRouter(0))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	out.Data = nil
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if _, present := out.Data[0]["context_window"]; present {
		t.Fatalf("undeclared window must be omitted: %+v", out.Data)
	}
}

// Alias resolution: the window declared on the canonical name applies to its
// alias too.
func TestContextWindowResolvesAliases(t *testing.T) {
	r := ctxWindowRouter(1234)
	if got := r.ContextWindow("cx"); got != 1234 {
		t.Fatalf("ContextWindow(alias) = %d, want 1234", got)
	}
	if got := r.ContextWindow("never-configured"); got != 0 {
		t.Fatalf("ContextWindow(unknown) = %d, want 0", got)
	}
}
