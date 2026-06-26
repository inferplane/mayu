# Console Phase 0a — Capability Negotiation + 8-Section IA Scaffold Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a token-gated `GET /admin/capabilities` endpoint and wire the console to fetch it on unlock, so the 8-section IA renders each section's enabled/disabled state from a single bootstrap call — never by probing endpoints and catching 404/5xx.

**Architecture:** A new `configapi.Capabilities` projection (mirroring the existing secret-free `configapi.View`/`Writable` capability-hint pattern) is served behind `AdminAuth` at `/admin/capabilities`. The assembly (`cmd/inferplane`) computes it from what it already knows (provider store present? PII filter active?). The console fetches it once after unlock and gates each nav section: implemented sections work; not-yet-backed sections (Usage/Logs/Teams) render a disabled-with-reason affordance. No SPA framework, no build step — vanilla JS + `go:embed` (ADR-002). No client-side persistence (ADR-001).

**Tech Stack:** Go 1.25 (`net/http`, `encoding/json`), `modernc.org/sqlite` (unaffected here), vanilla HTML/CSS/JS embedded via `go:embed`. Tests: Go (`go test`), including the existing `internal/server/adminui/adminui_test.go` asset-invariant scans. There is **no JS test runner** (toolchain-free) — frontend correctness is asserted via Go asset scans + `gofmt`/`go vet`/build.

## Global Constraints

Copied verbatim from the design spec (`docs/superpowers/specs/2026-06-26-admin-console-litellm-ux-redesign-design.md`) — every task implicitly includes these:

- **C1 Data-free browser** (ADR-001): admin token + all data in page memory only; **no `localStorage`, no `sessionStorage`** in any served asset; enforced by `adminui_test`.
- **C2 Toolchain-free** (ADR-002): vanilla HTML/CSS/JS via `go:embed`; **no framework, no node/Vite build** in the critical path.
- **C3 Secret-ref mandate** (§7): responses carry refs/modes only — **never a secret value**. Capabilities carries booleans/enums only.
- **C4 `/metrics` cardinality** unchanged: no new metric labels in this plan.
- **C5 `count_tokens` never non-200**: data path untouched.
- **Capabilities is token-gated** (§4.4): mounted behind `AdminAuth` like `/admin/config`.
- **Capability map (§4.4)**, Phase-0a subset — exact JSON keys: `analytics_index` (`"A"|"B"|"off"`), `logs_bodies` (bool), `teams_records` (bool), `key_governance_fields` (bool), `provider_store` (bool), `region_policy` (bool), `guardrails` (bool). Phase 0a: everything `off`/`false` except `provider_store` (true iff a provider store is configured) and `guardrails` (true iff a PII-mask filter is active).
- **Degradation, not errors** (§9.1): a section whose capability is off renders a calm affordance ("Enable the analytics store to see usage history"), never an error or blank paint. `api.js` maps `404`/`405`/`501` on an optional endpoint to *disabled*.
- **8 sections (§5)**: Overview, Usage, Logs, Virtual keys, Teams & Users, Providers & Models, Governance, Settings.
- Commit style: DCO sign-off (`git commit -s`); end body with `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`.

---

## File Structure

- `internal/server/configapi/capabilities.go` — **Create**. `Capabilities` struct + `CapabilitiesHandler(get func() Capabilities) http.Handler` (GET-only, JSON, secret-free). Lives in `configapi` because it is the same secret-free capability-hint concern as `View.Writable`.
- `internal/server/configapi/capabilities_test.go` — **Create**. Unit tests for the handler (shape, GET-only, no secret).
- `internal/server/server.go` — **Modify** `AdminMux(...)` (signature + one `mux.Handle`) to mount `/admin/capabilities` behind `AdminAuth`.
- `cmd/inferplane/*` (the serve assembly that calls `AdminMux`) — **Modify** to pass a `func() configapi.Capabilities`.
- `internal/server/adminui/static/index.html` — **Modify** nav (3 new buttons) + add 3 section shells (Usage, Logs, Teams & Users) with affordance placeholders; rename existing sections to the spec IA labels.
- `internal/server/adminui/static/app.js` — **Modify**: fetch capabilities on unlock, store in a page-memory variable, render section affordances; extend the shared `api()` to map 404/405/501→disabled.
- `internal/server/adminui/adminui_test.go` — **Modify**: assert the 8 nav sections exist, the capabilities fetch is wired, and the data-free invariant holds across assets.

---

