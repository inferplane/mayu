# Plan: E2E gateway setup verification + minimal admin key console

**Date:** 2026-06-11
**Spec:** [docs/specs/2026-06-10-inferplane-gateway-design.md](../../specs/2026-06-10-inferplane-gateway-design.md)
**Base:** main (v0.1 code-complete, ecd1809 lineage)

## Goal

Two deliverables, both grounded in the v0.1 design:

1. **E2E gateway-setup verification** — `cmd/inferplane` currently has zero tests.
   Nothing boots the assembled binary wiring (config → keystore → providers → router →
   governor → muxes) end-to-end. Add full-stack E2E tests that start the gateway from a
   real config file against an `httptest` upstream, exercising key issuance, the
   `/v1/messages` data path (non-streaming + SSE), governance blocking, the
   `count_tokens` never-non-200 mandate, `/metrics` secret-leak checks, and audit chain
   verification. Plus a regression test that the shipped example configs actually load,
   and a self-hosted (Ollama/vLLM via `openaicompat`) example config.

2. **Minimal admin key console (web UI)** — the spec defers UI to v0.2 ("키 발급
   셀프서비스 페이지 (최소 UI)", §9) and 부록 A explicitly rejects a full UI in core
   ("UI 분리 — 프론트 유지보수 세금"). This plan pulls the *minimal* page forward,
   honoring that decision: a single embedded static page (`go:embed`, vanilla JS, no
   build step, no framework, no npm) served on the **admin plane** at `/admin/ui/`.
   The page itself contains zero data and zero secrets; all data calls go to the
   existing token-gated `/admin/keys` JSON API with a Bearer token the operator pastes
   into the page (held in JS memory only — never localStorage/cookies). Recorded as
   ADR-001.

## Security invariants (must hold throughout)

- No inline secrets in any config (env:/file: refs only).
- Plaintext `ik_...` shown once; UI must not persist it anywhere.
- `/metrics` leaks no secret or `key_id`.
- `count_tokens` never returns non-200.
- Admin UI static assets are data-free; auth stays on the JSON API (`AdminTokenAuth`),
  no new auth path, no token in URL/query/localStorage.
- Strict CSP on UI responses (`default-src 'self'`), `X-Content-Type-Options: nosniff`,
  `Cache-Control: no-store` on key-creation responses.

---

### Task 1: Extract testable run() from cmd/inferplane (tidy-first, no behavior change)

`main.go`'s `serve` path calls `http.Server.ListenAndServe` directly, which makes the
binary unbootable from a test (no port discovery, no readiness, no clean shutdown
handle). Extract the serve wiring into `run(ctx, cfgPath string) (*gateway, error)`
(or equivalent) that: binds listeners explicitly via `net.Listen` (so `127.0.0.1:0`
works and bound addrs are discoverable), exposes `DataAddr()`/`AdminAddr()`, and shuts
down cleanly on ctx cancel. `main` becomes a thin caller. Pure refactor — existing
behavior (TLS branch, drain grace, graceful shutdown) preserved.

**Files:**

- Modify: `cmd/inferplane/main.go`
- Create: `cmd/inferplane/gateway.go`
- Test: `cmd/inferplane/gateway_test.go`

**Steps:**

- [ ] Write failing test: `TestGatewayBootsAndShutsDown` — start with a minimal temp
      config on `127.0.0.1:0`; assert admin plane `/healthz` 200 (healthz exists on the
      admin mux only) and data-plane reachability via `GET /v1/models` returning 401
      without a key (proves the listener + auth stack are up); cancel ctx, assert clean
      exit within drain grace.
- [ ] Extract the serve wiring from `main.go` into `gateway.go` (`run`/`gateway`
      struct), switching `ListenAndServe` → `net.Listen` + `srv.Serve(ln)`. TLS branch
      (currently `ListenAndServeTLS`, main.go:189) becomes
      `srv.ServeTLS(ln, certFile, keyFile)` — behavior preserved.
- [ ] `go test ./... -race` green; `go vet ./...`; `gofmt -l .` clean.
- [ ] Commit (DCO sign-off).

### Task 2: E2E happy path — config boot, key issuance, /v1/messages round-trip

Full-stack test: temp config (JSON written to `t.TempDir()`, secrets via `t.Setenv`
env refs — never inline), `anthropic`-type provider whose `base_url` points at an
in-test `httptest` server speaking the Anthropic Messages protocol, SQLite keystore in
temp dir, file audit sink in temp dir.

**Files:**

- Create: `cmd/inferplane/e2e_test.go`

**Steps:**

- [ ] Write failing test `TestE2EMessagesRoundTrip`: boot gateway → POST
      `/admin/keys` with Bearer admin token (env ref) → receive plaintext `ik_...` →
      POST `/v1/messages` with `x-api-key: ik_...` → 200 with upstream-shaped body;
      assert the upstream saw the **gateway's** provider key, not the client key (§5.2).
- [ ] Write failing test `TestE2ECountTokensNeverNon200`: count_tokens against a
      provider with no TokenCounter and against a failing upstream — always HTTP 200.
- [ ] Write failing test `TestE2EMetricsLeakFree`: scrape `/metrics`, assert no
      `ik_`, no admin token, no `key_id` label values appear.
- [ ] Make all three pass using only existing production code (fix wiring if any
      breaks; zero provider/core behavior change expected).
- [ ] `go test ./... -race` green. Commit.

### Task 3: E2E streaming, governance block, audit chain verify

**Files:**

- Modify: `cmd/inferplane/e2e_test.go`

**Steps:**

- [ ] Write failing test `TestE2EStreamingSSE`: upstream emits SSE; client receives
      the tee'd stream verbatim through the gateway (cache invariant §4.4 — byte-equal
      frames for same-protocol passthrough).
