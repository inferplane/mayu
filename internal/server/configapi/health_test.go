package configapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/inferplane/inferplane/providers"
)

func TestHealthStore_SetAndSnapshot(t *testing.T) {
	s := NewHealthStore()
	at := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	s.Set("acme-provider", providers.HealthResult{OK: true, LatencyMS: 42, Detail: "ok"}, at)

	snap := s.Snapshot()
	rec, ok := snap["acme-provider"]
	if !ok {
		t.Fatal("Snapshot missing the set record")
	}
	if !rec.OK || rec.LatencyMS != 42 || rec.Detail != "ok" {
		t.Fatalf("record fields wrong: %+v", rec)
	}
	if rec.LastProbedAt != "2026-07-12T12:00:00Z" {
		t.Fatalf("LastProbedAt = %q, want RFC3339Nano of 2026-07-12T12:00:00Z", rec.LastProbedAt)
	}
}

func TestHealthStore_SnapshotIsACopy(t *testing.T) {
	s := NewHealthStore()
	s.Set("p", providers.HealthResult{OK: true}, time.Now())

	snap := s.Snapshot()
	snap["p"] = HealthRecord{OK: false, Detail: "mutated"}

	snap2 := s.Snapshot()
	if !snap2["p"].OK {
		t.Fatal("mutating a returned snapshot must not affect a later Snapshot() call")
	}
}

func doHealth(t *testing.T, h http.Handler, method string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, "/admin/providers/health", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	var out map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return rr.Code, out
}

func TestHealthHandler_GET(t *testing.T) {
	s := NewHealthStore()
	s.Set("acme-provider", providers.HealthResult{OK: true, LatencyMS: 5, Detail: "ok"}, time.Now())

	code, out := doHealth(t, HealthHandler(s.Snapshot), http.MethodGet)
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d", code)
	}
	provMap, ok := out["providers"].(map[string]any)
	if !ok {
		t.Fatalf("response missing providers map: %+v", out)
	}
	if _, ok := provMap["acme-provider"]; !ok {
		t.Fatalf("providers map missing acme-provider: %+v", provMap)
	}
}

func TestHealthHandler_RejectsNonGET(t *testing.T) {
	s := NewHealthStore()
	code, _ := doHealth(t, HealthHandler(s.Snapshot), http.MethodPost)
	if code != http.StatusMethodNotAllowed {
		t.Fatalf("want 405, got %d", code)
	}
}

func TestHealthHandler_NilSnapshot(t *testing.T) {
	code, out := doHealth(t, HealthHandler(nil), http.MethodGet)
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d", code)
	}
	provMap, ok := out["providers"].(map[string]any)
	if !ok || len(provMap) != 0 {
		t.Fatalf("nil snapshot func must yield an empty providers map, got %+v", out)
	}
}
