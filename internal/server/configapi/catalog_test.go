package configapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func getCatalog(t *testing.T, url string) (int, []string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, url, nil)
	rr := httptest.NewRecorder()
	CatalogHandler().ServeHTTP(rr, req)
	var body struct {
		Models []string `json:"models"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	return rr.Code, body.Models
}

func TestCatalog_KnownTypeNonEmpty(t *testing.T) {
	code, models := getCatalog(t, "/admin/providers/catalog?type=anthropic")
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d", code)
	}
	if len(models) == 0 {
		t.Fatal("anthropic catalog should not be empty")
	}
}

func TestCatalog_UnknownTypeEmpty(t *testing.T) {
	code, models := getCatalog(t, "/admin/providers/catalog?type=does-not-exist")
	if code != http.StatusOK {
		t.Fatalf("unknown type should be 200, got %d", code)
	}
	if len(models) != 0 {
		t.Fatalf("unknown type should be empty, got %v", models)
	}
}

func TestCatalog_MissingType400(t *testing.T) {
	code, _ := getCatalog(t, "/admin/providers/catalog")
	if code != http.StatusBadRequest {
		t.Fatalf("missing type should be 400, got %d", code)
	}
}