- [ ] Write failing test `TestE2EGovernanceBlocks`: team config with tiny
      `rate_limit.requests_per_minute` (actual JSON tag, config.go:97 — NOT `rpm`) and
      `budget.usd_per_month` + `on_exceeded: "block"` (config.go:112-113) — second
      request 429 (rate) and a budget-exhausted request 402 (messages.go:149); with
      `on_exceeded: "warn"` the request passes (200). The test config MUST include a
      `pricing.overrides` rate for the mock model: the default
      `pricing.on_missing=allow` prices unknown models at 0 µUSD, so budget would
      never debit and the 402 path could not trigger.
- [ ] Write failing test `TestE2EAuditChainVerifies`: after the above traffic, run
      `audit.Verify` on the emitted file sink — chain valid; tamper one byte → verify
      fails.
- [ ] Make tests pass. `go test ./... -race` green. Commit.

### Task 4: Example configs load-test + self-hosted (Ollama/vLLM) example

"5분 안에 붙는다" (§9 v0.1 success bar) requires the shipped examples to actually
load. Add a regression test that `config.Load` accepts every file in `examples/`
(env refs stubbed), and add a self-hosted example for the `openaicompat` provider
(Ollama `http://localhost:11434/v1` / vLLM) — the third provider class in the spec
that currently has no example.

**Files:**

- Create: `examples/config.selfhosted.json`
- Create: `internal/config/examples_test.go`

**Steps:**

- [ ] Write failing test `TestExampleConfigsLoad`: glob `../../examples/*.json`,
      `t.Setenv` the referenced env vars, `config.Load` each — no error, ≥1 provider,
      ≥1 model route each.
- [ ] Write `examples/config.selfhosted.json`: `openaicompat` provider
      (`base_url: http://localhost:11434/v1`, **no `api_key_ref`** — the field is
      optional (`resolveSecret(nil)` returns "", config.go:192-193) and Ollama is
      typically keyless; this also keeps the load-test free of extra env stubs),
      one model route, sqlite keystore, stdout audit sink, one demo team.
- [ ] Test green. Commit.

### Task 5: ADR-001 — minimal embedded admin key console

