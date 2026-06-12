# ADR-004: OIDC admin authorization — resource-server-only, break-glass preserved

**Date:** 2026-06-12
**Status:** Accepted
**Deciders:** maintainers; 3-round multi-model design gate (codex gpt-5.5,
gemini 3.1-pro) + internal Plan-architect collaboration — 16 design findings
resolved before implementation
**Related:** spec §5.1 (Identity/Principal/Policy), §5.5 (admin auth), ADR-001/002
(console), ADR-003 (priority 1: free SSO), plan
`docs/superpowers/plans/2026-06-12-oidc-admin-sso.md`

## Context

ADR-003 made free OIDC SSO the top v0.2 item: competitors paywall exactly this.
Spec §5.1 delegates Identity to an external IdP (Dex/Keycloak/Okta) — the
gateway owns only the `groups`→team mapping; §5.5 keeps the static admin token
as break-glass.

## Decision

**The gateway is an OIDC resource server — never an OAuth client, never a
login host.** It validates externally-acquired ID tokens (JWS) on
`Authorization: Bearer` against the issuer's JWKS, offline. No redirect
handler, no session store, no cookies, no PKCE. Humans obtain tokens via their
IdP's CLI/device flow (the kubeconfig exec-credential pattern). The console's
single token field accepts either credential kind.

Why the alternatives are structurally ruled out:
- **Console PKCE-in-JS** requires the browser to fetch the IdP's `/token`
  endpoint — `connect-src 'self'` (from the ADR-001/002 CSP) forbids it.
  Relaxing CSP is a security regression for zero capability gain.
- **Gateway-hosted auth-code flow** requires a session store + redirect URI +
  CSRF defense — the exact "second authentication path" ADR-001 already
  rejected. (The future self-service page may revisit this with its own ADR.)

### Mechanics (each pinned by a gate finding)

- **One shared shape predicate** `adminauth.IsOIDCBearerShape` (3 non-empty
  unpadded base64url segments, 8 KiB cap) routes the middleware AND guards
  config load — a JWT-shaped static token is a load error, so the static and
  OIDC paths are total and mutually exclusive (break-glass can never be
  mis-routed; no fallthrough, no timing oracle).
- **Validation**: alg ∈ {RS256, ES256} only; `iss`; `aud` must contain the
  mandatory `client_id`; multiple audiences require `azp == client_id` (OIDC
  Core §3.1.3.7); exp/iat/nbf with ±60s wrapper-side skew; lazy discovery with
  negative cache + backoff; failure-armed JWKS rate limit (forged-kid floods
  can't hammer the IdP; rotation still works).
- **Issuer hygiene**: config load rejects non-https issuers and any
  query/fragment/userinfo (MITM-JWKS substitution / SSRF-by-config).
- **Mapping**: exact group match + explicit `*`; multi-group team union;
  `admin_groups` ⇒ all teams; no match ⇒ 403, never a default team.
- **Identity**: `principal.AdminIdentity{Subject, Teams, IsAdmin, AuthMethod}`
  under its own context key — email and raw groups never enter the request
  context. Static tokens inject the `break-glass` sentinel.
- **Entitlement in the handler**: members create/revoke keys only for mapped
  teams; handlers fail closed without an identity.
- **Audit**: every admin mutation AND every authenticated denial (middleware
  403 included) emits a chain record with opaque `sub` + `auth_method` —
  never email, never groups (ADR-003 content-free posture). 401s are never
  audited (unauthenticated floods must not grow the chain/WAL). The
  `auth_method` field is appended at the end of `PrincipalRef`; a raw-bytes
  fixture test proves pre-change chains still verify.

## Consequences

- Free SSO with zero new auth surface on the console and zero CSP change;
  the static token survives any IdP outage (verified: JWKS down ⇒ static 200).
- Operators need an IdP that can mint ID tokens via CLI/device flow; the
  `groups` claim must be top-level (configurable name, no nested traversal) —
  Keycloak/Okta users may need a claim mapper.
- New dependency: `coreos/go-oidc/v3` (+ x/oauth2, go-jose) — pure Go,
  CGO_ENABLED=0 preserved.
- The v0.2 self-service page builds on AdminIdentity.Teams as designed.
