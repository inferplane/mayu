package adminapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/inferplane/inferplane/internal/principal"
)

func whoamiReq(id *principal.AdminIdentity) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "/admin/whoami", nil)
	if id != nil {
		req = req.WithContext(principal.WithAdmin(req.Context(), *id))
	}
	rec := httptest.NewRecorder()
	WhoamiHandler().ServeHTTP(rec, req)
	return rec
}

func TestWhoamiOIDCIdentity(t *testing.T) {
	rec := whoamiReq(&principal.AdminIdentity{Subject: "sub-123", Teams: []string{"alpha", "beta"}, IsAdmin: false, AuthMethod: "oidc"})
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["subject"] != "sub-123" || got["is_admin"] != false || got["auth_method"] != "oidc" {
		t.Fatalf("whoami wrong: %v", got)
	}
	teams, _ := got["teams"].([]any)
	if len(teams) != 2 || teams[0] != "alpha" {
		t.Fatalf("teams wrong: %v", got["teams"])
	}
}

// TestWhoamiExactShape pins the PII-free invariant: the response has EXACTLY the
// four fields, so a future AdminIdentity field (email/claims/groups) cannot leak.
func TestWhoamiExactShape(t *testing.T) {
	rec := whoamiReq(&principal.AdminIdentity{Subject: "sub-x", Teams: []string{"t"}, AuthMethod: "oidc"})
	var got map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"subject": true, "teams": true, "is_admin": true, "auth_method": true}
	if len(got) != len(want) {
		t.Fatalf("whoami has %d fields, want exactly %d: %v", len(got), len(want), got)
	}
	for k := range got {
		if !want[k] {
			t.Fatalf("whoami leaked unexpected field %q", k)
		}
	}
	for _, banned := range []string{"email", "claims", "groups", "token"} {
		if _, bad := got[banned]; bad {
			t.Fatalf("whoami leaked %q", banned)
		}
	}
}

func TestWhoamiBreakGlassTeamsEmptyArray(t *testing.T) {
	rec := whoamiReq(&principal.AdminIdentity{Subject: "break-glass", IsAdmin: true, AuthMethod: "break_glass"})
	body := rec.Body.String()
	if got := struct{}{}; rec.Code != 200 {
		t.Fatalf("status %d %v", rec.Code, got)
	}
	// teams must serialize as [] (empty array), never null.
	if !containsJSON(body, `"teams":[]`) {
		t.Fatalf("break-glass teams must be [] not null: %s", body)
	}
	var got map[string]any
	_ = json.Unmarshal([]byte(body), &got)
	if got["is_admin"] != true || got["auth_method"] != "break_glass" {
		t.Fatalf("break-glass identity wrong: %v", got)
	}
}

func TestWhoamiNonGet405(t *testing.T) {
	req := httptest.NewRequest("POST", "/admin/whoami", nil)
	req = req.WithContext(principal.WithAdmin(req.Context(), principal.AdminIdentity{Subject: "x"}))
	rec := httptest.NewRecorder()
	WhoamiHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST whoami = %d, want 405", rec.Code)
	}
}

func containsJSON(haystack, needle string) bool {
	// tolerate whitespace-free encoder output
	return len(haystack) > 0 && (indexOf(haystack, needle) >= 0)
}
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