**Files:**

- Create: `docs/decisions/ADR-001-minimal-embedded-admin-key-console.md`

**Steps:**

- [ ] Write the ADR: context (spec defers UI to v0.2; 부록 A rejects full UI),
      decision (pull forward *only* the minimal key console: single embedded static
      page, vanilla JS, no build toolchain, served on admin plane), security posture
      (data-free static assets unauthenticated like `/metrics`; all data via existing
      `AdminTokenAuth` JSON API; token in JS memory only; strict CSP; plaintext key
      rendered once, never stored), alternatives rejected (full SPA + npm toolchain;
      server-rendered templates with session auth; postponing to v0.2 entirely),
      consequences (frontend tax capped at 3 static files; self-service OIDC login
      remains v0.2).
- [ ] Commit.

### Task 6: adminui package — embedded static console (TDD)

New package `internal/server/adminui`: `go:embed static/*` + `Handler()` serving
`GET /admin/ui/` (index.html, app.js, style.css). Page: paste admin token → list keys
(table: key_id, team, allowed_models) → create key (team + models form; plaintext
shown once with copy button and "will not be shown again" warning) → revoke. Vanilla
JS `fetch` with `Authorization: Bearer` header; token kept in a JS variable only.

**Files:**

- Create: `internal/server/adminui/adminui.go`
- Create: `internal/server/adminui/static/index.html`
- Create: `internal/server/adminui/static/app.js`
- Create: `internal/server/adminui/static/style.css`
- Test: `internal/server/adminui/adminui_test.go`

**Steps:**

- [ ] Write failing tests: `TestServesIndex` (200, `text/html`, CSP
      `default-src 'self'`, `nosniff`), `TestServesAssets` (js/css correct
      content-types), `TestNoSecretsInAssets` (assets contain no `ik_`, no token, no
      key_id values — static and data-free), `TestUnknownPath404`.
- [ ] Implement `adminui.go` (embed.FS, http.Handler, security headers) + the three
      static files.
- [ ] Static-asset grep guard in test: assert no `localStorage`, no `document.cookie`
      in app.js (token must not persist).
- [ ] `go test ./... -race` green. Commit.

### Task 7: Wire /admin/ui/ into AdminMux + integration tests

**Files:**

- Modify: `internal/server/server.go`
- Modify: `internal/server/server_test.go`

**Steps:**

- [ ] Write failing tests: `TestAdminMuxServesUI` (GET `/admin/ui/` 200 without
      auth — data-free page), `TestAdminMuxUIDoesNotBypassKeysAuth` (POST/GET
      `/admin/keys` still 401 without Bearer token after UI wiring),
      `TestAdminMuxUIRedirect` (`/admin/ui` → `/admin/ui/`).
- [ ] Wire `adminui.Handler()` into `AdminMux` in `server.go`.
- [ ] Browser smoke check via Playwright against a locally running gateway
      (create + revoke a key through the page) — manual gate, not CI.
- [ ] `go test ./... -race` green; `bash tests/run-all.sh` green. Commit.

### Task 8: Docs sync (Auto-Sync rules)

**Files:**

- Modify: `docs/architecture.md`
- Modify: `docs/reference/api.md`
- Modify: `docs/reference/infrastructure.md`
- Modify: `internal/CLAUDE.md`
- Modify: `README.md`

**Steps:**

- [ ] `docs/reference/api.md` + `internal/CLAUDE.md`: document `/admin/ui/` endpoint
      (admin plane, unauthenticated static, data via token-gated `/admin/keys`).
- [ ] `docs/architecture.md`: admin-plane diagram/text gains the console; note
      ADR-001.
- [ ] `docs/reference/infrastructure.md`: note embedded assets ship inside the single
      static binary (no image/chart change).
- [ ] README: one quickstart line — "open `http://localhost:9090/admin/ui/`"; mention
      `examples/config.selfhosted.json` for Ollama/vLLM.
- [ ] `bash tests/run-all.sh` green. Commit.
