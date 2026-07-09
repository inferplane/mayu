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

func TestAdminUI_eightSectionIA(t *testing.T) {
	_, html := get(t, "/index.html")
	_, js := get(t, "/app.js")
	views := []string{"overview", "usage", "logs", "keys", "teams", "providers", "governance", "settings"}
	for _, v := range views {
		if !strings.Contains(html, `data-view="`+v+`"`) {
			t.Errorf("index.html missing nav button data-view=%q", v)
		}
		if !strings.Contains(html, `id="view-`+v+`"`) {
			t.Errorf("index.html missing section id=view-%s (showView would null-deref)", v)
		}
		if !strings.Contains(js, v+":") { // VIEWS map key, e.g. `settings: "Settings"`
			t.Errorf("app.js VIEWS map missing key %q (HTML/JS IA out of sync)", v)
		}
	}
	if strings.Contains(html, `id="view-quickstart"`) {
		t.Error("index.html still has id=view-quickstart; rename it to view-settings")
	}
	if strings.Contains(js, "quickstart:") {
		t.Error("app.js VIEWS still has quickstart key; replace it with settings")
	}
}

func TestAdminUI_capabilitiesWired(t *testing.T) {
	_, js := get(t, "/app.js")
	for _, want := range []string{"/admin/capabilities", "loadCapabilities", "applyCapabilities", "DISABLED"} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js missing %q — capability wiring incomplete (spec §4.4/§9.1)", want)
		}
	}
	if !strings.Contains(js, "await loadCapabilities()") {
		t.Error("app.js does not await loadCapabilities() (must run on unlock, before showView)")
	}
}

func TestAdminUI_usageFetchesAnalytics(t *testing.T) {
	_, js := get(t, "/app.js")
	if !strings.Contains(js, "/admin/analytics/summary") {
		t.Error("app.js Usage view does not fetch /admin/analytics/summary")
	}
	if !strings.Contains(js, "refreshUsageView") {
		t.Error("app.js missing refreshUsageView()")
	}
	_, html := get(t, "/index.html")
	if !strings.Contains(html, `id="usage-content"`) {
		t.Error("index.html missing #usage-content block")
	}
}

func TestAdminUI_overviewSparkline(t *testing.T) {
	_, js := get(t, "/app.js")
	for _, want := range []string{"renderSparkline", "stat-spend-spark", "/admin/analytics/timeseries"} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js missing %q — Overview spend sparkline not wired", want)
		}
	}
	if strings.Contains(js, "innerHTML") {
		t.Error("app.js uses innerHTML — data-free/no-markup-from-data invariant violated")
	}
}

func TestAdminUI_keyGovernanceFieldsWired(t *testing.T) {
	_, html := get(t, "/index.html")
	for _, id := range []string{"kf-budget", "kf-tpm", "kf-rpm", "kf-expires", "kf-owner"} {
		if !strings.Contains(html, `id="`+id+`"`) {
			t.Errorf("index.html missing key-options input #%s", id)
		}
	}
	_, js := get(t, "/app.js")
	if !strings.Contains(js, "keyLimitsSummary") {
		t.Error("app.js missing keyLimitsSummary() — key governance fields not rendered")
	}
	if !strings.Contains(js, "budget_usd_micros") {
		t.Error("app.js does not send budget_usd_micros on key create")
	}
}

// TestAdminUI_teamsRecordsWired pins D3/ADR-016's console surface: the write
// form + tables exist, go through the token-gated api() helper (never a bare
// fetch — the same CSP/auth contract as the provider-write and whoami UI),
// and the HA/derived-users honesty hints are present verbatim.
func TestAdminUI_teamsRecordsWired(t *testing.T) {
	_, html := get(t, "/index.html")
	for _, id := range []string{
		`id="team-form"`, `id="tf-name"`, `id="tf-budget"`, `id="tf-rpm"`, `id="tf-tpm"`,
		`id="tf-tpd"`, `id="tf-quota-exceeded"`, `id="tf-budget-exceeded"`, `id="tf-models"`,
		`id="teams-table"`, `id="users-table"`, `id="teams-content"`,
	} {
		if !strings.Contains(html, id) {
			t.Errorf("index.html missing teams-view element %s", id)
		}
	}
	// no inline handlers/styles (CSP default-src 'self')
	for _, banned := range []string{"onclick=", "onsubmit=", "onchange=", "style=", "onload="} {
		if strings.Contains(html, banned) {
			t.Errorf("index.html contains inline %q in the teams section (CSP)", banned)
		}
	}
	// HA honesty (ADR-013) and the derived-users limitation are stated verbatim.
	for _, want := range []string{"per gateway instance", "there is no", "not available"} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing honesty hint %q", want)
		}
	}

	_, js := get(t, "/app.js")
	for _, call := range []string{
		`api("GET", "/admin/teams")`, `api("PUT", "/admin/teams/`,
		`api("DELETE", "/admin/teams/`, `api("GET", "/admin/users")`,
	} {
		if !strings.Contains(js, call) {
			t.Errorf("app.js missing token-gated call %q", call)
		}
	}
	for _, bad := range []string{
		`fetch("/admin/teams`, "fetch(`/admin/teams", `fetch("/admin/users`, "fetch(`/admin/users",
	} {
		if strings.Contains(js, bad) {
			t.Errorf("app.js bare-fetches a teams/users endpoint %q (must use api())", bad)
		}
	}
	if !strings.Contains(js, "refreshTeamsView") {
		t.Error("app.js missing refreshTeamsView()")
	}
	if !strings.Contains(js, `if (name === "teams") refreshTeamsView();`) {
		t.Error("showView does not call refreshTeamsView() for the teams section")
	}
	// write affordances are gated on the client-side whoamiIsAdmin hint (the
	// server enforces via requireAdmin regardless — this is UX, not the gate).
	if !strings.Contains(js, "whoamiIsAdmin") {
		t.Error("app.js does not gate the team write form on admin identity")
	}
}

