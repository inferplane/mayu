package usageapi

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/inferplane/inferplane/internal/budget"
	"github.com/inferplane/inferplane/internal/governance"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/limiter"
	"github.com/inferplane/inferplane/internal/principal"
)

func TestUsageHandlerReportsBudget(t *testing.T) {
	teams := map[string]governance.TeamPolicy{"t": {BudgetMicrosPerMonth: 1_000_000}}
	g := governance.NewGovernor(teams, limiter.NewMemory(), budget.NewMemory(), nil)
	h := NewHandler(g)

	req := httptest.NewRequest("GET", "/v1/usage", nil)
	ctx := principal.With(req.Context(), keystore.Principal{
		KeyID: "ik_secret", Team: "t",
		KeyOptions: keystore.KeyOptions{BudgetUSDMicros: 500_000},
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req.WithContext(ctx))
	if rec.Code != 200 {
		t.Fatalf("want 200, got %d body %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "ik_secret") {
		t.Fatalf("usage response must not leak key id: %s", body)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if got["team"] != "t" {
		t.Fatalf("team missing: %v", got)
	}
	// integer microUSD survives JSON round-trip (json numbers are float64 but
	// these are small exact integers).
	if !strings.Contains(body, "1000000") || !strings.Contains(body, "500000") {
		t.Fatalf("expected team + key budget micros in body: %s", body)
	}
}

func TestUsageHandlerNoPrincipal401(t *testing.T) {
	g := governance.NewGovernor(nil, limiter.NewMemory(), budget.NewMemory(), nil)
	h := NewHandler(g)
	req := httptest.NewRequest("GET", "/v1/usage", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req) // no principal
	if rec.Code != 401 {
		t.Fatalf("missing principal must be 401, got %d", rec.Code)
	}
}

// F4: a nil governor must not panic — return a well-formed ungoverned payload.
func TestUsageHandlerNilGovernor(t *testing.T) {
	h := NewHandler(nil)
	req := httptest.NewRequest("GET", "/v1/usage", nil)
	ctx := principal.With(req.Context(), keystore.Principal{KeyID: "ik", Team: "t"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req.WithContext(ctx))
	if rec.Code != 200 {
		t.Fatalf("nil governor must be 200 ungoverned, got %d", rec.Code)
	}
}
