package openaiapi

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/principal"
)

func TestModelsOpenAIShape(t *testing.T) {
	h := NewModelsHandler(testRouter())
	req := httptest.NewRequest("GET", "/v1/models", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out struct {
		Object string           `json:"object"`
		Data   []map[string]any `json:"data"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Object != "list" || len(out.Data) != 1 || out.Data[0]["object"] != "model" || out.Data[0]["id"] != "gpt-x" {
		t.Fatalf("OpenAI models shape wrong: %s", rec.Body.String())
	}
}

func TestModelsOpenAIFiltersByAllowList(t *testing.T) {
	h := NewModelsHandler(testRouter())
	req := httptest.NewRequest("GET", "/v1/models", nil)
	ctx := principal.With(req.Context(), keystore.Principal{AllowedModels: []string{"other-only"}})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req.WithContext(ctx))
	var out struct {
		Data []map[string]any `json:"data"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Data) != 0 {
		t.Fatalf("allow-list should filter out non-listed models: %+v", out.Data)
	}
}
