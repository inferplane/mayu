package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/inferplane/inferplane/internal/adminauth"
	"github.com/inferplane/inferplane/internal/audit"
	"github.com/inferplane/inferplane/internal/principal"
	"github.com/inferplane/inferplane/internal/server/configapi"
)

func probeTestMux() http.Handler {
	// writer nil → provider store disabled → probe returns 405 (like the write path).
	return AdminMux(stubStore{}, []string{"admin-tok"}, nil, adminauth.MappingConfig{},
		func() configapi.View { return configapi.View{} }, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
}

func TestProbeRoute_RequiresAuth(t *testing.T) {
	mux := probeTestMux()
	// no token → 401
	req := httptest.NewRequest(http.MethodPost, "/admin/providers/test", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("no-token probe: want 401, got %d", rr.Code)
	}
	// with admin token → reaches handler; store disabled → 405
	req = httptest.NewRequest(http.MethodPost, "/admin/providers/test", nil)
	req.Header.Set("Authorization", "Bearer admin-tok")
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("authed probe (store off): want 405, got %d", rr.Code)
	}
}

func TestCatalogRoute_RequiresAuth(t *testing.T) {
	mux := probeTestMux()
	req := httptest.NewRequest(http.MethodGet, "/admin/providers/catalog?type=anthropic", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("no-token catalog: want 401, got %d", rr.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "/admin/providers/catalog?type=anthropic", nil)
	req.Header.Set("Authorization", "Bearer admin-tok")
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("authed catalog: want 200, got %d", rr.Code)
	}
}

func TestRequireAdmin(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })
	var recs []audit.Record
	emit := func(rec audit.Record) { recs = append(recs, rec) }
	h := requireAdmin(next, emit)

	// full admin → passes through, no denial audit
	req := httptest.NewRequest(http.MethodPost, "/x", nil)
	req = req.WithContext(principal.WithAdmin(req.Context(), principal.AdminIdentity{IsAdmin: true}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusTeapot {
		t.Fatalf("admin should pass, got %d", rr.Code)
	}
	if len(recs) != 0 {
		t.Fatalf("admin pass must not audit, got %d records", len(recs))
	}

	// non-admin (team-mapped) identity → 403 + audited admin_denied (§5.5)
	req = httptest.NewRequest(http.MethodPost, "/x", nil)
	req = req.WithContext(principal.WithAdmin(req.Context(), principal.AdminIdentity{IsAdmin: false, Teams: []string{"demo"}, AuthMethod: "oidc"}))
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("non-admin should be 403, got %d", rr.Code)
	}
	if len(recs) != 1 || recs[0].Event != "admin_denied" {
		t.Fatalf("authenticated 403 must emit one admin_denied record, got %+v", recs)
	}

	// no identity → 403 (fail closed), NOT audited (401-equivalent never grows the chain)
	recs = nil
	req = httptest.NewRequest(http.MethodPost, "/x", nil)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("no identity should be 403, got %d", rr.Code)
	}
	if len(recs) != 0 {
		t.Fatalf("unauthenticated 403 must not audit, got %d records", len(recs))
	}
}
