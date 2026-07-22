// Package adminui serves the minimal embedded admin key console (ADR-001).
// Three dependency-free static files ship inside the binary via go:embed; the
// assets are data-free (no key material, no team data) and therefore safe to
// serve unauthenticated on the admin plane — every data operation the page
// performs goes through the existing token-gated /admin/keys JSON API.
package adminui

import (
	"embed"
	"net/http"
	"strings"
)

//go:embed static
var static embed.FS

// contentTypes pins the served types explicitly (with nosniff, the browser
// must not have to guess).
var contentTypes = map[string]string{
	"index.html":  "text/html; charset=utf-8",
	"app.js":      "text/javascript; charset=utf-8",
	"i18n.js":     "text/javascript; charset=utf-8",
	"style.css":   "text/css; charset=utf-8",
	"favicon.svg": "image/svg+xml",
}

// Handler serves the console at the mount root: "/" (and "/index.html"),
// "/app.js", "/i18n.js", "/style.css". Anything else is 404. Every response
// carries a strict CSP and nosniff (ADR-001 security posture).
func Handler(extraConnectSrc ...string) http.Handler {
	csp := "default-src 'self'; frame-ancestors 'none'"
	if len(extraConnectSrc) > 0 {
		csp = "default-src 'self'; connect-src 'self' " + strings.Join(extraConnectSrc, " ") + "; frame-ancestors 'none'"
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name := r.URL.Path
		if name == "/" || name == "" {
			name = "index.html"
		} else {
			name = name[1:] // drop leading slash
		}
		ct, ok := contentTypes[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		body, err := static.ReadFile("static/" + name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		h := w.Header()
		h.Set("Content-Type", ct)
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		w.Write(body)
	})
}
