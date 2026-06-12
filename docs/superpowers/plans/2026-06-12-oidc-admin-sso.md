# Plan: Free OIDC SSO — admin-plane IdP-group authorization (v0.2 #1)

**Date:** 2026-06-12
**Spec:** §5.1 (Identity/Principal/Policy), §5.5 (admin auth, break-glass)
**ADR:** ADR-003 priority 1; produces ADR-004
**Base:** main @ 8365d6b

## Goal

The gateway becomes an OIDC **resource server** for the admin plane: it accepts
externally-acquired ID-token JWTs on `/admin/keys*`, validates them offline
against the IdP's JWKS, maps the `groups` claim to teams (config-owned rules,
§5.1), and enforces per-team key-issuance authorization. The static admin token
remains as **break-glass** (§5.5) and must keep working with the IdP down.

Explicitly NOT in scope (recorded in ADR-004): gateway-hosted login/redirect
flow, console PKCE (structurally impossible under CSP `default-src 'self'` —
the IdP `/token` fetch would violate `connect-src`), data-plane OIDC (virtual
keys unchanged), RBAC beyond admin/team-member, token issuance, config
hot-reload, userinfo calls.

## Design decisions (chair + Plan-agent collaboration, pre-reviewed)

- **Resource-server-only**: humans obtain tokens via their IdP's CLI/device
  flow (kubelogin-style); the console gains a paste field — no redirect URI,
  no session store, no cookies, no CSP change.
- **Dependency**: `coreos/go-oidc/v3` + `golang.org/x/oauth2` (pure-Go, CNCF
  ecosystem standard; CGO_ENABLED=0 preserved).
- **Validation**: ID token; `alg` hard-pinned to {RS256, ES256}; `iss`;
  audience semantics pinned (panel r1): **`aud` must contain the configured
  `client_id` (mandatory, empty ⇒ load error)**, and a token with MULTIPLE
  audiences is rejected unless `azp` is present and equals `client_id`
  (OIDC Core §3.1.3.7 — prevents cross-app token reuse); `exp`/`nbf`/`iat`
  with ±60s skew — go-oidc exposes no leeway knob, so the wrapper does its own
  skew-aware checks (built-in expiry check disabled, replaced); JWKS lazy
  fetch + TTL cache + rate-limited rollover refetch on unknown `kid` +
  **negative cache with backoff for discovery/JWKS failures** (an IdP outage
  flood must not hammer the IdP nor add admin-plane latency).
- **Mapping**: exact group match + explicit `*`; multi-group ⇒ team **union**;
  `admin_groups` ⇒ all teams; **no match ⇒ 403** (authenticated, unauthorized).
- **Composition**: ONE middleware; **total** bearer-shape discriminator pinned
  (panel r2): when OIDC is configured, ANY bearer consisting of 3
  dot-separated base64url segments routes to the OIDC path — no JSON-header
  sniffing (a non-JSON header must not demote a token to the static path);
  everything else routes to static. `alg` enforcement happens INSIDE the
  verifier. The Task-1 load guard makes a 3-segment static token impossible,
  so the rule is total and unambiguous; with OIDC absent, everything routes
  to static (back-compat). Paths mutually exclusive — no fallthrough in
  either direction (auth-bypass/timing-oracle risk).
- **Identity**: new `principal.AdminIdentity` under a separate ctx key from the
  data-plane Principal; break-glass injects sentinel `{Subject: "break-glass",
  IsAdmin: true}`. PII-minimal by construction (panel r1): the type carries
  **only `{Subject, Teams, IsAdmin, AuthMethod}`** — email and raw IdP groups
  never enter the request context; groups are consumed inside the middleware's
  mapping step and dropped.
- **Static-token collision guard** (chair + both panels, r1): a static admin
  token that happens to be JWT-shaped (3 dot-separated base64url segments)
  would be routed to the OIDC path and locked out — breaking break-glass
  during an IdP outage. Config load REJECTS JWT-shaped values in
  `admin_auth.token_refs` whenever the `oidc` block is present.
