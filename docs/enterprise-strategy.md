# Enterprise product strategy

Status: canonical product direction · Last reviewed: 2026-08-28 · Release posture: **alpha**

Owns: target market, enterprise contracts, release gates.
Does not own: implementation status ([roadmap.md](roadmap.md)), current system
description ([architecture.md](architecture.md)), implementation decisions
([decisions/](decisions/)), request flow (`CLAUDE.md` → Request Flow).
Where this document and an ADR disagree, the ADR governs until superseded.

Inferplane remains alpha and must not be described as enterprise
production-ready until every P0 below is closed and the acceptance suite passes.

## 1. Scope

The governance plane for enterprise coding-agent traffic (Claude Code, Codex,
OpenCode) — not a general-purpose LLM proxy. First production topology: a fleet
of developer-local `mayu` data planes controlled by `inferplaned`, with the
control plane off the inference path.

**First production claim covers developer-local `mayu` fleets only.** A shared,
horizontally scaled Kubernetes data plane is a later profile.

**Non-goals for the first release:** generic API gateway, provider/modality
count, semantic response caching, multi-replica shared-gateway HA, custom
authorization languages, MCP traffic routing.

## 2. Enterprise contracts

| Contract | Requirement | Status |
|---|---|---|
| Durable identity | `UserID = (OIDC issuer, subject)`. Key rotation, re-login, restart, and a second device must not split policy, budget, quota, or audit attribution. Email/owner strings/key IDs are not identities. | ❌ P0 |
| Duty separation | Fixed roles (`platform-admin`, `policy-admin`, `provider-admin`, `budget-admin`, `auditor`, `team-admin`) with org/team scope. Every control-plane endpoint authorizes after authenticating. Every policy/provider/pricing/budget/role mutation records actor, capability, scope, before/after hash, generation. | ❌ P0 |
| Two-pool user budget | Premium pool + total hard cap in one explicit window. Premium exhausted → first compatible model in an admin-approved fallback set; total exhausted → deny before egress. Token quotas must state fallback-or-block explicitly, never inherit monetary behavior. | ❌ P0 |
| Pre-egress PII policy | Typed detector result; the policy engine (not the plugin) picks `external-unmodified` \| `external-masked` \| `internal-only` \| `blocked` and attaches it as an **egress ceiling**. Later stages may only narrow it. Detector/masker failure is fail-closed. `external-unmodified` requires a completed detector chain reporting nothing protected. | ❌ P0 |
| Fleet enforcement accuracy | Enforcement key ≥ `(org, UserID, pool, windowID)` in a durable ledger. A lease is spend authority already reserved centrally — non-overlapping, immediately reducing central balance, expiry returning only provably-uncommitted authority. Rate/quota must not multiply by data-plane count. | ❌ P0 |
| Guardrail / residency | A configured guardrail and region lock apply on **every** egress path, with no opt-out reachable from routing config. | ❌ P0 (regressed) |
| Cost explainability | Every served request settles observed usage against an immutable pricing version; every request mutation the gateway performs is recorded. Cache reads, 5m/1h writes, hit ratio, write-without-reuse, and masking/model-switch cache loss are reported. | 🔶 partial |

A hard cap governs admission against a versioned pricing table; it is not a
promise that a later invoice matches the internal ledger. Reconciliation
reports the difference. A target whose conservative upper bound cannot be
computed must be blocked, not admitted.

## 3. Current state

### Sound and worth preserving

Node-local data plane with the control plane off the request path (ADR-031) ·
virtual-key isolation and per-key/model allow-lists · team and user-subject
model access with post-routing RBAC re-check · region locking · budget-tier
substitution, fallback chains, circuit breakers (ADR-041) · integer µUSD
pricing, round-half-even · separate cache-read/5m/1h accounting, including
interrupted streams · OIDC login, short-lived virtual keys, STS credential
brokering (ADR-028/040) · hash-chained audit, optional encrypted body capture,
S3 anchoring (ADR-012/018) · optional Postgres usage analytics (ADR-036).

### P0 — blocks the enterprise-ready claim

