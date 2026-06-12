package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/inferplane/inferplane/internal/adminauth"
	"github.com/inferplane/inferplane/internal/principal"
)

func TestAdminTokenAuth(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	h := AdminTokenAuth([]string{"tok-a", "tok-b"}, next)
	cases := []struct {
		tok  string
		want int
	}{
		{"tok-a", 200}, {"tok-b", 200}, {"wrong", 401}, {"", 401},
	}
	for _, c := range cases {
		req := httptest.NewRequest("POST", "/admin/keys", nil)
		if c.tok != "" {
			req.Header.Set("Authorization", "Bearer "+c.tok)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Fatalf("tok %q: got %d want %d", c.tok, rec.Code, c.want)
		}
	}
}

func TestAdminTokenAuthRejectsEmptyConfig(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	h := AdminTokenAuth(nil, next)
	req := httptest.NewRequest("POST", "/admin/keys", nil)
	req.Header.Set("Authorization", "Bearer anything")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("empty token config must deny all: got %d", rec.Code)
	}
}

// --- unified AdminAuth (plan 2026-06-12 task 5) ---

// fakeVerifier instruments the OIDC path: counts calls, returns canned claims.
type fakeVerifier struct {
	calls  int
	claims adminauth.Claims
	err    error
}

func (f *fakeVerifier) Verify(_ context.Context, _ string) (adminauth.Claims, error) {
	f.calls++
	return f.claims, f.err
}

// adminAuthHarness wires AdminAuth with a capture handler; returns the
// recorder factory plus captured identity/denial state.
type adminAuthHarness struct {
	identity  *principal.AdminIdentity
	denials   []string
	handler   http.Handler
}

func newAdminAuthHarness(tokens []string, v OIDCVerifier, mapping adminauth.MappingConfig) *adminAuthHarness {
	h := &adminAuthHarness{}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id, ok := principal.AdminFrom(r.Context()); ok {
			h.identity = &id
		}
		w.WriteHeader(200)
	})
	h.handler = AdminAuth(tokens, v, mapping, func(_ *http.Request, subject string) {
		h.denials = append(h.denials, subject)
	}, next)
	return h
}

