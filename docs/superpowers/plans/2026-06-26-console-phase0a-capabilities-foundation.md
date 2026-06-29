# Console Phase 0a — Capability Negotiation + 8-Section IA Scaffold Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a token-gated `GET /admin/capabilities` endpoint and wire the console to fetch it on unlock, so the 8-section IA renders each section's enabled/disabled state from a single bootstrap call — never by probing endpoints and catching 404/5xx.

**Architecture:** A new `configapi.Capabilities` projection (mirroring the existing secret-free `configapi.View`/`Writable` capability-hint pattern) is served behind `AdminAuth` at `/admin/capabilities`. The assembly (`cmd/inferplane`) computes it from what it already knows (provider store present? PII filter active?). The console fetches it once after unlock and gates each nav section: implemented sections work; not-yet-backed sections (Usage/Logs/Teams) stay navigable but render a disabled-with-reason affordance card. No SPA framework, no build step — vanilla JS + `go:embed` (ADR-002). No client-side persistence (ADR-001).

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
- **Degradation, not errors** (§9.1): a section whose capability is off stays navigable and renders a calm affordance ("Enable the analytics store to see usage history"), never an error or blank paint. The shared `api()` maps `404`/`405`/`501` to *disabled* **only for opt-in optional calls** — required endpoints still throw.
- **8 sections (§5)**: Overview, Usage, Logs, Virtual keys, Teams & Users, Providers & Models, Governance, Settings.
- Commit style: DCO sign-off (`git commit -s`); end body with `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`.

---

## File Structure

- `internal/server/configapi/capabilities.go` — **Create**. `Capabilities` struct + `CapabilitiesHandler(get func() Capabilities) http.Handler` (GET-only, JSON, secret-free). Lives in `configapi` because it is the same secret-free capability-hint concern as `View.Writable`.
- `internal/server/configapi/capabilities_test.go` — **Create**. Unit tests for the handler (shape, GET-only, no secret).
- `internal/server/server.go` — **Modify** `AdminMux(...)` (signature + one `mux.Handle`) to mount `/admin/capabilities` behind `AdminAuth`.
- `cmd/inferplane/*` (the serve assembly that calls `AdminMux`) — **Modify** to pass a `func() configapi.Capabilities`.
- `internal/server/adminui/static/index.html` — **Modify** nav (8 buttons) + add Usage/Logs/Teams section shells with affordance cards; rename `#view-quickstart` → `#view-settings`.
- `internal/server/adminui/static/app.js` — **Modify**: update the `VIEWS` map (atomic with the HTML), add an `optional` flag to the shared `api()`, fetch capabilities on unlock, render affordances.
- `internal/server/adminui/adminui_test.go` — **Modify**: assert the 8-section IA (nav + sections + `VIEWS` in sync) and the capabilities wiring. (The existing `TestAssetsAreDataFreeAndTokenSafe` already bans `localStorage`/`sessionStorage` — do not duplicate it.)

**Task order is TDD fail-first:** Task 1-2 (Go endpoint + mount, each test-first), Task 3 (IA: failing asset test → HTML + `VIEWS`, atomic), Task 4 (capabilities wiring: failing asset test → JS).

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
- Test: `internal/server/server_test.go` (append; create only if no admin-mux test file exists)

**Interfaces:**
- Consumes: `configapi.CapabilitiesHandler`, `configapi.Capabilities` (Task 1).
- Produces: `AdminMux(...)` gains parameter `capabilities func() configapi.Capabilities` placed **immediately before** the variadic `probeAllowedHosts ...string` (a variadic must stay last). When `capabilities` is nil, the route is omitted (same nil-guard discipline as `configExport`/`m`).

- [ ] **Step 1: Write the failing test**