func TestAdminUI_bodyLoggingWired(t *testing.T) {
	_, html := get(t, "/index.html")
	for _, id := range []string{
		`id="logs-content"`, `id="logs-table"`, `id="logs-load-more"`,
		`id="body-drawer"`, `id="body-drawer-content"`, `id="body-drawer-close"`,
		`data-cap="logs_bodies"`,
	} {
		if !strings.Contains(html, id) {
			t.Errorf("index.html missing logs/body element %s", id)
		}
	}
	for _, banned := range []string{"onclick=", "onsubmit=", "onchange=", "style=", "onload="} {
		if strings.Contains(html, banned) {
			t.Errorf("index.html contains inline %q in the logs section (CSP)", banned)
		}
	}
	// Retention/privacy honesty (§6.3): outside-the-chain, best-effort masking,
	// and the streaming-response capture limitation are stated verbatim.
	for _, want := range []string{"OUTSIDE the audit chain", "best-effort", "streaming", "body_accessed"} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing honesty hint %q", want)
		}
	}

	_, js := get(t, "/app.js")
	for _, call := range []string{
		`api("GET", "/admin/logs"`, `api("GET", "/admin/bodies/"`, `api("DELETE", "/admin/bodies/"`,
	} {
		if !strings.Contains(js, call) {
			t.Errorf("app.js missing token-gated call %q", call)
		}
	}
	for _, bad := range []string{
		`fetch("/admin/logs`, "fetch(`/admin/logs", `fetch("/admin/bodies`, "fetch(`/admin/bodies",
	} {
		if strings.Contains(js, bad) {
			t.Errorf("app.js bare-fetches a logs/bodies endpoint %q (must use api())", bad)
		}
	}
	if !strings.Contains(js, "refreshLogsView") {
		t.Error("app.js missing refreshLogsView()")
	}
	if !strings.Contains(js, `if (name === "logs") refreshLogsView();`) {
		t.Error("showView does not call refreshLogsView() for the logs section")
	}
}

func TestAdminUI_budgetAlertsWired(t *testing.T) {
	_, html := get(t, "/index.html")
	for _, id := range []string{`data-cap="budget_alerts"`, `id="alerts-table"`} {
		if !strings.Contains(html, id) {
			t.Errorf("index.html missing budget-alerts element %s", id)
		}
	}
	for _, banned := range []string{"onclick=", "onsubmit=", "onchange=", "style=", "onload="} {
		if strings.Contains(html, banned) {
			t.Errorf("index.html contains inline %q in the budget-alerts card (CSP)", banned)
		}
	}
	// per-instance honesty (same posture as ADR-013's limiter/budget caveat).
	if !strings.Contains(html, "Per-instance state") {
		t.Error("index.html missing per-instance-state honesty hint for budget alerts")
	}

	_, js := get(t, "/app.js")
	if !strings.Contains(js, `api("GET", "/admin/alerts/recent", null, true)`) {
		t.Error("app.js missing token-gated call to /admin/alerts/recent")
	}
	if strings.Contains(js, `fetch("/admin/alerts`) || strings.Contains(js, "fetch(`/admin/alerts") {
		t.Error("app.js bare-fetches /admin/alerts (must use api())")
	}
}

// TestAdminUI_guardrailFieldsWired (D6, ADR-019): the team form carries the
// guardrail override inputs, submits them through the existing token-gated
// PUT /admin/teams/{name} call (no new endpoint), and states the no-opt-out
// anti-bypass invariant.
func TestAdminUI_guardrailFieldsWired(t *testing.T) {
	_, html := get(t, "/index.html")
	for _, id := range []string{`id="tf-guardrail-id"`, `id="tf-guardrail-version"`} {
		if !strings.Contains(html, id) {
			t.Errorf("index.html missing guardrail element %s", id)
		}
	}
	for _, banned := range []string{"onclick=", "onsubmit=", "onchange=", "style=", "onload="} {
		if strings.Contains(html, banned) {
			t.Errorf("index.html contains inline %q in the team-form guardrail fields (CSP)", banned)
		}
	}
	if !strings.Contains(html, "no per-team opt-out") {
		t.Error("index.html missing the no-opt-out anti-bypass honesty hint for guardrails")
	}

	_, js := get(t, "/app.js")
	if !strings.Contains(js, `body.guardrail_id = $("tf-guardrail-id").value.trim();`) {
		t.Error("app.js does not submit guardrail_id in the team-form handler")
	}
	if !strings.Contains(js, `$("tf-guardrail-id").value = t.guardrail_id`) {
		t.Error("app.js does not prefill guardrail_id when editing a team")
	}
}
