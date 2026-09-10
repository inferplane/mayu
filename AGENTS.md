<!-- generated-by: co-agent · source: CLAUDE.md · claude-md-sha: 6dabbf7d502d · generated-at: 2026-09-10 · DO NOT EDIT — edit CLAUDE.md then run /co-agent sync-context -->
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
model name, judged globally by the control plane, never widening access
or denying, (4) team/per-user budget control with visibility, (5) no SPOF.
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
  cache-affinity is enforced anywhere yet. This is unrelated to `internal/tier`
  (ADR-041 budget-tier substitution), which is implemented and does not
  depend on `internal/cache`.
- A budget-tier substitution TARGET must pass RBAC and be routed on the
  enforcing data plane, or the ORIGINAL model is served — substitution must
  never widen access and must never itself deny a request.

## Policy-aware routing (ADR-043)

- `sensitivity` is a stdlib-only leaf inspecting original bytes without mutation
  or retained text. Finite email/phone/Luhn-card/SSN/IPv4/Korean-ID signals include
  exact numeric spellings; opaque/unknown content is incomplete, errors explicit.
  No universal PII guarantee, remote classifier, session pinning, or Responses
  ingress is implied. Boundary labels are operator assertions.
- `Store.MatchingRoutingPolicies` supplies one snapshot and a rejection gate.
  SensitiveData requires FailClosed: Block wins, internal-model sets intersect,
  each attempt needs an approved model AND an explicitly internal provider.
  Rejected distributed sensitive generations deny until valid recovery.
- Context requires FailOpen, defaults to Shadow, and cannot relax privacy.
  Privacy always enforces, even with Shadow. Enforce switches only completely
  inspectable single-user-turn requests without history/tools/media/reasoning/
  structured output. Alternatives need declared context/capabilities, pricing,
  RBAC/regions, and physical transport compatibility; metadata cannot override
  translator losses. No-rule/passive paths preserve existing authorized behavior.
- Requested is resolved pre-tier; context sources match post-tier before privacy;
  selected and proposed differ from actual attempts. Passive results preserve the
  input preflight Model. Attempts/context/pricing use the same live.State.
- `cmd/mayu` installs `live.Holder.RoutedAndPriced` after usable topology and
  reloads local policy before listeners; future Reload/ApplyWire validate too.
  Runtime candidate checks remain required after topology changes. Metadata must
  survive DB seed/overlay, admin read/write/export, and reload.
- Both count APIs remain local/200 on routing refusal and unready/stale gates.
  CP privacy from first request needs require_sync. Upgrade binaries/CRD before
  new-rule activation. Evaluate task success, total cost including cold-cache/
  retries, p95 latency, and privacy negative cases before Enforce; tests establish
  neither measured savings nor production readiness.
- Routing audit fields are append-only/omitempty, with planned/actual provider
  and boundary. Counter labels only team/mode/reason; no prompt, detection value,
  session ID, user, or key ID. Existing optional body logging is independent.

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
- Data-plane multi-replica HA has a maintainer-stated direction, not yet an
  ADR (Postgres-only shared state, narrower than ADR-013's original
  Postgres+Redis design) — implementation is still deferred, ADR-013 itself
  is not marked superseded,
  and no implementation ADR exists yet. Rate-limit counters, budget/quota
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