```go
// internal/server/server_test.go  (append; package server)
// NOTE: ensure these imports exist in the file's import block:
//   "net/http", "net/http/httptest", "strings", "testing",
//   "github.com/inferplane/inferplane/internal/server/configapi",
//   "github.com/inferplane/inferplane/internal/adminauth"
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

> Match the EXACT `AdminMux` argument list at the real call site. The args above assume the current order `(store, adminTokens, verifier, mapping, configView, auditFileSinks, aud, m, writer, configExport, <caps>, probeAllowedHosts...)`. If the real signature differs, copy the real one and add `caps` as the last non-variadic parameter. The `nil` for `configView`/`writer`/etc. is fine — those routes simply aren't exercised by this test.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/server/ -run TestAdminMux_capabilitiesEndpoint -v`
Expected: FAIL — too many arguments to `AdminMux` / route 404.

- [ ] **Step 3: Add the parameter and mount the route**

In `internal/server/server.go`, change the `AdminMux` signature to add `capabilities func() configapi.Capabilities` immediately before `probeAllowedHosts ...string`:

```go
func AdminMux(store keystore.Store, adminTokens []string, verifier OIDCVerifier, mapping adminauth.MappingConfig, configView func() configapi.View, auditFileSinks []string, aud *audit.Writer, m *metrics.Metrics, writer configapi.Writer, configExport func() configapi.ExportDoc, capabilities func() configapi.Capabilities, probeAllowedHosts ...string) http.Handler {
```

Mount the route right after the `mux.Handle("/admin/config", ...)` line. `denied` is the existing local in `AdminMux` (`denied := adminDenialEmitter(emit)`, declared above the `/admin/keys` mount) — reuse it:

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

> Substitute the assembly's real local names (`providerWriter`, `piiFilterActive`, etc.) — grep the call site to read them. If a "PII filter active" boolean is not readily available at the call site, set `Guardrails: false` for Phase 0a and leave a `// TODO(phase4): wire guardrail status` — this is the one acceptable deferral and must be a real, named follow-up, not a vague placeholder.

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

### Task 3: 8-section IA — failing asset test → `index.html` sections/nav + `VIEWS` map (ATOMIC)

The HTML section ids and the JS `VIEWS` map MUST change in the **same commit**: `showView` (`app.js:35`) iterates `Object.keys(VIEWS)` and does `$("view-"+v).hidden = v !== name`. If the HTML renames `#view-quickstart`→`#view-settings` but `VIEWS` still has `quickstart`, the next `showView` call dereferences a null section and throws. This task touches `index.html` **and** `app.js` (the `VIEWS` const only) together.

**Files:**
- Modify: `internal/server/adminui/static/index.html`
- Modify: `internal/server/adminui/static/app.js` (the `VIEWS` const at line ~33 only)
- Test: `internal/server/adminui/adminui_test.go`

**Interfaces:**
- Consumes: the existing `get(t, path) (*http.Response, string)` helper (`adminui_test.go:11`).
- Produces: nav buttons + sections for `data-view`/`id` values `overview, usage, logs, keys, teams, providers, governance, settings`; affordance cards `<div class="card affordance" data-cap="...">` in the Usage/Logs/Teams sections; `VIEWS` map updated to those 8 keys (no `quickstart`).

- [ ] **Step 1: Write the failing test** (append to `adminui_test.go`; `strings` is already imported there):

```go
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
	// quickstart was renamed to settings — old id and VIEWS key must be gone.
	if strings.Contains(html, `id="view-quickstart"`) {
		t.Error("index.html still has id=view-quickstart; rename it to view-settings")
	}
	if strings.Contains(js, "quickstart:") {
		t.Error("app.js VIEWS still has quickstart key; replace it with settings")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/server/adminui/ -run TestAdminUI_eightSectionIA -v`
Expected: FAIL — missing `usage`/`logs`/`teams`/`settings` nav/section/VIEWS entries; `view-quickstart` still present.

- [ ] **Step 3: Replace the nav block in `index.html`** (currently 5 buttons, ~lines 35-41) with 8, preserving the existing icon/`&ensp;` style:

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

- [ ] **Step 4: Add the Usage/Logs/Teams section shells.** Insert Usage + Logs after `#view-overview` (before `#view-keys`); insert Teams after `#view-keys`:

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

