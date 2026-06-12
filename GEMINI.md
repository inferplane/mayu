<!-- generated-by: co-agent · source: CLAUDE.md · claude-md-sha: 88e0393ee739 · generated-at: 2026-06-12 · DO NOT EDIT — edit CLAUDE.md then run /co-agent sync-context -->
> You are Gemini, an external reviewer — project context below.

# inferplane — reviewer context

LLM consumption-governance gateway: virtual keys, team RBAC, quotas/budgets,
tamper-evident audit for Claude Code / OpenCode traffic → Anthropic / Bedrock /
OpenAI-compatible upstreams. Go 1.25, single static binary (`CGO_ENABLED=0`,
every dependency pure-Go), Kubernetes-native, Apache-2.0, CNCF Sandbox aspirant.

## Build · test · lint

```bash
CGO_ENABLED=0 go build -trimpath -o bin/inferplane ./cmd/inferplane
go test ./... -race
go vet ./... && gofmt -l .
bash tests/run-all.sh   # harness tests (bash, not Go)
```

All four must pass on every change. Tests must run without networks,
credentials, or a real IdP (httptest fakes only).

## Architectural boundaries

- `providers/<name>/` is the extension surface: a new provider = one package +
  one blank import in `cmd/inferplane/main.go`. **A provider PR that touches
  `internal/*` has violated the boundary.**
- `internal/principal`, `internal/metrics`, `internal/governance`,
  `internal/adminauth` are import-cycle-free leaves — they must not grow
  dependencies on `internal/server` or `internal/config`.
- Config-coupled packages get decoupled mirror types (e.g.
  `governance.ConfigTeam`, `adminauth.MappingConfig`) — never import
  `internal/config` from a leaf.
- `cmd/inferplane` stays a thin assembly diagram; logic lives in `internal/*`.

## Banned patterns / security mandates (violations are CRITICAL)

- Inline secrets in config — secrets only via `env:`/`file:` refs; config load
  must reject inline `api_key`.
- Logging/exposing virtual-key plaintext (`ik_...`); keys are SHA-256 at rest,
  plaintext shown exactly once.
- Forwarding the client's key upstream, or exposing the gateway's upstream
  credential to clients.
- Secrets or `key_id` values in `/metrics` (unauthenticated by design; labels
  must be config-bounded — never raw client input).
- `count_tokens` returning non-200 (it crashes Claude Code).
- Float cost arithmetic — cost is integer microUSD, round-half-even.
- Mutating a request body when ingress protocol == provider protocol (cache
  invariant: verbatim `RawBody` passthrough preserves prompt-cache hits).
- Admin-plane: JWT-shaped static tokens with OIDC enabled; non-https OIDC
  issuer; email or raw IdP groups in audit records or request context
  (opaque `sub` only); auditing 401s (only authenticated 403s are audited).
- Skipping DCO sign-off on commits.

## Review expectations

- TDD: tests land with (or before) the change; adversarial cases for any
  auth/governance path (alg confusion, fallthrough, fail-open on error paths).
- Two-phase governance: PreCheck BEFORE billing, Settle AFTER; `on_exceeded`
  block wins ties.
- Errors wrapped with `%w`; ingress errors returned in the ingress protocol's
  own error shape.
- Audit chain: records are hashed as exact line bytes — new fields are
  append-only with `omitempty`, proven by a mixed-version fixture test.
- Fail closed: missing identity/lookup errors deny, never default-allow.

## Review checklist

1. Does the diff cross the provider/core or leaf-package boundaries?
2. Any banned pattern above? Any secret in code, config, or test fixtures?
3. Auth paths: total routing, no fallthrough, 401 vs 403 correct, fail-closed
   error paths, constant-time comparisons preserved?
4. Tests: can each new assertion actually fail? Races under `-race`?
5. Docs: ADR for decisions, reference docs synced for schema/endpoint changes.

Known false-positives to suppress: `/metrics` being unauthenticated is by
design (§5.5); the admin console's static assets being unauthenticated is by
design (ADR-001/002 — they are data-free); key-existence signal on revoke 403
is an accepted, documented trade-off.