- **Audit**: `sub` (opaque) into existing `PrincipalRef.User` — **never email,
  never raw groups** (PII / ADR-003 content-free posture); new additive
  `auth_method` field. Also closes a pre-existing §5.5 gap: admin key
  create/revoke currently emit NO audit record at all.

## Security invariants (must hold throughout)

- Break-glass static token works with OIDC configured AND the JWKS/IdP
  unreachable (no network on the static path).
- `alg: none` and HS256 (public-key-as-HMAC confusion) tokens are rejected.
- With OIDC configured, ANY 3-dot-segment bearer (even a garbage one like
  `a.b.c`) goes to the OIDC path and gets a clean 401 there — it is NEVER
  compared against static hashes; non-3-segment bearers never reach the
  verifier. No fallthrough in either direction.
- Empty `client_id` in config with an OIDC block present is a config LOAD
  error; so is a JWT-shaped static token_ref.
- Admin-plane denials are governance events too: a 403'd create/revoke
  attempt emits an audit record (no secrets, no PII).
- Non-admin identities can create/revoke keys only for their mapped teams —
  enforced in the handler, not just the middleware (else authZ is cosmetic).
- No email/groups in audit records by default; `/metrics` stays leak-free.
- All tests run without a real IdP (httptest JWKS + self-minted JWTs).

---

### Task 1: OIDC config schema + load-time validation

**Files:**

- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

**Steps:**

- [ ] Failing tests: `TestOIDCConfigLoads` (full block parses: issuer,
      client_id, groups_claim default `"groups"`, admin_groups,
      group_mappings[]{group, teams[]}); `TestOIDCConfigRejectsMissingClientID`
      (block present + empty client_id/issuer ⇒ load error);
      `TestOIDCConfigRejectsDuplicateGroupKeys`;
      `TestOIDCConfigRejectsJWTShapedStaticToken` (oidc block present + a
      token_ref resolving to a 3-segment JWT-shaped value ⇒ load error naming
      the offending ref — preserves the break-glass invariant; without the
      oidc block, any token shape loads as before).
- [ ] Add `OIDCConfig` under `AdminAuth` (json `oidc`, pointer — nil = absent).
      Pure schema + validation; NO network at load.
- [ ] `go test ./internal/config/ -race` green. Commit (DCO sign-off).

### Task 2: AdminIdentity type + context plumbing

**Files:**

- Modify: `internal/principal/principal.go`
- Create: `internal/principal/principal_test.go`

**Steps:**

- [ ] Failing tests: `TestWithAdminAndAdminFrom` (round-trip, separate ctx key
      — storing AdminIdentity does NOT shadow the data-plane Principal and
      vice versa); `TestAdminFromAbsent`.
- [ ] Add `AdminIdentity{Subject string; Teams []string; IsAdmin bool;
      AuthMethod string}` + `WithAdmin`/`AdminFrom` (new key). No email, no
      raw groups — PII never enters the request context (panel r1).
- [ ] `go test ./internal/principal/ -race` green. Commit (DCO sign-off).

### Task 3: groups→team mapping resolver (new leaf package)

**Files:**

- Create: `internal/adminauth/mapping.go`
- Test: `internal/adminauth/mapping_test.go`

**Steps:**

- [ ] Failing table-driven tests: exact match; multi-group **union**; explicit
      `*` wildcard mapping; `admin_groups` ⇒ isAdmin + all-teams; empty groups
      claim ⇒ `ok=false`; no mapping matches ⇒ `ok=false` (NOT a default team
      — silent privilege grant is banned); duplicate teams deduped.
- [ ] Implement `Resolve(groups []string, cfg MappingConfig) (teams []string,
      isAdmin, ok bool)`.
- [ ] `go test ./internal/adminauth/ -race` green. Commit (DCO sign-off).

### Task 4: JWT verifier wrapper (go-oidc, adversarial suite)

**Files:**

- Create: `internal/adminauth/oidc.go`
- Test: `internal/adminauth/oidc_test.go`
- Modify: `go.mod`