**Guardrails on the Mantle egress path: refused, not applied.**
Original bug: `guardrailFor` was called on the Converse and InvokeModel paths
only; the Mantle paths (`providers/bedrock/mantle.go` `Complete`/`Stream`)
never called it, while `internal/server/bedrockapi/invoke.go` writes
`pr.GuardrailID` into the tamper-evident record unconditionally — so a
`model_api: {"<model>": "mantle"}` entry silently disabled a mandated
guardrail **and the audit chain attested that it was applied.** Fixed:
`mantleGuardrailCheck` (`providers/bedrock/bedrock.go`) now refuses any
guarded request routed to Mantle with a 400 naming the conflict, before
egress — the bypass and the falsified attestation are gone. Remaining gap
(why the contract row stays ❌): Mantle has no guardrail parameter, so the
requirement "a configured guardrail *applies* on every egress path" is still
unmet — a guarded team simply cannot use Mantle-only models until guardrail
evaluation exists off the InvokeModel/Converse APIs (or the refusal is
accepted as the permanent posture and documented as such).

**A 200 response could bill zero on the Mantle path.**
Original bug: the Bedrock ingress settles only when `resp.Parsed != nil`, and
Mantle's `Complete` dropped `Parsed` on any unmarshal/conversion failure — a
malformed-but-200 upstream response was served with no debit, no cost, and
empty audit usage (the ADR-030 zero-cost class re-entering through a new
path; Converse and InvokeModel build Parsed from typed fields and cannot
reach this state). Fixed: that path is now fail-closed — an unparseable 2xx
returns a synthesized 502 (`providers/bedrock/mantle.go` `Complete`), and the
stream path errors when a 200 stream yields no parseable frame, so nothing is
served unbilled. Remaining gap: fail-closed conversion is a per-path
discipline, not a structural guarantee — a future egress that builds `Parsed`
from re-parsed JSON must repeat it (no test fences the invariant generically).

**Per-user budget shipped; per-user rate and durable identity are still absent.**
`internal/policy/store.go` `checkEnforceable` now rejects a user-subject
*rate* rule only (needs the rate-share model — `docs/roadmap.md` item ①);
user-subject *budget* is enforced (ADR-042 Phase 3 —
`internal/governance/governance.go` third PreCheck/Settle scope). Per-user
budget still has no control-plane lease (`docs/roadmap.md` §Accepted
limitation), so N data planes admit up to N× a user's configured cap. Usage
attribution remains the free-form key owner / bare OIDC `sub` — no
`(issuer, subject)` pairing exists yet — so a stable per-person budget still
does not survive a second IdP or a colliding `sub` across issuers.

**Substitution is team pressure, not a user fallback contract.** ADR-041
activates a per-team substitution map from a referenced team budget;
`router.SubstituteTier` leaves the premium model unchanged when the target is
not allowed. Premium-pool exhaustion → user-specific fallback → total hard cap
does not exist.

**Management authorization is coarse.** The control plane grants whole-console
authority to any accepted OIDC identity or static token, and policy
PUT/DELETE (`internal/controlplane/policies.go:170-174`) sits behind that same
layer with no mutation audit. Provider/model writes on the `mayu` admin plane
have no dedicated capability gate.

**Enforcement state is neither durable nor globally accurate.** Key store is
SQLite; rate/quota/budget counters are process-local; the lease ledger is
in-memory with approximate window rollover and prunes dead data-plane spend
(`internal/controlplane/controlplane.go:39`). Standalone and per-key budgets
get no lease. Helm pins `replicaCount: 1`.

**PII handling is masking-only.** The `filter` seam and `piimask` plugin
transform and fail closed, but produce no typed detection result, no
policy-selected action, no destination classification, and no egress ceiling.

### P1 — operational competitiveness

- **Undisclosed request mutation.** `providers/bedrock/converse.go:443-467`
  and `providers/bedrock/mantle.go:112-117` are two separate model→param strip
  tables in different wire vocabularies, both keyed by
  `strings.Contains(upstream, …)`, both sourced from a one-off manual probe
  (2026-08-28) with no stored artifact and no CI guard. A dropped
  `temperature: 0` changes sampling semantics with no audit field, metric, or
  response header. Pricing has `mayu pricing check`; this has no equivalent.
