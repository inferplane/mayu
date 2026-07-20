# ADR-026: Admin console SSO login — SPA-as-OAuth-public-client (ADR-004 revision)

**Date:** 2026-07-20
**Status:** Accepted
**Deciders:** maintainers; 3-round multi-model plan gate + H4 code gate
(codex gpt-5.6-sol, kiro-cli claude-opus-4.8 / glm-5) — implemented via the
co-agent harness (implementer: codex; host chair: Claude)
**Related:** ADR-004 (OIDC admin authz — resource-server-only), ADR-001/002
(console + CSP), plan `piped-herding-puffin.md`

## Context

ADR-004 kept the admin console (`/admin/ui/`) gated by a single token field:
the operator obtains an ID token from their IdP's CLI/device flow and pastes
it in. That is correct and secure but a poor UX — most operators expect a
"Sign in with SSO" button that opens their IdP's hosted login screen and
unlocks the console automatically.

ADR-004 explicitly ruled out "Console PKCE-in-JS" for **one** structural
reason: the browser must fetch the IdP's `/token` endpoint, and the ADR-001/002
CSP `connect-src 'self'` forbids that. It also left the door open: *"The future
self-service page may revisit this with its own ADR."* This is that ADR.

## Decision

**The gateway stays a pure OIDC resource server. The console SPA becomes the
OAuth2 public client.** The browser performs Authorization Code + PKCE against
the IdP entirely client-side; the resulting **ID token** is then used as a
`Bearer` exactly like the pasted token — so ADR-004's token-validation code
(`adminauth.Verifier`) is completely unchanged. The gateway gains no OAuth
client role, no session store, no cookies, no redirect/callback handler, and no
client secret. The "second authentication path" ADR-004 rejected is not created.

New gateway surface is only two things, both secret-free and opt-in:

1. **`GET /admin/auth/config`** — unauthenticated, returns
   `{"sso": bool, "issuer"?: string, "client_id"?: string}`. `issuer` and
   `client_id` are the **public** identifiers of a PKCE public client (no
   secret exists). Omitted (endpoint returns 404) when OIDC is not configured;
   `{"sso": false}` (issuer/client_id omitted) when OIDC is configured but SSO
   is not opted into.
2. **A config-gated CSP `connect-src` extension** — driven by a new
   `oidc.login_origins` field. When empty, the console CSP is **byte-identical**
   to before (`default-src 'self'; frame-ancestors 'none'`) and the SSO button
   is hidden — i.e. this whole feature is invisible unless explicitly enabled.

### Opt-in switch: `oidc.login_origins`

A list of absolute **https origins** (scheme+host only — no path, query,
fragment, or userinfo; a bare trailing slash is allowed; duplicates rejected;
empty list = SSO off). Setting it does three things: exposes the SSO button,
mounts `/admin/auth/config` with `sso:true`, and extends the console CSP.

The IdP's discovery document and hosted-UI/token endpoints live on origins the
SPA must `fetch`. The CSP `connect-src` therefore becomes
`connect-src 'self' <issuer-origin> <login_origins…>` — where **issuer-origin**
is the scheme+host of `oidc.issuer` with any path stripped (Cognito issuers
carry a `/<poolId>` path that would otherwise break the CSP source expression;
the discovery fetch and the hosted-UI/token fetch are on **different** origins,
so both must be present). `connect-src` only — script/style stay `'self'`. The
authorize redirect is a top-level navigation, not governed by `connect-src`.

### Browser flow (all client-side)

1. On load the SPA fetches `/admin/auth/config`; `sso:true` reveals the button.
2. On click it fetches `<issuer>/.well-known/openid-configuration` to discover
   the authorization and token endpoints (never hard-coded), generates a PKCE
   verifier/challenge + `state` + `nonce`, stashes the three short-lived values
   in `sessionStorage`, and redirects to the IdP's hosted login.
3. The IdP returns to `/admin/ui/?code=…&state=…`; the SPA exchanges the code
   (with the verifier) at the token endpoint for an `id_token`, then unlocks via
   the same seam the manual-paste path uses.

### Invariants (each pinned by a gate finding)

- **`sessionStorage` holds only three keys** — `ip_sso_verifier`,
  `ip_sso_state`, `ip_sso_nonce` — short-lived PKCE values cleared on **every**
  callback exit path (`finally`). The `id_token` is **never** persisted (memory
  only, same as the pasted token); no `localStorage`, no `document.cookie`. A
  static-asset test enforces this, including a ban on bracket/computed
  `sessionStorage[...]` access and `sessionStorage.clear`.
- **Callback ordering follows RFC 6749 §4.1.2.1**: reject `code`+`error` both
  present; reject missing `state`; **validate `state` before trusting any
  `error` param** (a forged error can't drive the UI); render `error_description`
  via `textContent` (XSS-safe, never `innerHTML`); `history.replaceState` strips
  `code`/`state` from the URL **before** the token exchange (no code reuse on
  refresh); reject a missing `id_token`; verify the `nonce` claim.
- **Discovered endpoints must be https** — the SPA refuses a discovery document
  advertising a non-https authorization/token endpoint (defense-in-depth on top
  of the TLS-protected, config-trusted issuer).
- **The manual token field stays** — break-glass static token and CLI-issued
  ID tokens remain valid (ADR-004 invariant). SSO is an additional path.
- **Nonce vs. signature**: the client-side `id_token` decode is signature-blind
  by design (the gateway `Verifier` is the authoritative check when the token is
  used as a `Bearer`); the browser nonce comparison is an anti-injection check,
  not token validation.

### What ADR-004 changes and what it does not

- **Revised**: ADR-004's "zero CSP change" outcome and its rejection of
  PKCE-in-JS are now **conditionally** revised — but only when
  `oidc.login_origins` is set, and only to extend `connect-src`.
- **Unchanged**: resource-server-only posture, token validation
  (alg/aud/azp/skew/lazy-discovery), the `IsOIDCBearerShape` routing, group→team
  mapping (no match ⇒ 403, never a default team), break-glass, and the
  content-free audit posture.

## Consequences

- **Deployment (out of repo).** The IdP app client used by the SPA must be a
  **public client with no secret** (`GenerateSecret=false`) and must have **CORS
  enabled on its token endpoint**, or the browser-side PKCE exchange fails
  outright. Register `AllowedOAuthFlows=[code]` and the callback
  `https://<gateway>/admin/ui/`. Add the hosted-UI origin to
  `oidc.login_origins` (the issuer origin is added automatically).
- **Groups claim footgun.** The SPA requests `scope=openid` only (the gateway
  reads no other scope). RBAC depends on the groups claim: Cognito emits
  `cognito:groups` regardless of scope, so a Cognito deployment MUST set
  `groups_claim: "cognito:groups"` plus `admin_groups`/`group_mappings`, or SSO
  login succeeds but every team-scoped admin call returns 403 (ADR-004:
  unmapped ⇒ 403, never a default team). IdPs that gate group emission behind a
  scope (Okta/Keycloak `groups`/`profile`) would need that scope added — deferred
  until such a deployment exists.
- **`login_origins` is operator-controlled.** Validation blocks paths and
  duplicates but not a deliberate wildcard host (`https://*`); operators should
  list exact origins. Not attacker-reachable (it is server config).
- **Disconnect is a local lock only.** The console's Disconnect clears in-memory
  state and reloads; it does not invalidate the IdP session cookie, so a
  re-click re-logs-in frictionlessly. RP-initiated logout is out of scope.
- **Federation path.** Social IdPs (Google, etc.) extend this with **zero
  console change** via Cognito (or any OIDC IdP) federation.
