package anthropicapi

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/principal"
)

func TestModelsListShape(t *testing.T) {
	h := NewModelsHandler(testRouter()) // testRouter from messages_test.go (same package)
	req := httptest.NewRequest("GET", "/v1/models", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	var out struct {
		Data    []map[string]any `json:"data"`
		HasMore bool             `json:"has_more"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(out.Data) != 1 || out.Data[0]["id"] != "claude-sonnet-4-6" || out.Data[0]["type"] != "model" {
		t.Fatalf("unexpected data: %+v", out.Data)
	}
}

func TestModelsFiltersByAllowList(t *testing.T) {
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