**Steps:**

- [ ] Test harness first: httptest server serving OIDC discovery + JWKS with
      in-test RS256/ES256 keys; helper to mint arbitrary-claim JWTs.
- [ ] Failing adversarial tests: valid RS256 ✓; valid ES256 ✓; `alg: none` ✗;
      HS256 signed with the RSA public key as HMAC secret ✗ (confusion);
      wrong `iss` ✗; `aud` missing client_id ✗; **multi-audience without
      `azp` ✗; multi-audience with `azp == client_id` ✓; `azp != client_id`
      ✗** (OIDC Core §3.1.3.7); expired ✗; `iat` 5min future ✗ (skew leeway
      ±60s: 30s future ✓ — wrapper-side checks, go-oidc has no leeway knob);
      **`nbf` 30s future ✓ (within skew); `nbf` 5min future ✗** (panel r2 —
      same ±60s policy across exp/iat/nbf);
      unknown `kid` after key rotation ⇒ one refetch then ✓; refetch
      rate-limited (forged-kid flood does not hammer the JWKS endpoint);
      **discovery/JWKS outage: repeated JWT attempts hit the negative cache —
      at most one outbound attempt per backoff window, request latency stays
      bounded, and recovery works after the window** (outage flood test).
- [ ] Implement `Verifier` wrapping `coreos/go-oidc/v3`: lazy discovery (IdP
      down at boot ≠ startup failure) with negative cache + backoff,
      `SupportedSigningAlgs=[RS256,ES256]`, built-in expiry check replaced by
      skew-aware wrapper checks, aud/azp rule above, claims extraction with
      configurable groups_claim.
- [ ] `go mod tidy`; confirm CGO-free (`CGO_ENABLED=0 go build ./...`).
- [ ] `go test ./internal/adminauth/ -race` green. Commit (DCO sign-off).

### Task 5: Unified AdminAuth middleware (discriminator is the security boundary)

**Files:**

- Modify: `internal/server/adminauth.go`
- Modify: `internal/server/adminauth_test.go`

**Steps:**

- [ ] Failing adversarial tests: static token still works (back-compat, nil
      verifier ⇒ everything routes static); with OIDC configured: a garbage
      3-segment bearer (`a.b.c`, and one with a non-JSON header) ⇒ 401 from
      the OIDC path and NEVER compared against static hashes (instrumented
      fake verifier asserts the call; static comparison spy asserts no call);
      a JWT with garbage signature ⇒ 401, never static-compared; a non-3-
      segment bearer never reaches the verifier (spy asserts zero calls);
      valid JWT + no group match ⇒ **403**; valid JWT + match ⇒ 200 +
      `AdminIdentity` in context with mapped teams; static token ⇒ sentinel
      `{Subject:"break-glass", IsAdmin:true, AuthMethod:"break_glass"}`;
      **JWKS server down + OIDC configured ⇒ static token still 200, JWT 401**
      (break-glass invariant).
- [ ] Implement `AdminAuth(staticTokens, verifier, next)`: total shape rule —
      OIDC configured AND bearer has exactly 3 dot-separated base64url
      segments ⇒ OIDC path (no header sniffing; `alg` enforcement lives in
      the verifier); else static path. Mutually exclusive, 401/403 semantics.
- [ ] `go test ./internal/server/ -race` green. Commit (DCO sign-off).

### Task 6: Per-team key authZ + admin-action audit records (BEFORE OIDC wiring)

Sequencing pinned by panel r2 (CRITICAL): entitlement enforcement and admin
audit MUST be live before any OIDC identity can reach `/admin/keys` — else an
intermediate commit lets mapped non-admin users create/revoke arbitrary teams
unaudited. This task lands with the AdminMux switched to the new `AdminAuth`
middleware with a **nil verifier** (static-only): behavior is unchanged for
operators (break-glass sentinel IsAdmin ⇒ all teams) but identity injection,
per-team enforcement, and audit are already in force.

Also closes the pre-existing §5.5 gap (admin key create/revoke currently emit
no audit record).