func (h *adminAuthHarness) do(bearer string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "/admin/keys", nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

const jwtShaped = "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ4In0.c2ln"

func TestAdminAuthStaticBackCompat(t *testing.T) {
	h := newAdminAuthHarness([]string{"opaque-token"}, nil, adminauth.MappingConfig{})
	if rec := h.do("opaque-token"); rec.Code != 200 {
		t.Fatalf("static token with nil verifier = %d, want 200", rec.Code)
	}
	if h.identity == nil || h.identity.Subject != "break-glass" || !h.identity.IsAdmin || h.identity.AuthMethod != "break_glass" {
		t.Fatalf("break-glass sentinel missing: %+v", h.identity)
	}
	if rec := h.do("wrong"); rec.Code != 401 {
		t.Fatalf("bad token = %d, want 401", rec.Code)
	}
	if rec := h.do(""); rec.Code != 401 {
		t.Fatalf("no token = %d, want 401", rec.Code)
	}
}

// TestAdminAuthNilVerifierRoutesEverythingStatic: with OIDC absent, even a
// JWT-shaped bearer takes the static path (back-compat).
func TestAdminAuthNilVerifierRoutesEverythingStatic(t *testing.T) {
	h := newAdminAuthHarness([]string{jwtShaped}, nil, adminauth.MappingConfig{})
	if rec := h.do(jwtShaped); rec.Code != 200 {
		t.Fatalf("JWT-shaped static token with nil verifier = %d, want 200 (static path)", rec.Code)
	}
}

// TestAdminAuthMutualExclusivity pins the security boundary (P2 gate): with a
// verifier configured, a JWT-shaped bearer is NEVER compared against static
// hashes — even if it matches one byte-for-byte — and a non-shaped bearer
// never reaches the verifier.
func TestAdminAuthMutualExclusivity(t *testing.T) {
	t.Run("shaped bearer never static-compared", func(t *testing.T) {
		v := &fakeVerifier{err: errStub}
		// Deliberately configure the SAME JWT-shaped string as a static token
		// (config.Load forbids this; the middleware must still not fall through).
		h := newAdminAuthHarness([]string{jwtShaped}, v, adminauth.MappingConfig{})
		if rec := h.do(jwtShaped); rec.Code != 401 {
			t.Fatalf("shaped bearer = %d, want 401 from OIDC path (static fallthrough = auth bypass)", rec.Code)
		}
		if v.calls != 1 {
			t.Fatalf("verifier calls = %d, want 1", v.calls)
		}
	})
	t.Run("garbage 3-segment routes to verifier", func(t *testing.T) {
		v := &fakeVerifier{err: errStub}
		h := newAdminAuthHarness([]string{"opaque-token"}, v, adminauth.MappingConfig{})
		if rec := h.do("aaaa.bbbb.cccc"); rec.Code != 401 {
			t.Fatalf("= %d, want 401", rec.Code)
		}
		if v.calls != 1 {
			t.Fatalf("verifier calls = %d, want 1 (non-JSON header must not demote to static)", v.calls)
		}
	})
	t.Run("non-shaped bearer never reaches verifier", func(t *testing.T) {
		v := &fakeVerifier{}
		h := newAdminAuthHarness([]string{"opaque-token"}, v, adminauth.MappingConfig{})
		if rec := h.do("opaque-token"); rec.Code != 200 {
			t.Fatalf("static = %d, want 200", rec.Code)
		}
		if rec := h.do("a.b"); rec.Code != 401 { // 2 segments → static path → no match
			t.Fatalf("2-segment = %d, want 401", rec.Code)
		}
		if v.calls != 0 {
			t.Fatalf("verifier calls = %d, want 0", v.calls)
		}
	})
}

func TestAdminAuthOIDCIdentityAndDenials(t *testing.T) {
	mapping := adminauth.MappingConfig{
		AdminGroups:   []string{"admins"},
		GroupMappings: []adminauth.GroupMapping{{Group: "team-alpha", Teams: []string{"alpha"}}},
	}
	t.Run("mapped member gets identity", func(t *testing.T) {
		v := &fakeVerifier{claims: adminauth.Claims{Subject: "u1", Groups: []string{"team-alpha"}}}
		h := newAdminAuthHarness([]string{"opaque"}, v, mapping)
		if rec := h.do(jwtShaped); rec.Code != 200 {
			t.Fatalf("= %d, want 200", rec.Code)
		}
		if h.identity == nil || h.identity.Subject != "u1" || h.identity.IsAdmin ||
			len(h.identity.Teams) != 1 || h.identity.Teams[0] != "alpha" || h.identity.AuthMethod != "oidc" {
			t.Fatalf("identity = %+v", h.identity)
		}
		if len(h.denials) != 0 {
			t.Fatalf("denials = %v, want none", h.denials)
		}
	})
	t.Run("unmapped groups: 403 + denial audited", func(t *testing.T) {
		v := &fakeVerifier{claims: adminauth.Claims{Subject: "u2", Groups: []string{"strangers"}}}
		h := newAdminAuthHarness([]string{"opaque"}, v, mapping)
		if rec := h.do(jwtShaped); rec.Code != 403 {
			t.Fatalf("= %d, want 403 (authenticated, unauthorized)", rec.Code)
		}
		if len(h.denials) != 1 || h.denials[0] != "u2" {
			t.Fatalf("denials = %v, want [u2]", h.denials)
		}
	})
	t.Run("verify error: 401, never audited", func(t *testing.T) {
		v := &fakeVerifier{err: errStub}
		h := newAdminAuthHarness([]string{"opaque"}, v, mapping)
		if rec := h.do(jwtShaped); rec.Code != 401 {
			t.Fatalf("= %d, want 401", rec.Code)
		}
		if len(h.denials) != 0 {
			t.Fatalf("401 must not be audited (flood guard); denials = %v", h.denials)
		}
	})
}

func TestAdminAuthOversizedBearer(t *testing.T) {
	v := &fakeVerifier{}
	h := newAdminAuthHarness([]string{"opaque"}, v, adminauth.MappingConfig{})
	big := strings.Repeat("a", 9*1024)
	if rec := h.do(big); rec.Code != 401 {
		t.Fatalf("oversized bearer = %d, want 401", rec.Code)
	}
	if v.calls != 0 || len(h.denials) != 0 {
		t.Fatalf("oversized bearer must be rejected before parsing/audit (calls=%d denials=%v)", v.calls, h.denials)
	}
}

var errStub = errors.New("stub verify failure")