### Task 1: `/admin/capabilities` endpoint (handler + projection)

**Files:**
- Create: `internal/server/configapi/capabilities.go`
- Test: `internal/server/configapi/capabilities_test.go`

**Interfaces:**
- Consumes: nothing (leaf).
- Produces:
  - `type Capabilities struct { AnalyticsIndex string; LogsBodies, TeamsRecords, KeyGovernanceFields, ProviderStore, RegionPolicy, Guardrails bool }` with JSON tags `analytics_index,logs_bodies,teams_records,key_governance_fields,provider_store,region_policy,guardrails`.
  - `func CapabilitiesHandler(get func() Capabilities) http.Handler` — GET-only; 405 on other methods; `Content-Type: application/json`; encodes `get()`.

- [ ] **Step 1: Write the failing test**

```go
// internal/server/configapi/capabilities_test.go
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
	for _, key := range []string{"analytics_index", "logs_bodies", "teams_records", "key_governance_fields", "provider_store", "region_policy", "guardrails"} {
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/server/configapi/ -run TestCapabilitiesHandler -v`
Expected: FAIL — `undefined: CapabilitiesHandler` / `undefined: Capabilities`.

- [ ] **Step 3: Write minimal implementation**

```go
// internal/server/configapi/capabilities.go

// Capabilities is the secret-free feature/runtime map the console fetches on
// bootstrap (design spec §4.4) to render each section's enabled/disabled state
// without probing endpoints and catching 404/5xx. It carries booleans/enums
// only — never a secret value (§7). Same capability-hint concern as
// View.Writable, hence this package.
package configapi

import (
	"encoding/json"
	"net/http"
)

type Capabilities struct {
	// AnalyticsIndex is "A" (local single-replica), "B" (shared HA store), or
	// "off". Phase 0a always reports "off" (no analytics index yet).
	AnalyticsIndex      string `json:"analytics_index"`
	LogsBodies          bool   `json:"logs_bodies"`
	TeamsRecords        bool   `json:"teams_records"`
	KeyGovernanceFields bool   `json:"key_governance_fields"`
	ProviderStore       bool   `json:"provider_store"`
	RegionPolicy        bool   `json:"region_policy"`
	Guardrails          bool   `json:"guardrails"`
}

// CapabilitiesHandler serves the capability map (GET only, JSON). It is mounted
// behind AdminAuth (token-gated, §4.4). get is evaluated per request so a
// hot-reload that changes the topology is reflected without a restart.
func CapabilitiesHandler(get func() Capabilities) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(get())
	})
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/server/configapi/ -run TestCapabilitiesHandler -v`
Expected: PASS (both tests).

- [ ] **Step 5: Format/vet and commit**