**Files:**

- Modify: `internal/server/adminapi/keys.go`
- Modify: `internal/server/adminapi/keys_test.go`
- Modify: `internal/server/server.go`
- Modify: `internal/server/server_test.go`
- Modify: `internal/audit/record.go`
- Modify: `internal/audit/record_test.go`
- Modify: `cmd/inferplane/e2e_test.go`

**Steps:**

- [ ] Failing tests: team-member (mapped to team A) creating a key for team A
      ⇒ 201; for team B ⇒ **403**; admin ⇒ any team; revoke symmetric
      (team-member revokes only own-team keys); break-glass sentinel ⇒ any
      team (IsAdmin); request with NO AdminIdentity in context ⇒ 403
      (fail-closed default).
- [ ] Failing audit tests: key create/revoke emit an audit record with
      `PrincipalRef.User = sub` (opaque — never email), `Team`, new additive
      `auth_method` field; **denied attempts (403) also emit an audit record**
      (outcome: denied; no secrets, no PII — §5.5 "admin API calls are audit
      events" covers denials, panel r2); record appended at END of
      PrincipalRef (hash chain is line-byte based — old records still
      verify). Mixed-version verify test uses a **raw JSONL byte fixture
      captured from a pre-change audit log** (checked into testdata)
      interleaved with new-format records — byte-exact, not re-serialized
      (panel r1).
- [ ] E2E: `TestE2EAdminActionsAudited` — boot gateway, create+revoke via
      admin API, audit chain verifies and contains both admin events.
- [ ] Wire the audit writer into `adminapi.NewKeysHandler` (signature change)
      and switch `AdminMux` to `AdminAuth(staticTokens, nil, keys)` in
      `server.go` — static-only until Task 7.
- [ ] Full `go test ./... -race` green. Commit (DCO sign-off).

### Task 7: Wire OIDC verifier into AdminMux (enforcement already live)

**Files:**

- Modify: `internal/server/server.go`
- Modify: `internal/server/server_test.go`
- Modify: `cmd/inferplane/gateway.go`

**Steps:**

- [ ] Failing tests: AdminMux with nil OIDC config behaves identically to
      Task 6 state (static-only); with OIDC config, both credential kinds
      reach `/admin/keys` and a mapped team-member is immediately subject to
      the Task-6 entitlement + audit (integration test: OIDC team-member
      cross-team create ⇒ 403 AND audited); `/metrics`, `/healthz`,
      `/admin/ui/` remain unauthenticated.
- [ ] Build verifier from config in `gateway.go` (nil when `oidc` absent);
      thread through `AdminMux` signature.
- [ ] Full `go test ./... -race` green. Commit (DCO sign-off).

### Task 8: Console paste-field + ADR-004 + docs sync

**Files:**

- Modify: `internal/server/adminui/static/index.html`
- Modify: `internal/server/adminui/static/app.js`
- Modify: `internal/server/adminui/adminui_test.go`
- Create: `docs/decisions/ADR-004-oidc-admin-authz.md`
- Modify: `docs/reference/data.md`
- Modify: `docs/reference/security.md`
- Modify: `docs/reference/api.md`
- Modify: `internal/CLAUDE.md`
- Modify: `README.md`

**Steps:**

- [ ] Console: the lock screen's single token field gains help text "admin
      token or OIDC ID token (paste from your IdP CLI)" — same field, same
      `Authorization: Bearer` path, zero new auth logic in JS, no CSP change;
      asset tests still ban storage APIs / `ik_` literals.
- [ ] ADR-004: resource-server-only decision + CSP/PKCE impossibility
      rationale + break-glass invariant + audit PII stance (sub not email).
- [ ] `docs/reference/data.md`: `auth_method` audit field; `security.md`:
      admin-plane authN matrix (static vs OIDC, 401/403); `api.md` +
      `internal/CLAUDE.md`: adminauth package row; README: one OIDC config
      example block.
- [ ] `bash tests/run-all.sh` green. Commit (DCO sign-off).
