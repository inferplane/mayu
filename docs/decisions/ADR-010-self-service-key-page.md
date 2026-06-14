# ADR-010: Self-service key issuance — identity endpoint + console

**Date:** 2026-06-14
**Status:** Accepted — design gate passed (gemini PASS; codex + kiro CHANGES-REQUIRED, all findings test/serialization hardening — architecture unanimously sound, no new authz surface). Folded in: dedicated PII-free DTO + exact-shape test, non-admin 403 regression test, explicit 401 test, teams:[] not null, whoami not audited (read-only).
**Related:** ADR-004 (OIDC admin authz — `AdminIdentity`, groups→team `Resolve`,
`Entitled`), ADR-001/002 (console), ADR-008 (console write patterns), §5.1/§5.5

## Context

A developer who authenticates with their IdP should be able to mint **their own**
virtual key for **their own team** without an operator handing them a break-glass
admin token. The building blocks already exist:

- `AdminAuth` admits OIDC ID tokens and builds an `AdminIdentity{Subject, Teams,
  IsAdmin}` (ADR-004); non-admin identities pass the middleware.
- `POST /admin/keys` already enforces **per-team entitlement** in the handler:
  `if !id.Entitled(body.Team) { 403 }`. A team-mapped non-admin is `Entitled` to
  its own team(s), so it can **already** issue a key for them.
- The console already accepts "admin token **or** an OIDC ID token" to unlock.

So self-service issuance is already possible at the API level. What is missing is
(a) a way for the console to **know who the caller is** and which teams they may
use — today the team is a free-text field, so a developer can type a team they
are not entitled to and only discover the `403` after submitting; and (b) a
self-service UX that prefills/constrains the team and shows the signed-in
identity, distinct from the admin "issue for any team" flow.

## Decision

**Add a secret-free identity endpoint `GET /admin/whoami` and use it in the
console to drive a self-service key flow.** No new key-issuance authorization is
introduced — entitlement stays exactly as `POST /admin/keys` already enforces it.

### 1. `GET /admin/whoami` (behind `AdminAuth`)

Returns the caller's resolved admin identity, secret-free:

```json
{ "subject": "<opaque OIDC sub or 'break-glass'>",
  "teams": ["alpha", "beta"],
  "is_admin": true,
  "auth_method": "oidc" }
```

It reflects the `AdminIdentity` the middleware already put in the request context
— the opaque subject (never an email/PII, ADR-003/004), the entitled teams, the
admin flag, and the auth method. It carries **no token and no secret**; it is the
identity the gateway already knows, surfaced for the UI. (A new
`principal.AdminFrom`-backed handler; the middleware is unchanged.)

**Serialized via a dedicated response DTO, never the `AdminIdentity` struct
directly** (gate, codex/kiro): a hand-built `{subject, teams, is_admin,
auth_method}` struct with explicit lowercase json tags and `teams` initialized to
`[]` (never `null`) makes the PII-free shape **structural** — a future field
added to `AdminIdentity` (email, raw claims, groups) cannot leak through `whoami`
because it would have to be added to the DTO on purpose. A test asserts the exact
shape (only those four fields).

`whoami` is **read-only and not audited**: it changes no state and the identity
is already recorded at middleware entry (and on any denial, ADR-004); it is a
self-read, like `GET /admin/config`, which is also not audited.

### 2. Console self-service flow

On unlock, the console calls `/admin/whoami` and adapts:

- **Signed-in identity** is shown (`subject` + `auth_method`), so a developer
  sees who they are.
- **Non-admin** (`is_admin: false`): the key-issuance team field becomes a
  **select constrained to `teams`** (prefilled when there is exactly one) — they
  can only issue for a team they are entitled to, matching the server's `403`
  rule, so the UI never invites a request that will be denied.
- **Admin** (`is_admin: true`): the team stays free-entry (issue for any team) —
  the existing admin behavior, unchanged.
- **Break-glass static token**: `whoami` returns `is_admin: true`, `teams: []`,
  `auth_method: "break_glass"` → admin free-entry (unchanged).

The token remains in page memory only (no storage), exactly as today.

## Alternatives considered

1. **A dedicated `POST /admin/keys/self` endpoint that ignores the team and uses
   the identity's team.** Rejected — it duplicates the issuance path and its
   entitlement logic. `POST /admin/keys` + `Entitled` already does exactly this;
   self-service only needs the UI to know the entitled teams. One issuance path,
   one authz rule.
2. **Decode the OIDC token client-side to get teams.** Rejected — the browser
   would have to parse/trust a JWT and replicate the groups→team mapping
   (`Resolve`), which lives server-side (ADR-004) and is the authority. `whoami`
   returns the server's resolved view, so the UI and the enforcement never drift.
3. **Expose more identity detail (email, raw groups, token claims).** Rejected —
   `whoami` is deliberately minimal and PII-free: opaque subject + resolved teams
   + flags. Raw claims/email would put PII on an endpoint and in console memory.
4. **A separate self-service binary/page.** Rejected — the console already has
   OIDC unlock + key issuance; self-service is a small adaptation of it, not a
   new surface.

## Consequences

- A developer logs in with their IdP token and issues a key for their own team
  from the console — no operator, no break-glass token — within the SAME
  entitlement rule the API already enforces (no new authz surface to audit).
- `whoami` is secret-free and PII-free (opaque subject + resolved teams); it adds
  one read-only admin-plane endpoint behind the existing `AdminAuth`.
- The console no longer invites a doomed cross-team request: non-admins pick from
  their entitled teams; admins keep free entry.
- No change to key issuance, entitlement, or the OIDC middleware — purely
  additive (an identity read + a UI adaptation).
