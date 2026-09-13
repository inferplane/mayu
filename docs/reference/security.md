# Security

### 1. Overview
Cross-cutting security: virtual-key authentication, team RBAC, key/secret isolation,
inline-secret rejection, optional self-TLS, and metrics that never leak secrets. These
are non-negotiable invariants (see CLAUDE.md → Security mandates).

### 2. Components
| Component | Path | Purpose |
|---|---|---|
| Data-plane auth | `internal/server/auth.go` | `KeyAuth` resolves `ik_...` → Principal |
| Admin auth | `internal/server/adminauth.go` | `AdminAuth`: static break-glass tokens + OIDC ID tokens on one Bearer header (ADR-004); total shape-predicate routing, 401 vs 403, denial audit |
| OIDC verify | `internal/adminauth/` | shared `IsOIDCBearerShape`, groups→team `Resolve`, go-oidc verifier (alg pin, aud/azp, ±60s skew, JWKS negative cache) |
| Console SSO (SPA=public client) | `internal/server/adminui/static/app.js` | ADR-026: OAuth2 Authorization Code + PKCE runs in the browser; gateway stays a resource server. `sessionStorage` holds ONLY `ip_sso_verifier`/`ip_sso_state`/`ip_sso_nonce` (cleared on every callback exit); id_token is memory-only. Callback follows RFC 6749 §4.1.2.1 (state-before-error, textContent-only error, replaceState-before-exchange, nonce check); discovered endpoints must be https. Opt-in via `oidc.login_origins` (empty ⇒ CSP byte-identical, SSO hidden) |
| Console CSP connect-src | `internal/server/adminui/adminui.go` + `cmd/mayu/gateway.go` (`ssoConnectSrc`) | ADR-026: when `login_origins` set, `connect-src 'self' <issuer-origin> <login_origins…>` (issuer path stripped so it can't break the source expression); script/style stay `'self'`. Empty ⇒ `default-src 'self'; frame-ancestors 'none'` unchanged |
| CLI login (loopback PKCE) | `cmd/mayu/login.go`, `internal/server/authapi/` | ADR-028: `mayu login` runs Authorization Code + PKCE against a DISTINCT OIDC `client_id`/`Verifier` from the console's (never share — the console's is a secretless public client); every gateway/issuer URL validated https-or-loopback, discovered endpoints same-origin-or-subdomain of the issuer; issuer/client_id TOFU-pinned into the credential file, silent change refused without `--reset`. No IdP refresh token is ever cached (`credentials.go`) — a stored refresh token would be a stronger, gateway-unrevocable credential than the `ik_` key it protects. `POST /v1/auth/key` decides `expires_at`/`owner` server-side only, rate-limited per subject |
| Config view | `internal/server/configapi/` | secret-free topology projection (ADR-005): view type cannot hold a secret; auth string from ref name / IAM mode only, never the resolved key |
| RBAC | `internal/keystore/keystore.go` | `Principal.Allows()` (team + allowed models) |
| Cross-model fallback RBAC re-check | `internal/router/router.go` (`FilterModelAllowed`) | ADR-029/D5: `ResolveChain` appends a model_fallbacks target's provider chain AFTER the ingress allow-list check already ran against the originally requested model — every ingress handler re-checks those appended targets' model with `FilterModelAllowed` before ever sending a request there, or a key allowed only model A would silently reach fallback model B. A key allowed only the unconfigured requested name is denied outright (fail-closed), never silently downgraded to a configured fallback |
| Key hashing | `internal/keystore/sqlite.go` | SHA-256 at rest; plaintext shown once (or, for a declaratively-bootstrapped key, ADR-023, referenced via `virtual_keys[].key_ref` — never inline, same §7 posture as a provider's `api_key_ref`) |
| TLS validation | `internal/server/tls.go` | rejects half-specified cert/key pairs |
| Secret refs | `internal/config/config.go` | `env:`/`file:`/`secret:` only; inline `api_key` rejected |
| Credential brokering | `internal/controlplane/broker.go`, `internal/proxy/credentials.go`, `providers/bedrock/client.go` | ADR-040: inferplaned vends ≤1h STS Bedrock sessions to mayu so node IAM can hold ZERO Bedrock permissions — enforcement moves from convention to IAM, **on hosts where mayu's env is not readable by its users** (a developer-owned machine can read `broker_token_ref` and mint directly; there brokering is IAM hygiene, not a bypass control). Machine channel only: a dedicated `INFERPLANED_BROKER_TOKEN`, distinct from `INFERPLANED_TOKEN` and rejected at boot if equal, with a JWT-shaped bearer 403'd unverified so a console SSO session can never mint credentials. FAIL-CLOSED: `auth.mode: "broker"` with no injected source is a construction error (never the node's default credential chain), the first fetch is EAGER so a bad broker fails BuildState instead of user traffic, an unknown `auth.mode` is a load error, and a non-loopback plain-`http` control-plane URL is rejected. SECRET HYGIENE: no credential field is ever logged; the fetcher's non-2xx/decode errors carry a status code and fixed strings only (no `%w` on JSON errors — `time.Time`'s parse error echoes the value), and an STS failure returns a fixed 502 body. **v1 limitation:** the dataplane id is caller-chosen, so CloudTrail attribution is "which id was claimed", never "which machine called", and brokered sessions carry unrestricted `bedrock:Invoke*` — condition the broker role on `aws:SourceIp`/`aws:SourceVpce` (runbook, not enforceable here) |
| Metrics safety | `internal/metrics/metrics.go` | no `key_id`/secret labels; `_rejected` sentinel bounds cardinality |

### 3. Key Decisions
- The gateway never forwards the client key upstream and never exposes its upstream keys to clients (§5.2).
- `/metrics` is unauthenticated but carries no secret or `key_id` and bounds label cardinality.
- Pre-resolution 403/404 paths use a sentinel model label so attacker-supplied model strings can't explode Prometheus series.
- ADR-029 model-level fallback fails closed on RBAC: a key allowed only the requested (unconfigured) model name is 403'd, never silently served by its fallback model; a cross-model fallback target appended after the allow-list check is re-checked via `FilterModelAllowed`.
- ADR-040 credential brokering fails closed in both directions: a brokered provider that cannot obtain credentials errors rather than falling back to the node's own AWS identity, and the credential endpoint has no OIDC branch at all rather than one that could be mis-routed into. Both are pinned by mutation-checked tests, not just by review.

### 4. Code Pointers
- `internal/server/auth.go` — virtual-key auth, empty-key bypass guard
- `internal/config/config.go` — secret-ref resolution + inline-secret rejection
- `internal/server/anthropicapi/messages.go` / `openaiapi/chat.go` — `_rejected` label on 403/404

### 5. Cross-references
- Related modules: `internal/keystore`, `internal/audit`, `internal/metrics`
- Related ADRs: docs/decisions/ADR-004-oidc-admin-authz.md, docs/decisions/ADR-026-console-sso-login.md, docs/decisions/ADR-028-cli-oidc-login-short-lived-keys.md, docs/decisions/ADR-029-model-level-fallback.md, docs/decisions/ADR-040-credential-brokering.md
- Related runbooks: docs/runbooks/ ; docs/runbooks/cli-login.md ; policy in `SECURITY.md`

### Sensitive-data destination restrictions (ADR-043)

`InternalOnly` requires an approved canonical model AND a provider explicitly
attested `internal` on every attempt. Unknown/omitted boundary is not trusted.
All matching rules intersect, Block wins, and no safe chain denies before masking,
admission or provider calls. Privacy always enforces, including context Shadow;
optional context failures keep the already-safe route. Original model RBAC must
pass. New alternatives need pricing/context/capability and physical ingress checks.

The local inspector has finite email/phone/Luhn-card/SSN/IPv4/Korean-ID coverage,
not universal PII detection. Opaque/unknown content is uninspectable; malformed or
cancelled inspection fails closed under sensitive rules. Legacy opt-in masking is
independent and does not cover every inspected surface. Provider labels are
operator assertions, not endpoint verification. No prompts/detected values enter
new audit/headers/metrics; the routing counter labels only team/mode/reason.

A rejected distributed privacy generation gates affected subjects until valid
recovery. Boot local policy is revalidated against usable topology before serving.
Upgrade mayu/inferplaned/CRD before activating new rules. `require_sync` is necessary
for CP privacy before first sync; `max_policy_age` bounds readiness staleness. Both
count APIs stay local/200 while unready, stale, or refused. See
[limits and rollout](../policy-routing.md).

ADR-044 adds complete finite Mask transformation before an egress chain is
returned. Masking remains required even alongside InternalOnly. Unsafe structural
or numeric changes, remaining detections and incomplete transformations refuse.
Responses remote-tool egress cannot be authorized merely by an internal model
label. Strict budget constraints also gate rejected policy updates and filter
every subsequent context choice, affinity hit and fallback. Session hints are
scoped HMAC inputs only; they never become principals or audit dimensions.
