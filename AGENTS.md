<!-- generated-by: co-agent · source: CLAUDE.md · claude-md-sha: 2560a9843d3a · generated-at: 2026-09-11 · DO NOT EDIT — edit CLAUDE.md then run /co-agent sync-context -->
> You are an external reviewer for this repo — project context below, distilled
> from CLAUDE.md. This file is shared verbatim by Kiro, Codex, and Agy (not a
> per-AI copy).

# inferplane — reviewer context

Governance control plane + node-local data plane for LLM consumption: virtual
keys, team RBAC, quotas/budgets, tamper-evident audit for Claude Code /
OpenCode / Codex traffic → Anthropic / Bedrock / OpenAI-compatible upstreams.
Go 1.25; each of the two binaries below is itself a single static binary
(`CGO_ENABLED=0`, every dependency pure-Go),
Kubernetes-native, Apache-2.0, CNCF Sandbox aspirant.

**Core purpose (judge diffs against this):** (1) a single entry point for
coding-assistant traffic, (2) per-user model choice, (3) cost-driven model
substitution — enforceable via `routing.budgetTiers` (ADR-041): a budget
rule crossing a threshold substitutes a cheaper target for a requested
model name. Legacy substitutions never widen access or deny; opt-in ADR-044
strict targets constrain every attempt and may refuse. (4) team/per-user budget control with visibility, (5) no SPOF.
Known tension: (5)'s no-SPOF pulls against making (4)'s enforcement accurate
under multi-replica — see the HA note below.

Two binaries: **`cmd/mayu`** is the node-local data plane (the full gateway —
routing, auth, governance, audit; runs standalone, no control plane required).
**`cmd/inferplaned`** is the control plane (ADR-034) — distributes
`GovernancePolicy` documents and issues budget leases over one heartbeat; it
never carries inference traffic. Credential brokering is live (ADR-040):
with INFERPLANED_BROKER_ROLE_ARN set, inferplaned vends <=1h STS Bedrock
sessions over POST /v1alpha1/credentials behind a DEDICATED broker token
(never the heartbeat token, no OIDC branch); mayu opts in per provider with
auth.mode "broker" — which must never fall back to the default credential
chain.

## Build · test · lint

```bash
CGO_ENABLED=0 go build -trimpath -o bin/mayu ./cmd/mayu
CGO_ENABLED=0 go build -trimpath -o bin/inferplaned ./cmd/inferplaned
go test ./... -race
go vet ./... && gofmt -l .
bash tests/run-all.sh   # harness tests (bash, not Go)
```

All four must pass on every change. Tests must run without networks,
credentials, or a real IdP (httptest fakes only).

## Architectural boundaries

- `providers/<name>/` is the extension surface: a new provider = one package +
  one blank import in `cmd/mayu/main.go`. **A provider PR that touches
  `internal/*` has violated the boundary.**
- `internal/principal`, `internal/metrics`, `internal/governance`,
  `internal/adminauth` are import-cycle-free leaves — they must not grow
  dependencies on `internal/server` or `internal/config`.
- Config-coupled packages get decoupled mirror types (e.g.
  `governance.ConfigTeam`, `adminauth.MappingConfig`) — never import
  `internal/config` from a leaf.
- `cmd/mayu` and `cmd/inferplaned` stay thin assembly diagrams; logic lives in
  `internal/*`. `internal/policy` is the rule/lease schema shared by both
  binaries — the single source of truth for policy semantics.
- A data plane's policy source is `policies` (local file, watched) XOR
  `control_plane` (ADR-034 heartbeat) — config load must reject both set at
  once.
- `internal/cache` (VolatileStore, for cache-affinity routing) is an
  unimplemented interface with no importers today — don't assume
  the legacy affinity subtype is enforced. ADR-044 context stability has its own
  bounded local successful-target store; it is not financial authority. This is unrelated to `internal/tier`
  (ADR-041 budget-tier substitution), which is implemented and does not
  depend on `internal/cache`.
- A legacy budget-tier substitution TARGET must pass RBAC and be routed or
  the ORIGINAL is served. Opt-in enforceTargets instead restricts every
  choice/retry to its target; unavailable/conflicting targets deny. Its soft
  budget reference is an accounting threshold, not an admission lease or a
  smaller hard cap. Independent hard caps always remain binding.

## Adaptive routing (ADR-043/044)

- Local original-byte inspection returns finite email/phone/card/SSN/IPv4/Korean-ID
  signals and coverage. Unknown/opaque input is not clean. No universal PII claim.