- **Cache behavior differs by path.** Anthropic passthrough and Bedrock Claude
  InvokeModel preserve `cache_control`; Bedrock Converse does not map it to
  `cachePoint`. `internal/cache.VolatileStore` and cache-affinity are
  unimplemented.
- **Cache efficiency is measured as tokens, not outcomes** — no hit ratio,
  write-without-reuse, prefix fragmentation, or model-switch loss attribution.
- **Request audit lacks durable identity** — records carry key ID and team;
  usage telemetry carries the key owner.
- **Mantle errors miss model-level fallback.** `isModelNotFound`
  (`internal/server/bedrockapi/invoke.go:277-282`) matches on
  `"ValidationException"`, which Mantle's OpenAI-shaped error bodies need not
  contain.
- **Alpha deployment posture** — Helm defaults persistence off, no resource
  requests, no security context, PDB, NetworkPolicy, or autoscaling; no CI
  workflow running the full race/vet/vulnerability/release gates.

## 4. Delivery sequence

Ordered by trust boundary. Each phase is a separate design spec and plan.

| Phase | Work | Exit gate |
|---|---|---|
| **0a. Invariant guards** | Refuse to boot (or record honestly) when a configured guardrail or region lock is unreachable on a selected egress path; make a settled cost mandatory for every 2xx; CI guard for strip-table and guardrail-path coverage, in the `mayu pricing check` mold | No egress path can silently drop a mandated control or a billable settlement; a new path fails CI rather than review |
| **0b. Identity & management trust** | First-class `(issuer, sub)` and typed service accounts; credentials reference identity; fixed roles with org/team scope; capability check on every management endpoint; mutation audit for policy/provider/pricing/budget/role | Key rotation and a second device retain the same policy and audit identity; cross-role negative authorization tests pass |
| **1. User budget state machine** | Durable window IDs and enforcement ledger; premium + total pools; atomic reserve/settle/release; approved fallback sets with compatibility checks; explicit quota fallback-or-block; non-overlapping fleet leases | Concurrent requests, restart, key rotation, and two data planes cannot resurrect spend or bypass the total cap |
| **2. Pre-egress PII policy** | Typed detector/transform contract; central action selection; destination classification; fail-closed detector and masker; PII-free OTel plus correlated audit | Detector timeout, transform failure, and every action prove unmasked protected data never reaches an external target |
| **3. Cache & cost efficiency** | Converse cache-point translation; hit/write/waste and fragmentation metrics; model-switch and masking loss attribution; live per-path write-then-read tests; Bedrock billing reconciliation | Operators explain cache-cost changes from telemetry without reading prompt bodies |
| **4. Fleet ops & release hardening** | Fleet-wide rate/quota accuracy; data-plane version visibility, rollout, diagnostics; production packaging and network controls; race/vulnerability/signing/release CI; SLOs and runbooks | The scenario suite and supply-chain gates pass from a clean checkout |

## 5. Acceptance suite

Unit tests of isolated functions are not sufficient evidence. Every scenario
must show agreement among the actual target model/provider, enforcement
ledger, usage ledger, OTel metadata, and audit record.

**Identity** — two users in one team keep independent balances · one user on
two devices is one ledger · CLI key re-mint resets no budget, quota, or audit
identity.

**Budget** — concurrent near-cap requests cannot bypass the cap · an upper
bound exceeding balance denies before egress · premium exhaustion selects an
approved compatible target · no compatible fallback blocks (never serves the
premium model) · total exhaustion issues no provider request · a fallback after
a failed attempt takes a separate reservation · uncertain usage after
cancellation stays unavailable until reconciled · control-plane interruption
and window rollover resurrect no spend.

**PII** — maskable data is irreversibly transformed and evidenced as metadata
only · internal-handling data reaches only approved internal targets · an
`internal-only` request under budget or provider fallback never goes external ·
detector or masker failure makes no unmasked external call.

**Egress controls** — every path applies a configured guardrail and region
lock, or refuses the request; no routing-config value is an opt-out · every
request mutation the gateway performs appears in the audit record.

**Cost** — a stable same-model prefix produces a cache write then a measured
read · masking and model switching attribute the expected cache loss · every
2xx carries a settled cost.

**Administration** — a forbidden admin action is denied and evidenced · every
policy/provider/budget mutation records actor, scope, diff hash, and generation.