- [ ] **Step 5: Rename Quickstart → Settings.** Change the section opening tag `<section id="view-quickstart" ...>` to `<section id="view-settings" ...>`. **Keep every inner element id unchanged** (`usage-claude`, `usage-curl`, `usage-openai`, `usage-models`, etc.) so the existing `renderUsage()` snippet-filler keeps targeting them. (Connection snippets are legitimately settings-adjacent; Phase 0b adds routing/caching/compliance toggles under it.)

- [ ] **Step 6: Update the `VIEWS` map in `app.js`** (the const at ~line 33) — ATOMIC with the HTML above:

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

> Do not confuse `renderUsage()` (the Quickstart/Settings snippet filler) with the new `usage` *analytics* view — they are unrelated; `renderUsage()` is unchanged.

- [ ] **Step 7: Run the test to verify it passes**

Run: `go test ./internal/server/adminui/ -run TestAdminUI_eightSectionIA -v`
Expected: PASS.

- [ ] **Step 8: Commit (both files + test together — atomic)**

```bash
git add internal/server/adminui/static/index.html internal/server/adminui/static/app.js internal/server/adminui/adminui_test.go
git commit -s -m "feat(adminui): 8-section IA — nav + Usage/Logs/Teams shells + VIEWS map (spec §5)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 4: Capability wiring — failing asset test → `optional` api(), bootstrap fetch, affordances

**Files:**
- Modify: `internal/server/adminui/static/app.js`
- Test: `internal/server/adminui/adminui_test.go`

**Interfaces:**
- Consumes: `GET /admin/capabilities` (Tasks 1-2); the `data-cap` attributes (Task 3); the existing `api(method, path, body)` (`app.js:11`), `loadWhoami`/`showView` and the `#token-form` unlock handler (`app.js:736`).
- Produces: `api(method, path, body, optional)` — a 4th param; when `optional` is true a `404/405/501` returns the `DISABLED` sentinel instead of throwing (required calls are unchanged). `let caps`, `async function loadCapabilities()`, `function applyCapabilities()` (hides/shows `.affordance[data-cap]` cards — it does **not** disable nav buttons; sections stay navigable per §9.1), `function capOn(key)` returning a strict boolean.

- [ ] **Step 1: Write the failing test** (append to `adminui_test.go`):

```go
func TestAdminUI_capabilitiesWired(t *testing.T) {
	_, js := get(t, "/app.js")
	for _, want := range []string{"/admin/capabilities", "loadCapabilities", "applyCapabilities", "DISABLED"} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js missing %q — capability wiring incomplete (spec §4.4/§9.1)", want)
		}
	}
	// The unlock path must load capabilities before first paint.
	if !strings.Contains(js, "await loadCapabilities()") {
		t.Error("app.js does not await loadCapabilities() (must run on unlock, before showView)")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/server/adminui/ -run TestAdminUI_capabilitiesWired -v`
Expected: FAIL — `loadCapabilities`/`DISABLED`/etc. not found.

- [ ] **Step 3: Extend the EXISTING `api()` with an opt-in `optional` flag.** The real `api()` (`app.js:11-29`) inlines headers and returns `resp.json()` (or `null` on 204). Add a `DISABLED` sentinel above it and a 4th `optional` param; only return `DISABLED` when `optional` is true — required callers still throw. Result reads exactly:

```js
const DISABLED = Symbol("capability-disabled");

async function api(method, path, body, optional) {
  const resp = await fetch(path, {
    method,
    headers: {
      "Authorization": "Bearer " + adminToken,
      ...(body ? { "Content-Type": "application/json" } : {}),
    },
    body: body ? JSON.stringify(body) : undefined,
  });
  if (resp.status === 401) throw new Error("unauthorized — check the admin token");
  // Opt-in only: an optional/capability endpoint that is absent → disabled,
  // not an error (§9.1). Required calls (optional falsy) still throw below.
  if (optional && (resp.status === 404 || resp.status === 405 || resp.status === 501)) return DISABLED;
  if (!resp.ok && resp.status !== 204) {
    let msg = "API error " + resp.status;
    try { const j = await resp.json(); if (j && j.error) msg = j.error; } catch { /* keep generic */ }
    throw new Error(msg);
  }
  return resp.status === 204 ? null : resp.json();
}
```

