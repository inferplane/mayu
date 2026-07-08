package adminapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/inferplane/inferplane/internal/alert"
)

func TestAlertsHandler_GET_returnsFires(t *testing.T) {
	recent := func() []alert.Fire {
		return []alert.Fire{{TS: "t1", Team: "acme", Threshold: 0.8, Ratio: 0.85, Delivered: true}}
	}
	h := AlertsHandler(recent)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/alerts/recent", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type = %q", ct)
	}
	var body struct {
		Fires []alert.Fire `json:"fires"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Fires) != 1 || body.Fires[0].Team != "acme" {
		t.Fatalf("got %+v", body.Fires)
	}
}

func TestAlertsHandler_EmptyRecentIsEmptyArrayNotNull(t *testing.T) {
	h := AlertsHandler(func() []alert.Fire { return nil })
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/alerts/recent", nil))
	if strings.Contains(rec.Body.String(), `"fires":null`) {
		t.Fatalf("fires must serialize as [] not null: %s", rec.Body.String())
	}
}

func TestAlertsHandler_NilRecentFunc(t *testing.T) {
	h := AlertsHandler(nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/alerts/recent", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 even with nil recent func", rec.Code)
	}
}

func TestAlertsHandler_rejectsNonGET(t *testing.T) {
	h := AlertsHandler(func() []alert.Fire { return nil })
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/alerts/recent", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}
