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