> Existing callers pass 3 args, so `optional` is `undefined` (falsy) for them — their behavior is unchanged. Do not introduce `localStorage`/`sessionStorage` (C1).

- [ ] **Step 4: Add capability load + apply** (place near the other `refresh*`/`load*` functions):

```js
let caps = null;

async function loadCapabilities() {
  const out = await api("GET", "/admin/capabilities", null, true); // optional=true
  caps = (out && out !== DISABLED) ? out : {};   // absent endpoint → all-off (safe default)
  applyCapabilities();
}

// Each affordance card declares the capability it needs via data-cap; when the
// capability is present we hide the "enable X" card. Nav buttons are NOT
// disabled — sections stay navigable and show the affordance (§9.1).
function applyCapabilities() {
  document.querySelectorAll(".affordance[data-cap]").forEach((el) => {
    el.hidden = capOn(el.dataset.cap);
  });
}

// capOn maps a capability key to a strict boolean. analytics_index is an enum
// ("A"|"B"|"off"); everything else is a bool.
function capOn(key) {
  if (!caps) return false;
  if (key === "analytics_index") return !!(caps.analytics_index && caps.analytics_index !== "off");
  return !!caps[key];
}
```

- [ ] **Step 5: Call `loadCapabilities()` on unlock.** In the `#token-form` submit success path (`app.js:736-757`), insert it immediately after `await loadWhoami();` and before `showView("overview");`:

```js
    await loadWhoami(); // self-service identity + team scoping (ADR-010)
    await loadCapabilities(); // capability-driven section affordances (spec §9.1)
    showView("overview");
```

> `refreshKeys()` earlier in the handler is the auth gate (throws → stays locked), so the token is known-good by the time `loadCapabilities()` runs.

- [ ] **Step 6: Run the test + quick scans**

Run: `go test ./internal/server/adminui/ -run TestAdminUI_capabilitiesWired -v`
Expected: PASS.
Run: `grep -nE "localStorage|sessionStorage" internal/server/adminui/static/app.js || echo clean`
Expected: `clean`.

- [ ] **Step 7: Commit**

```bash
git add internal/server/adminui/static/app.js internal/server/adminui/adminui_test.go
git commit -s -m "feat(adminui): bootstrap /admin/capabilities, capability-driven affordances (spec §9.1)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Final verification (run after all tasks)

- [ ] `gofmt -l .` → no output (all formatted).
- [ ] `go vet ./...` → clean.
- [ ] `go test ./... -race` → PASS (including the existing `TestAssetsAreDataFreeAndTokenSafe`, untouched).
- [ ] `CGO_ENABLED=0 go build -trimpath -o bin/inferplane ./cmd/inferplane` → builds (single static binary, ADR-002 intact).
- [ ] `bash tests/run-all.sh` → harness tests pass.
- [ ] Manual smoke (optional): `go run ./cmd/inferplane serve --config examples/config.json`, open `/admin/ui/`, unlock with the admin token, confirm 8 nav sections render and Usage/Logs/Teams show their affordance cards.

## Scope boundary (explicit)

**In scope (Phase 0a):** capabilities endpoint + wiring; 8-section nav + Usage/Logs/Teams affordance shells; Quickstart→Settings rename; capability-driven first paint; IA + capabilities asset tests.

**Out of scope — next plan (Phase 0b):** the conventional dashboard visual reskin (`style.css`), splitting `app.js` into per-view ES modules + `charts.js`/`ui.js`, hand-rolled SVG sparklines, real Settings toggles, the LRU client-cache cap. **Out of scope — later phases:** the analytics index + query API (Phase 1, makes Usage/Logs functional), per-key governance fields (Phase 2), body logging (Phase 3), guardrails/region enforcement (Phase 4).