- Privacy requires FailClosed: Block wins, internal-model sets intersect, Mask
  remains required alongside InternalOnly. Complete and independently reinspect
  redaction before returning a usable chain; ingresses consume SanitizedBody and
  reparse. Unsafe structural/numeric changes refuse. Remote tools cannot bypass
  an internal boundary by selecting an internal model.
- Legacy context stays single-turn/two-class. Optional normalModel/stability
  enables compatible tool/history sessions. Extended keywords use latest user
  intent; full input size still constrains capacity. All context remains Shadow
  by default and cannot loosen privacy or strict budget targets.
- Affinity pins an actual successful model/provider/upstream, with bounded TTL
  and identity-scoped HMAC hints. Revalidate every hit; no raw prompt/session ID
  in storage, observability or authorization. No network dependency; restart or
  capacity loss can cause a cold cache, never permission expansion.
- Responses ingress uses the same identity/readiness/routing/governance gates.
  Native transport preserves bytes; stateless adapters reject unsupported opaque
  or provider-owned state. CLI/fake-provider tests do not establish model quality.
- Requested is resolved pre-tier; selected/proposed differ from actual attempts.
  All attempts and pricing use one topology snapshot. Policy/metadata changes
  still require runtime revalidation and an upgraded producer/consumer/CRD set.
- Rejected privacy or strict-budget distribution gates affected subjects until
  valid recovery. Count APIs stay local/200 on refusal or unready/stale gates.
- Audit additions are append-only/omitempty; metric dimensions remain bounded.
  No prompt, detected value, session ID, user or key ID enters new metric labels.
- Evaluate task success, total settled cost including cold-cache/retries and
  latency before enabling Enforce. Model names do not attest privacy or price.

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
- Mutating same-protocol content outside documented model substitution or
  explicitly required, completely verified PII masking. Default RawBody
  passthrough preserves prompt-cache bytes.
- Admin-plane: JWT-shaped static tokens with OIDC enabled; non-https OIDC
  issuer; email or raw IdP groups in audit records or request context
  (opaque `sub` only); auditing 401s (only authenticated 403s are audited).
- A `GovernancePolicy` rule with `hardCap: true` and a non-`FailClosed`
  `failurePolicy` (hard caps must fail closed).
- Skipping DCO sign-off on commits.

## Review expectations

- TDD: tests land with (or before) the change; adversarial cases for any
  auth/governance path (alg confusion, fallthrough, fail-open on error paths).
- Two-phase governance: PreCheck BEFORE billing, Settle AFTER; `on_exceeded`
  block wins ties — including across layered sources (config team vs. policy
  overlay vs. control-plane lease clamp: most-restrictive wins, never a soft
  layer loosening a blocking one).
- Errors wrapped with `%w`; ingress errors returned in the ingress protocol's
  own error shape.
- Audit chain: records are hashed as exact line bytes — new fields are
  append-only with `omitempty`, proven by a mixed-version fixture test.
- Fail closed: missing identity/lookup errors deny, never default-allow.
- ADR-044 records node-local failure boundaries and the Postgres-only shared
  authority direction, narrower than ADR-013's original Postgres+Redis design.
  Durable reserve/settle/window implementation remains deferred; ADR-013 is
  not marked superseded and routing changes do not implement shared-state HA. Rate-limit counters, budget/quota
  stores, and the circuit breaker are all instance-local today. **Suppress**
  "the in-memory limiter/budget won't scale past one replica" as a new
  architecture finding — that's the known, tracked gap. **Do flag** any doc,
  chart value, config comment, or code comment that implies multi-replica/HA
  works *today* — that's a docs-accuracy bug, not the known gap.

## Review checklist

1. Does the diff cross the provider/core or leaf-package boundaries?
2. Any banned pattern above? Any secret in code, config, or test fixtures?
3. Auth paths: total routing, no fallthrough, 401 vs 403 correct, fail-closed
   error paths, constant-time comparisons preserved?
4. Policy/governance layering: does a narrower/softer source ever loosen a
   stricter one it's layered under (block→warn, hard→soft)?
5. Tests: can each new assertion actually fail? Races under `-race`?
6. Docs: ADR for decisions, reference docs synced for schema/endpoint changes.

Known false-positives to suppress: `/metrics` being unauthenticated is by
design (ADR-005); the admin console's static assets being unauthenticated is
by design (ADR-001/002 — they are data-free); key-existence signal on revoke
403 is an accepted, documented trade-off; single-replica-only enforcement
(see ADR-013 note above) is known, not a new finding.