```bash
gofmt -w internal/server/configapi/capabilities.go internal/server/configapi/capabilities_test.go
go vet ./internal/server/configapi/
git add internal/server/configapi/capabilities.go internal/server/configapi/capabilities_test.go
git commit -s -m "feat(configapi): secret-free capabilities projection + handler (spec §4.4)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 2: Mount `/admin/capabilities` in `AdminMux` and wire the assembly

**Files:**
- Modify: `internal/server/server.go` (function `AdminMux`, ~line 72)
- Modify: the serve assembly under `cmd/inferplane/` that calls `AdminMux` (grep for `AdminMux(` to find the exact caller/line)
- Test: `internal/server/server_test.go` (add a test; create the file only if no admin-mux test file exists — otherwise append)

**Interfaces:**
- Consumes: `configapi.CapabilitiesHandler`, `configapi.Capabilities` (Task 1).
- Produces: `AdminMux(...)` gains a trailing parameter `capabilities func() configapi.Capabilities` placed **immediately before** the variadic `probeAllowedHosts ...string` (a variadic must stay last). When `capabilities` is nil, the route is omitted (same nil-guard discipline as `configExport`/`m`).

- [ ] **Step 1: Write the failing test**

```go
// internal/server/server_test.go  (append; package server)
func TestAdminMux_capabilitiesEndpoint(t *testing.T) {
	caps := func() configapi.Capabilities {
		return configapi.Capabilities{AnalyticsIndex: "off", ProviderStore: true}
	}
	// adminTokens with a known token so AdminAuth lets us through.
	h := AdminMux(nil, []string{"tok"}, nil, adminauth.MappingConfig{}, nil, nil, nil, nil, nil, nil, caps)
	req := httptest.NewRequest(http.MethodGet, "/admin/capabilities", nil)
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"provider_store":true`) {
		t.Fatalf("body missing provider_store:true — %s", rec.Body.String())
	}
}
```

> Note: match the exact `AdminMux` argument list at the call site. The arguments above follow the current order `(store, adminTokens, verifier, mapping, configView, auditFileSinks, aud, m, writer, configExport, <caps>, probeAllowedHosts...)`. If the real signature differs, copy the real one and add `caps` as the last non-variadic parameter. Ensure imports for `configapi`, `adminauth`, `httptest`, `strings`, `net/http` are present.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/server/ -run TestAdminMux_capabilitiesEndpoint -v`
Expected: FAIL — too many arguments to `AdminMux` / route 404.

- [ ] **Step 3: Add the parameter and mount the route**

In `internal/server/server.go`, change the `AdminMux` signature to add `capabilities func() configapi.Capabilities` immediately before `probeAllowedHosts ...string`:

```go
func AdminMux(store keystore.Store, adminTokens []string, verifier OIDCVerifier, mapping adminauth.MappingConfig, configView func() configapi.View, auditFileSinks []string, aud *audit.Writer, m *metrics.Metrics, writer configapi.Writer, configExport func() configapi.ExportDoc, capabilities func() configapi.Capabilities, probeAllowedHosts ...string) http.Handler {
```

Mount the route next to `/admin/config` (after the `mux.Handle("/admin/config", ...)` line):

```go
	// Capability map (spec §4.4), behind the same AdminAuth — secret-free
	// booleans/enums the console reads on bootstrap to render each section's
	// enabled/disabled affordance (degradation contract §9.1). nil → omitted.
	if capabilities != nil {
		mux.Handle("/admin/capabilities", AdminAuth(adminTokens, verifier, mapping, denied, configapi.CapabilitiesHandler(capabilities)))
	}
```

- [ ] **Step 4: Update the assembly call site**

Find it: `grep -rn "AdminMux(" cmd/ internal/ | grep -v _test`. At the serve call site, build and pass the capabilities provider. Phase 0a derives it from what the assembly already has — `writer != nil` for the provider store, and whether a PII-mask filter is configured:

```go
	caps := func() configapi.Capabilities {
		return configapi.Capabilities{
			AnalyticsIndex: "off", // no analytics index yet (Phase 1)
			ProviderStore:  providerWriter != nil,
			Guardrails:     piiFilterActive, // true iff a PII-mask filter is configured
			// LogsBodies, TeamsRecords, KeyGovernanceFields, RegionPolicy: false until their phases land
		}
	}
	// ...pass `caps` as the new arg, before the probeAllowedHosts variadic:
	adminMux := server.AdminMux(store, adminTokens, verifier, mapping, configView, auditFileSinks, aud, m, providerWriter, configExport, caps, probeAllowedHosts...)
```

> Substitute the assembly's real local names (`providerWriter`, `piiFilterActive`, etc.) — grep the call site to read them. If a "PII filter active" boolean is not readily available at the call site, set `Guardrails: false` for Phase 0a and leave a `// TODO(phase4): wire guardrail status` — *this is the one acceptable deferral and must be a real, named follow-up, not a vague placeholder.*

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/server/ -run TestAdminMux_capabilitiesEndpoint -v && go build ./...`
Expected: PASS and a clean build.

- [ ] **Step 6: Format/vet and commit**

```bash
gofmt -w internal/server/server.go cmd/inferplane/
go vet ./...
git add internal/server/server.go internal/server/server_test.go cmd/inferplane/
git commit -s -m "feat(server): mount /admin/capabilities behind AdminAuth (spec §4.4)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 3: 8-section nav + section shells in `index.html`

**Files:**
- Modify: `internal/server/adminui/static/index.html` (nav block ~lines 35-41; section blocks)

**Interfaces:**
- Consumes: nothing at build time; the new `data-view` names are consumed by `app.js` (Task 4).
- Produces: nav buttons with `data-view` values `overview`, `usage`, `logs`, `keys`, `teams`, `providers`, `governance`, `settings`; matching `<section id="view-<name>">` blocks. Each not-yet-backed section contains an element `<div class="affordance" data-cap="<capkey>">` the JS toggles.

- [ ] **Step 1: Replace the nav block** (currently 5 buttons) with the 8-section nav, preserving the existing icon/`&ensp;` style:

```html
      <nav>
        <button class="nav active" data-view="overview">⌂&ensp;Overview</button>
        <button class="nav" data-view="usage">▤&ensp;Usage</button>
        <button class="nav" data-view="logs">≡&ensp;Logs</button>
        <button class="nav" data-view="keys">⚿&ensp;Virtual keys</button>
        <button class="nav" data-view="teams">⚇&ensp;Teams &amp; Users</button>
        <button class="nav" data-view="providers">⇄&ensp;Providers &amp; Models</button>
        <button class="nav" data-view="governance">◷&ensp;Governance</button>
        <button class="nav" data-view="settings">⚙&ensp;Settings</button>
      </nav>
```

- [ ] **Step 2: Add the three new section shells** with affordance placeholders. Insert after the existing `#view-overview` section and before `#view-keys` (Usage, Logs), and after `#view-keys` (Teams). Each is hidden by default and carries a `data-cap` the JS reads:

```html
      <!-- ===== usage ===== -->
      <section id="view-usage" class="view" hidden>
        <div class="card affordance" data-cap="analytics_index">
          <div class="microlabel">usage &amp; spend analytics</div>
          <p class="hint">Enable the analytics store to see spend over time and per
            team/key/model breakdowns. <span class="ko">분석 스토어를 켜면 기간별
            스펜드와 팀·키·모델별 사용량을 볼 수 있습니다.</span></p>
        </div>
      </section>

      <!-- ===== logs ===== -->
      <section id="view-logs" class="view" hidden>
        <div class="card affordance" data-cap="analytics_index">
          <div class="microlabel">request log viewer</div>
          <p class="hint">Enable the analytics store to inspect individual requests
            (metadata). Prompt/response bodies require the opt-in body store.
            <span class="ko">분석 스토어를 켜면 개별 요청(메타데이터)을 볼 수 있습니다.</span></p>
        </div>
      </section>
```

```html
      <!-- ===== teams & users ===== -->
      <section id="view-teams" class="view" hidden>
        <div class="card affordance" data-cap="teams_records">
          <div class="microlabel">teams &amp; users</div>
          <p class="hint">Team and user records are not enabled. Today teams are
            derived from issued keys. <span class="ko">팀·유저 레코드가 비활성화돼
            있습니다. 현재 팀은 발급된 키에서 파생됩니다.</span></p>
        </div>
      </section>
```

- [ ] **Step 3: Rename Quickstart → Settings and relabel.** Change the section opening tag `<section id="view-quickstart" ...>` to `<section id="view-settings" ...>` (so it matches the `settings` key in `VIEWS`). **Keep every inner element id unchanged** (`usage-claude`, `usage-curl`, `usage-openai`, `usage-models`, etc.) so the existing `renderUsage()` snippet-filler keeps targeting them. No structural change to Overview/Keys/Governance beyond the nav label; `#view-providers` keeps its id (the nav label "Providers & Models" comes from the `VIEWS` map in Task 4, Step 3).

> Decision recorded: Quickstart becomes the initial Settings view (connection snippets are legitimately settings-adjacent). The follow-up plan (Phase 0b) adds routing/caching/compliance toggles under it. This is a rename, not a removal — no content lost, and `VIEWS`↔section ids stay 1:1 (so `showView` never dereferences a null section).

- [ ] **Step 4: Verify the asset parses** (no JS test runner — this is a structural check):

Run: `grep -c 'data-view=' internal/server/adminui/static/index.html`
Expected: `8`.

- [ ] **Step 5: Commit**

```bash
git add internal/server/adminui/static/index.html
git commit -s -m "feat(adminui): 8-section nav + Usage/Logs/Teams shells with affordances (spec §5)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 4: Fetch capabilities on unlock + capability-driven affordances + api.js status mapping

**Files:**
- Modify: `internal/server/adminui/static/app.js`

**Interfaces:**
- Consumes: `GET /admin/capabilities` (Tasks 1-2); the `data-cap` attributes (Task 3); the existing `api(method, path, body)` (`app.js:11`) and `showView(name)` (`app.js:35`).
- Produces: a page-memory `let caps = null;`, an `async function loadCapabilities()`, an `function applyCapabilities()` that disables nav buttons / shows affordances per capability, and a `DISABLED` sentinel returned by `api()` for 404/405/501 on optional endpoints.

- [ ] **Step 1: Extend the EXISTING `api()` to map disabled-optional statuses.** The real `api()` (`app.js:11-29`) inlines its headers and returns `resp.json()` (or `null` on 204). Do **not** rewrite it — insert two lines only: a `DISABLED` sentinel above it, and a 404/405/501 short-circuit **after the 401 check, before the non-OK throw**. The result must read exactly:

```js
const DISABLED = Symbol("capability-disabled");

async function api(method, path, body) {
  const resp = await fetch(path, {
    method,
    headers: {
      "Authorization": "Bearer " + adminToken,
      ...(body ? { "Content-Type": "application/json" } : {}),
    },
    body: body ? JSON.stringify(body) : undefined,
  });
  if (resp.status === 401) throw new Error("unauthorized — check the admin token");
  // Optional-capability endpoints absent → treat as disabled, not an error (§9.1).
  if (resp.status === 404 || resp.status === 405 || resp.status === 501) return DISABLED;
  if (!resp.ok && resp.status !== 204) {
    let msg = "API error " + resp.status;
    try { const j = await resp.json(); if (j && j.error) msg = j.error; } catch { /* keep generic */ }
    throw new Error(msg);
  }
  return resp.status === 204 ? null : resp.json();
}
```

> Caution: `DISABLED` now leaks into every `api()` caller's success path. Audit callers that pattern-match the result — none should treat the `DISABLED` symbol as data. In Phase 0a only `loadCapabilities()` (Step 2) consumes it; existing callers hit real endpoints that never return 404/405/501 here, so they are unaffected. Do not introduce `localStorage`/`sessionStorage` (C1).

- [ ] **Step 2: Add capability load + apply.** Add near the other `refresh*`/`load*` functions:

```js
let caps = null;

async function loadCapabilities() {
  const out = await api("GET", "/admin/capabilities");
  caps = (out && out !== DISABLED) ? out : {};   // absent endpoint → all-off (safe default)
  applyCapabilities();
}

function applyCapabilities() {
  // Each affordance card declares the capability it needs via data-cap.
  document.querySelectorAll(".affordance[data-cap]").forEach((el) => {
    const key = el.dataset.cap;
    const on = capOn(key);
    el.hidden = on;     // capability present → hide the "enable X" affordance
  });
}

// capOn maps a capability key to a boolean. analytics_index is an enum
// ("A"|"B"|"off"); everything else is a bool.
function capOn(key) {
  if (!caps) return false;
  if (key === "analytics_index") return caps.analytics_index && caps.analytics_index !== "off";
  return !!caps[key];
}
```

- [ ] **Step 3: Update the `VIEWS` map (REQUIRED — `showView` is driven by it).** `showView` (`app.js:35`) iterates `Object.keys(VIEWS)` and does `$("view-"+v).hidden = v !== name`. A nav view that is **not** in `VIEWS`, or whose `#view-<key>` section does not exist, breaks rendering (`$()` returns `null` → throws). Replace the `VIEWS` const (`app.js:33`) with all 8, matching the Task 3 section ids exactly:

```js
const VIEWS = {
  overview: "Overview",
  usage: "Usage",
  logs: "Logs",
  keys: "Virtual keys",
  teams: "Teams & Users",
  providers: "Providers & Models",
  governance: "Governance",
  settings: "Settings",
};
```

> `quickstart` is removed from `VIEWS` because Task 3 renames `#view-quickstart` → `#view-settings`. The inner snippet element ids are unchanged, so the existing `renderUsage()` (which fills those snippets — unrelated to the new Usage *analytics* view) keeps working. Do not confuse `renderUsage()` (Quickstart/Settings snippet filler) with the `usage` view.

- [ ] **Step 4: Call `loadCapabilities()` on unlock.** In the `#token-form` submit success path (`app.js:736-757`), insert `await loadCapabilities();` immediately after `await loadWhoami();` and before `showView("overview");`:

```js
    await loadWhoami(); // self-service identity + team scoping (ADR-010)
    await loadCapabilities(); // capability-driven section affordances (spec §9.1)
    showView("overview");
```

> `refreshKeys()` earlier in the handler is the auth gate (throws → stays locked), so by the time `loadCapabilities()` runs the token is known-good.

- [ ] **Step 5: Verify no forbidden storage + structure** (Go-side asset scan is Task 5; quick local check here):

Run: `grep -nE "localStorage|sessionStorage" internal/server/adminui/static/app.js || echo "clean"`
Expected: `clean`.
Run: `grep -c "loadCapabilities" internal/server/adminui/static/app.js`
Expected: `>=2` (definition + call).
Run: `grep -c 'usage:\|logs:\|teams:\|settings:' internal/server/adminui/static/app.js`
Expected: `>=1` (the VIEWS map updated).

- [ ] **Step 6: Commit**

```bash
git add internal/server/adminui/static/app.js
git commit -s -m "feat(adminui): bootstrap /admin/capabilities, capability-driven affordances (spec §9.1)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 5: Extend `adminui_test.go` — data-free invariant + IA + capabilities wiring

**Files:**
- Modify: `internal/server/adminui/adminui_test.go`

**Interfaces:**
- Consumes: the existing `get(t, path) (*http.Response, string)` helper (`adminui_test.go:11`) that spins an `httptest.NewServer(Handler())` and GETs the asset, returning the body string.
- Produces: tests asserting (a) the 8 nav sections exist, (b) the capabilities fetch is wired in `app.js`, (c) every `VIEWS` key has a matching section (no null-deref in `showView`). The existing `TestAssetsAreDataFreeAndTokenSafe` already enforces no `localStorage`/`sessionStorage` — do **not** duplicate it; instead extend its asset list if needed.

- [ ] **Step 1: Write the failing tests** (append to `adminui_test.go`; reuse the existing `get` helper, not a new one):

```go
func TestAdminUI_eightNavSections(t *testing.T) {
	_, html := get(t, "/index.html")
	for _, view := range []string{"overview", "usage", "logs", "keys", "teams", "providers", "governance", "settings"} {
		if !strings.Contains(html, `data-view="`+view+`"`) {
			t.Errorf("index.html missing nav button data-view=%q", view)
		}
		// Every nav view must have a matching section, or showView() null-derefs.
		if !strings.Contains(html, `id="view-`+view+`"`) {
			t.Errorf("index.html missing section id=view-%s for nav view %q", view, view)
		}
	}
	// quickstart was renamed to settings — its old id must be gone.
	if strings.Contains(html, `id="view-quickstart"`) {
		t.Error("index.html still has id=view-quickstart; it must be renamed to view-settings")
	}
}

func TestAdminUI_capabilitiesWired(t *testing.T) {
	_, js := get(t, "/app.js")
	if !strings.Contains(js, "/admin/capabilities") {
		t.Error("app.js does not fetch /admin/capabilities — capability negotiation missing (spec §4.4)")
	}
	if !strings.Contains(js, "loadCapabilities") {
		t.Error("app.js missing loadCapabilities()")
	}
}
```

> Confirm `strings` is already imported in `adminui_test.go` (it is used by the existing CSP/data-free tests). If not, add it.

- [ ] **Step 2: Run to verify the new tests pass** (they assert the state produced by Tasks 3-4, so they should pass once those are done; if any fail, fix the asset, not the test):

Run: `go test ./internal/server/adminui/ -run 'TestAdminUI_(eightNavSections|capabilitiesWired)' -v`
Expected: PASS (2 tests).

- [ ] **Step 3: Run the full adminui + server suites**

Run: `go test ./internal/server/... -race`
Expected: PASS (no regressions in existing `adminui_test`/`server_test`).

- [ ] **Step 4: Commit**

```bash
git add internal/server/adminui/adminui_test.go
git commit -s -m "test(adminui): assert data-free, 8-section IA, capabilities wiring

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Final verification (run after all tasks)

- [ ] `gofmt -l .` → no output (all formatted).
- [ ] `go vet ./...` → clean.
- [ ] `go test ./... -race` → PASS.
- [ ] `CGO_ENABLED=0 go build -trimpath -o bin/inferplane ./cmd/inferplane` → builds (single static binary, ADR-002 intact).
- [ ] `bash tests/run-all.sh` → harness tests pass.
- [ ] Manual smoke (optional): `go run ./cmd/inferplane serve --config examples/config.json`, open `/admin/ui/`, unlock with the admin token, confirm 8 nav sections render and Usage/Logs/Teams show their affordance cards.

## Scope boundary (explicit)

**In scope (Phase 0a):** capabilities endpoint + wiring; 8-section nav + Usage/Logs/Teams affordance shells; capability-driven first paint; data-free/IA tests.

**Out of scope — next plan (Phase 0b):** the conventional dashboard visual reskin (`style.css`), splitting `app.js` into per-view ES modules + `charts.js`/`ui.js`, hand-rolled SVG sparklines, folding Quickstart into Settings, the LRU client-cache cap. **Out of scope — later phases:** the analytics index + query API (Phase 1, makes Usage/Logs functional), per-key governance fields (Phase 2), body logging (Phase 3), guardrails/region enforcement (Phase 4).
