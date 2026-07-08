package configapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCapabilitiesHandler_GET_returnsJSON(t *testing.T) {
	h := CapabilitiesHandler(func() Capabilities {
		return Capabilities{AnalyticsIndex: "off", ProviderStore: true, Guardrails: true}
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/capabilities", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type = %q, want application/json", ct)
	}
	var got Capabilities
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.AnalyticsIndex != "off" || !got.ProviderStore || !got.Guardrails {
		t.Fatalf("got %+v, want analytics_index=off provider_store=true guardrails=true", got)
	}
	// JSON keys must be the snake_case contract the console reads.
	for _, key := range []string{"analytics_index", "logs_bodies", "teams_records", "key_governance_fields", "provider_store", "region_policy", "guardrails", "budget_alerts"} {
		if !strings.Contains(rec.Body.String(), `"`+key+`"`) {
			t.Fatalf("response missing key %q: %s", key, rec.Body.String())
		}
	}
}

func TestCapabilitiesHandler_rejectsNonGET(t *testing.T) {
	h := CapabilitiesHandler(func() Capabilities { return Capabilities{} })
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/capabilities", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}
