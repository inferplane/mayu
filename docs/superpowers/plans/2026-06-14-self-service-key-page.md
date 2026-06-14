# Plan: Self-service key issuance (roadmap #5a)

**Date:** 2026-06-14
**Related:** ADR-010 (this plan), ADR-004 (OIDC identity), ADR-001/002 (console)
**Base:** main @ c1346d0 · **Produces:** `GET /admin/whoami` + console self-service

## Goal

Let an IdP-authenticated developer issue their **own** virtual key for their
**own** team from the console — no break-glass admin token — within the SAME
per-team entitlement the API already enforces. Add a secret-free `whoami`
identity endpoint and adapt the console; no new authorization.

## Core architecture (from ADR-010)

- **`GET /admin/whoami`** (behind `AdminAuth`) returns the resolved
  `AdminIdentity` view: `{subject, teams, is_admin, auth_method}` — opaque
  subject (no PII), no token/secret. Built from `principal.AdminFrom(ctx)`; the
  middleware is unchanged.
- **Console**: on unlock, call `whoami`; show the signed-in identity; for a
  non-admin, constrain the issue-key team to a `<select>` of `teams` (prefilled
  when exactly one); admins keep free entry. Token stays in page memory.
- **No new issuance/authz**: `POST /admin/keys` + `id.Entitled(team)` already
  authorizes self-service; this only surfaces the entitled teams to the UI.

## Hard safety invariants (the gate's checklist)

- **whoami is secret-free + PII-free**: returns only the opaque subject + resolved
  teams + flags — never a token, email, or raw claims. Pinned by a test
  (populate a token/email-shaped subject context, assert only the opaque subject
  is serialized).
- **No authz change**: a non-admin still cannot issue for a team they are not
  entitled to — the server `403` is unchanged; the UI merely doesn't offer it.
  Pinned by a test (non-admin POST for an un-entitled team still 403).
- **whoami behind AdminAuth**: unauthenticated → 401 (no identity leak). Pinned.
- **Console CSP unchanged**: no inline handlers/styles; whoami fetched via the
  token-gated `api()`; identity rendered with `textContent` only. Pinned by the
  adminui structure test.
- **Break-glass unchanged**: a static-token admin sees `is_admin:true, teams:[]`
  → free team entry (existing behavior).

## Tasks

Each task: failing test → minimal code → refactor; one `git commit -s`; all four
gates green (build, test -race, vet+gofmt, tests/run-all.sh).

- [ ] **T1 — `GET /admin/whoami` handler.**
  New `internal/server/adminapi/whoami.go`: a handler that reads
  `principal.AdminFrom(ctx)` and writes a **dedicated DTO** `whoamiResponse{Subject
  string json:"subject"; Teams []string json:"teams"; IsAdmin bool
  json:"is_admin"; AuthMethod string json:"auth_method"}` — NOT the AdminIdentity
  struct (structural PII-free guarantee, gate); `Teams` initialized to `[]string{}`
  so it serializes as `[]` not `null`. 405 on non-GET. Mount in `AdminMux` behind
  the same `AdminAuth` as `/admin/keys`. Tests: **unauthenticated (no token) →
  401**; OIDC identity → its subject/teams/flags; **exact-shape assertion** — the
  JSON has exactly {subject, teams, is_admin, auth_method} and NO email/claims/
  groups/token even when the identity context is seeded with such-looking values;
  break-glass → `is_admin:true`, `teams:[]` (empty array, not null);
  **regression: a non-admin POST /admin/keys for an un-entitled team still → 403**
  (whoami adds no escalation path).
  *Files:* `internal/server/adminapi/whoami.go`,
  `internal/server/adminapi/whoami_test.go`, `internal/server/server.go`,
  `internal/server/server_test.go`.

- [ ] **T2 — console self-service flow.**
  On unlock, `api("GET","/admin/whoami")`; render "signed in as <subject> ·
  <auth_method>"; for a non-admin replace the free-text team input with a
  `<select>` of `teams` (prefill if one); admins keep the text input. All
  CSP-safe (addEventListener, **textContent** rendering — never innerHTML/concat;
  DOM properties; no inline). Tests: adminui structure (whoami via `api()`, no
  bare fetch incl. template-literal, no inline handlers, no secret/PII field;
  identity rendered via textContent); the team select is populated from `teams`.
  *Files:* `internal/server/adminui/static/index.html`,
  `internal/server/adminui/static/app.js`,
  `internal/server/adminui/static/style.css`,
  `internal/server/adminui/adminui_test.go`.

- [ ] **T3 — docs.** `docs/reference/api.md` (whoami endpoint, EN/KO),
  `internal/CLAUDE.md` (whoami), mark ADR-010 Accepted.
  *Files:* `docs/reference/api.md`, `internal/CLAUDE.md`, ADR-010 status.

## File scope (allow-list)

```
docs/decisions/ADR-010-self-service-key-page.md
docs/superpowers/plans/2026-06-14-self-service-key-page.md
internal/server/adminapi/whoami.go
internal/server/adminapi/whoami_test.go
internal/server/server.go
internal/server/server_test.go
internal/server/adminui/static/index.html
internal/server/adminui/static/app.js
internal/server/adminui/static/style.css
internal/server/adminui/adminui_test.go
docs/reference/api.md
internal/CLAUDE.md
```

## Out of scope (explicit)

- New key-issuance or entitlement logic (reuse `POST /admin/keys` + `Entitled`).
- OIDC login/redirect flow in the browser (the user still pastes an ID token from
  their IdP CLI/device flow — same as today; a full browser OAuth dance is a
  separate effort).
- Exposing email / raw claims (PII) on `whoami`.
