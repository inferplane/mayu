package adminui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func get(t *testing.T, path string) (*http.Response, string) {
	t.Helper()
	srv := httptest.NewServer(Handler())
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestServesIndex(t *testing.T) {
	for _, path := range []string{"/", "/index.html"} {
		resp, body := get(t, path)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: status %d", path, resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("GET %s: Content-Type %q, want text/html", path, ct)
		}
		if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") {
			t.Errorf("GET %s: CSP %q, want default-src 'self'", path, csp)
		}
		if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("GET %s: missing nosniff", path)
		}
		if !strings.Contains(body, "inferplane") {
			t.Errorf("GET %s: index does not mention inferplane", path)
		}
	}
}

func TestServesAssets(t *testing.T) {
	for path, wantCT := range map[string]string{
		"/app.js":      "text/javascript",
		"/style.css":   "text/css",
		"/favicon.svg": "image/svg+xml",
	} {
		resp, _ := get(t, path)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: status %d", path, resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, wantCT) {
			t.Errorf("GET %s: Content-Type %q, want %s", path, ct, wantCT)
		}
		if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("GET %s: missing nosniff", path)
		}
	}
}

// TestAssetsAreDataFreeAndTokenSafe enforces the ADR-001 posture: the static
// assets carry no key material and the script never persists the admin token.
func TestAssetsAreDataFreeAndTokenSafe(t *testing.T) {
	for _, path := range []string{"/", "/app.js", "/style.css"} {
		_, body := get(t, path)
		for _, banned := range []string{"ik_", "localStorage", "sessionStorage", "document.cookie"} {
			if strings.Contains(body, banned) {
				t.Errorf("asset %s contains banned token %q", path, banned)
			}
		}
	}
}

func TestUnknownPath404(t *testing.T) {
	resp, _ := get(t, "/nope.txt")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /nope.txt: status %d, want 404", resp.StatusCode)
	}
}

// TestGovernanceTabCSPAndAuth pins the V3 invariants: the new Governance tab
// adds no inline style/handler attributes (CSP default-src 'self') and the
// verify button goes through the token-gated api() helper, never a bare
// unauthenticated fetch to /admin/audit/verify.
func TestGovernanceTabCSPAndAuth(t *testing.T) {
	_, html := get(t, "/")
	for _, banned := range []string{"onclick=", "style=", "onload="} {
		if strings.Contains(html, banned) {
			t.Errorf("index.html contains inline %q (CSP default-src 'self' bans it)", banned)
		}
	}
	_, js := get(t, "/app.js")
	// the verify handler must call api("GET", "/admin/audit/verify") — the
	// token-gated path — not a raw fetch to it.
	if !strings.Contains(js, `api("GET", "/admin/audit/verify")`) {
		t.Fatal("verify button must use the token-gated api() helper")
	}
	if strings.Contains(js, `fetch("/admin/audit/verify")`) {
		t.Fatal("verify button must NOT bare-fetch /admin/audit/verify (would be unauthenticated)")
	}
}

// TestProviderWriteUICSPAndContract pins the ADR-008 T8 console invariants:
// the provider/model write forms exist, carry NO inline handlers/styles (CSP
// default-src 'self'), go through the token-gated api() helper, and collect a
// secret REFERENCE (env/file) — never a secret value field.
func TestProviderWriteUICSPAndContract(t *testing.T) {
	_, html := get(t, "/")
	// the write affordances exist
	for _, id := range []string{`id="provider-form"`, `id="model-form"`, `id="export-btn"`, `id="pf-refkind"`, `id="mf-targets"`} {
		if !strings.Contains(html, id) {
			t.Errorf("index.html missing write-UI element %s", id)
		}
	}
	// no inline handlers/styles anywhere (CSP)
	for _, banned := range []string{"onclick=", "onsubmit=", "onchange=", "style=", "onload="} {
		if strings.Contains(html, banned) {
			t.Errorf("index.html contains inline %q (CSP default-src 'self' bans it)", banned)
		}
	}
	// the form collects a REF, not a secret value — guidance text + ref kind options
	if !strings.Contains(html, "never the secret value") {
		t.Error("provider form must tell the operator to enter a ref, not the secret value")
	}

	_, js := get(t, "/app.js")
	// writes go through the token-gated api() helper (carry the in-memory token)
	for _, call := range []string{`api("PUT", "/admin/providers/`, `api("PUT", "/admin/models/`, `api("DELETE", "/admin/providers/`, `api("GET", "/admin/config/export")`} {
		if !strings.Contains(js, call) {
			t.Errorf("app.js missing token-gated write call %q", call)
		}
	}
	// must NOT bare-fetch the write endpoints (would be unauthenticated) — guard
	// both string-literal and template-literal forms (T8 gate, kiro).
	for _, bad := range []string{
		`fetch("/admin/providers`, `fetch("/admin/models`,
		"fetch(`/admin/providers", "fetch(`/admin/models",
	} {
		if strings.Contains(js, bad) {
			t.Errorf("app.js bare-fetches a write endpoint %q (must use api())", bad)
		}
	}
}

// TestSelfServiceWhoamiUI pins ADR-010 T2: the console fetches identity via the
// token-gated api() (never a bare fetch), exposes the constrained team select,
// and renders identity via textContent (no innerHTML/concat into markup).
func TestSelfServiceWhoamiUI(t *testing.T) {
	_, html := get(t, "/")
	for _, id := range []string{`id="whoami-line"`, `id="team-select"`} {
		if !strings.Contains(html, id) {
			t.Errorf("index.html missing self-service element %s", id)
		}
	}
	_, js := get(t, "/app.js")
	if !strings.Contains(js, `api("GET", "/admin/whoami")`) {
		t.Fatal("whoami must be fetched via the token-gated api() helper")
	}
	for _, bad := range []string{`fetch("/admin/whoami`, "fetch(`/admin/whoami"} {
		if strings.Contains(js, bad) {
			t.Errorf("app.js bare-fetches whoami %q (must use api())", bad)
		}
	}
	// identity rendered via textContent, never innerHTML
	if !strings.Contains(js, `line.textContent = note`) || !strings.Contains(js, `"signed in as "`) {
		t.Fatal("identity must be rendered via textContent")
	}
	if strings.Contains(js, "innerHTML") {
		t.Fatal("app.js must not use innerHTML (CSP / XSS)")
	}
}
