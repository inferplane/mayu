# P0 enterprise readiness: implementation sequence and acceptance gates

> For agentic workers: this is the coordinating plan for six P0 workstreams.
> Use the approved phase design and a task-specific implementation plan before
> changing runtime code. The task numbers below preserve the original review
> references; they are not thirteen independent, immediately executable patches.

**Reviewed:** 2026-09-11, against `main` at `08d7869` (including merged PR #71).

**Goal:** Preserve coding-task usefulness while controlling total cost inside
approved data-processing boundaries. Enterprise readiness requires durable
identity, scoped management authority, accurate shared enforcement, and
verifiable handling of protected data.

**Scope of PR #70:** documentation and the rooted `/review/` ignore rule only.
This plan neither implements the workstreams nor supersedes an ADR. In particular,
ADR-013 is still a proposed design; the maintainer's later Postgres-only shared-state
direction is not an implemented or superseding ADR. Record that decision before
implementing shared enforcement. Do not introduce Redis as an assumed dependency.

**Design sources:** [enterprise strategy](../../enterprise-strategy.md),
[roadmap](../../roadmap.md), and the applicable [decision records](../../decisions/).
The [phase-0 identity/management plan](2026-08-28-phase-0-identity-management-trust.md)
is the existing detailed input to Tasks 3, 4, 7 and 8. Reconcile it with an approved
phase design and current code rather than creating a competing identity model.

**Architecture:** retain the node-local data plane and keep the control plane off
the inference path. Establish identity and management trust first; then durable
spend authority and window identity; only then activate fleet-wide user pools and
rate shares. PII work can proceed against its own approved boundary contract.
Dependency completion determines release order, not the previous twelve-week
calendar estimate.

**Tech stack:** existing Go 1.25, `net/http`, `go-oidc`, pure-Go SQLite and `pgx`
dependencies. SQLite remains suitable for local stores and the persistence probe
below. The proposed production shared ledger is Postgres-authoritative, subject to
its implementation ADR; a SQLite restart test does not demonstrate multi-replica
coordination.

## Current state and remaining work

Status here is implementation evidence, not completion of an entire P0 contract.

| Tasks / P0 | Evidence at reviewed main | Remaining acceptance |
|---|---|---|
| 1 / guardrails | `providers/bedrock/bedrock.go` calls `mantleGuardrailCheck` for Complete and Stream; it considers the effective request/provider guardrail | Prove every new egress applies the required control or refuses before sending; guarded Mantle remains refused |
| 2 / billing | `providers/bedrock/mantle.go` refuses unparseable or usage-less successful responses; cache-aware settlement already exists | Preserve observed-usage accounting, partial-stream behavior and immutable pricing across every path |
| 3–6 / user governance | ADR-042 user-budget rules and `Governor.SetUserLookup` are implemented | Durable issuer/subject identity, user-rate authority, premium/total pools and fleet-wide admission |
| 7–8 / management trust | Dedicated policy-write token, mutation logging and admin-only provider/model writes exist after #71 | Organization/team capability matrix, durable authorization bindings and complete mutation evidence |
| 9–11 / fleet enforcement | Team budget leases exist; instance-local and nondurable enforcement limits remain documented | Durable reservations/settlement, authoritative window IDs, safe rate shares and fleet-wide token quotas |
| 12–13 / PII | Main still has the legacy opt-in masking seam | Integrate or independently implement the approved detection/egress contract, then close the broader masking contract |

There is also a separately reviewed **local, unmerged** implementation:
`feat/policy-aware-routing` at `d056a10`. Its ADR-043 and
`docs/superpowers/plans/2026-09-09-policy-aware-routing.md` describe finite local
inspection, InternalOnly/Block routing, context Shadow/Enforce, count-path protection,
metadata persistence and actual-target audit evidence. Those files are not in
this PR or reviewed main. Inspect and merge/adapt that work through its own PR
before claiming it is delivered here. It does not complete policy-selected
external masking, durable identity, user pools, fleet enforcement or Responses
support.

## Global constraints

- Both binaries build with `CGO_ENABLED=0`; keep dependencies pure Go.
- A provider extension adds its package and the assembly import. Keep shared
  policy/enforcement changes in an explicitly scoped core change.
- `principal`, `metrics`, `governance` and `adminauth` must not depend on
  `server` or `config`. Store keys and schemas belong to a shared leaf; do not
  make the budget store import its governor to obtain key types.
- Use the real `keystore.Principal`, `providers.ProxyRequest`/`ProxyResponse`,
  `schema.Usage`, and Go `net/http` routes. Do not replace them with the obsolete
  `config.Provider`, `bedrock.Request`, `LoginCommand` or Gorilla-mux examples
  from the earlier draft.
- Secrets are env/file references; virtual-key plaintext is shown only at issuance.
  Never forward client credentials or expose upstream credentials.
- Metrics labels are configuration-bounded, with no secret, key ID or raw user
  input. Audit/context identity is opaque, never email or raw IdP groups.
- Authenticate before authorizing. Preserve constant-time token checks, total
  JWT/static-token routing, 401 versus authenticated 403, and no auditing of 401s.
- Broker credentials retain their dedicated token and must never fall back to the
  node's default credential chain.
- PreCheck occurs before billable work; Settle records actual observed usage after
  it. Cost is integer microUSD with round-half-even. Missing usage is not free;
  legitimate rounded-zero or explicitly free usage is not a billing bug.
- Every applicable policy layer can only narrow authority. Block wins; hard caps
  require FailClosed. Budget-tier substitution retains ADR-041's separate
  never-widen/never-deny contract.
- Same-protocol forwarding preserves RawBody. A security transformation must be
  explicitly enabled and evidenced; inspection alone never mutates the payload.
- Count-token handlers return 200 with a local estimate when a policy, detector,
  destination or readiness gate prevents upstream access.
- Audit additions are append-only with `omitempty`, proven against mixed-version
  exact-byte fixtures. Record hashes of changes, not raw secret-bearing states.
- Initialization, DDL, query, scan, transaction and required audit errors must be
  handled. Constructors return errors; schema failure is a startup failure, not
  a silently usable store.
- All commits, including the task examples below, carry DCO sign-off.
- No phase can claim HA, universal PII detection, successful task quality or cost
  savings from unit tests alone.

## Dependency and activation order

| Stage | Tasks | Entry/exit gate |
|---|---|---|
| A: baseline and designs | 1–2, phase design reconciliation | Confirm existing fixes; identify exact remaining behavior before adding code |
| B: identity and management trust | 3–4 and 7–8 | Verified identities, scoped authorization and accountable mutations |
| C: durable authority | 9–10 | Shared atomic reserve/settle/release plus durable window IDs |
| D: fleet user admission | 5–6, then 11 | B and C complete; concurrent/restart/multi-device budget, rate and quota tests pass |
| E: protected-data handling | 12–13 | Approved detection/transform/egress design, dependency review and all ingress tests |

Existing local user-budget behavior remains available with its documented limits.
Do not disable that shipped feature merely to reorder this plan. **New fleet-wide
claims and user-rate support remain gated** until identity, shared authority and
all participating data-plane consumers are ready. Define the activation/version
contract in the phase ADR; do not invent an accepted-but-unenforced config flag.
`control_plane.require_sync` is a readiness control, not proof of global accuracy.

## Task 1: Preserve the guardrail refusal and audit invariant (P0-01)

**Status:** baseline fix present; verify and extend coverage where evidence is missing.

**Files to inspect:** `providers/bedrock/{bedrock,mantle,converse}.go`,
`providers/bedrock/{guardrail,mantle}_test.go`,
`internal/server/bedrockapi/invoke.go`, `internal/audit/record.go`.

- [ ] Exercise both provider defaults and per-request/team guardrails across
  Complete/Stream and every selectable Bedrock API path.
- [ ] Keep the existing effective-guardrail lookup. Checking only a provider's
  default ID would miss a team/request override.
- [ ] A path unable to apply the required guardrail refuses before egress. Do not
  infer successful application merely from configuration.
- [ ] If new application evidence is needed, append an optional audit field and
  test old/new records together. An absent historical field must not become a
  false assertion that a guardrail ran.
- [ ] Add a failing regression only for a demonstrated remaining gap; do not add
  duplicate Complete/Stream methods or recreate the shipped refusal.

Verify: `go test ./providers/bedrock ./internal/server/bedrockapi ./internal/audit -race`.

Commit any verified change: `git commit -s -m "fix: preserve effective guardrails on every egress path"`.

## Task 2: Preserve settlement truth (P0-02)

**Status:** core refusal present; the cross-path accounting contract still needs evidence.

**Files to inspect:** `providers/bedrock/mantle.go`,
`providers/openaicompat/openaicompat.go`, `pkg/schema/usage.go`,
`internal/server/{anthropicapi,openaiapi,bedrockapi}`, `internal/pricing`.

- [ ] Retain the current refusal of an unparseable or usage-less successful
  response. Do not return success with invented usage from PreCheck.
- [ ] Distinguish missing usage from a real zero count. Preserve cache reads,
  total writes, 5m/1h writes and observed partial-stream usage.
- [ ] Keep reservation estimates separate from observed settled usage. Uncertain
  outcomes retain reserved authority for reconciliation; they are neither a
  fabricated settled bill nor an automatic full refund.
- [ ] Resolve pricing by configuration provider name and upstream model ID from
  the same immutable generation used for the attempt.
- [ ] Cover malformed JSON, parseable missing usage, zero noncached input with
  cache usage, interrupted streams and model/provider fallback. Do not flag
  every zero-microUSD result as a defect.

Verify: `go test ./providers/... ./internal/server/... ./internal/pricing -race`.

Commit: `git commit -s -m "fix: keep observed usage and pricing consistent across egress paths"`.

## Task 3: Durable identity model and migration (P0-03)

**Dependencies:** approved phase-0 design; use the existing phase-0 plan as input.

**Existing seams:** `internal/keystore/{keystore,sqlite}.go`,
`internal/principal/principal.go`, `internal/adminauth/oidc.go`.
The proposed identity leaf and exact schema belong to the phase-specific plan.

- [ ] Define a typed human identity from verified issuer and opaque subject,
  scoped to an organization; define service identities separately.
- [ ] Choose a validated, unambiguous serialization. A raw delimiter join or
  free-form owner string is not a migration strategy.
- [ ] Version the SQLite migration, serialize concurrent initialization and
  propagate every DDL error. Keep existing key hashes and issuance semantics.
- [ ] Do not derive identities from email, client headers, or unverified claims.
  Bind legacy human keys through an explicit trusted migration/reissuance path;
  unresolved identity cannot satisfy a per-user governed operation.
- [ ] Test same person/key rotation/second device, different issuers with equal
  subjects, human/service separation, missing identity, rollback and concurrent
  migration. Keep team-scoped legacy behavior explicitly classified.

Verify the affected identity, keystore, principal and auth packages with `-race`.

Commit: `git commit -s -m "feat: persist typed durable identities on credentials"`.

## Task 4: Bind identity at verified server-side minting (P0-03)

**Dependency:** Task 3's validated identity/storage contract.

**Existing seams:** `internal/adminauth`, `internal/server/authapi`,
`internal/server/adminapi`, `cmd/mayu/{login,gateway}.go`,
`cmd/inferplaned/oidcenv.go`.

- [ ] Extract issuer/subject only after issuer, audience, signature, expiry and
  the relevant login flow have been verified.
- [ ] The server binds identity when minting credentials. The CLI is not trusted
  to mint authority from a decoded token or a local `owner`/`email` value.
- [ ] Keep console and CLI audiences distinct and retain the broker's dedicated
  authentication path.
- [ ] Test fake-IdP key issuance, re-login and rotation end to end, including
  wrong audience/issuer/algorithm, missing subject and service impersonation.
- [ ] Confirm request context, usage grouping and audit refer to the same durable
  identity without exposing email or groups.

Verify: `go test ./internal/adminauth ./internal/server/authapi ./cmd/mayu ./cmd/inferplaned -race`.

Commit: `git commit -s -m "feat: bind verified identities during credential issuance"`.

## Task 5: Governed subject/pool/window keys (P0-03)

**Dependencies for activation:** Tasks 3–4, 7–10; deployed consumers must understand
the new authority contract.

**Existing seams:** `api/v1alpha1/types.go`, `internal/policy/{policy,store}.go`,
`internal/governance/governance.go`, `internal/budget/budget.go`.

- [ ] Preserve the current user-budget implementation and user-rate rejection
  until its enforcement consumer actually exists.
- [ ] Define shared comparable keys for organization, durable identity, rule or
  scope, pool and authoritative window. Keep key types out of governor/store
  dependency cycles.
- [ ] Apply team, key and user limits together; do not replace the two-phase
  Governor API with a single map lookup or an unconditional allow.
- [ ] Reject unsupported or ambiguous subject/pool/window documents explicitly.
  Exercise team-only, user-only and combined selectors and stricter overlays.
- [ ] Remove a rejection only with tests proving its full admission/settlement
  consumer, version-skew behavior and restart behavior.

Verify: `go test ./internal/policy ./internal/governance ./internal/budget -race`.

Commit: `git commit -s -m "feat: key governed authority by identity pool and window"`.

## Task 6: Premium pool and total hard-cap admission (P0-03)

**Dependencies:** Task 5 keys and the durable authority in Tasks 9–10.

**Existing seams:** Governor PreCheck/Settle, budget store, router, lease client,
all three generation ingresses and their actual-attempt pricing.

- [ ] Reserve a conservative cost bound atomically before billable work. Keep
  premium and total balances distinct; total exhaustion denies before egress.
- [ ] Premium exhaustion selects only an approved, authorized, compatible
  alternative within every data/region restriction. No compatible alternative
  denies under this new admission contract.
- [ ] Preserve ADR-041's existing optional budget-tier substitution behavior.
  Do not silently redefine its fallback-to-original semantics.
- [ ] Settle observed usage and release only proven unused reservation. A retry
  obtains its own authority; timeout/cancellation with uncertain usage stays
  reserved until reconciled.
- [ ] Test concurrent near-cap requests, two devices/data planes, key rotation,
  restart, duplicate/out-of-order reports, retries and period rollover. Assert
  no denied request reaches a provider and no source loosens another hard cap.

Verify the affected governance, budget, router, proxy and ingress packages with `-race`.

Commit: `git commit -s -m "feat: enforce durable premium and total budget admission"`.

## Task 7: Capability and scope authorization (P0-04)

**Dependencies:** Task 3 identities and approved phase-0 route/capability design.

**Existing seams:** `internal/adminauth`, `internal/controlplane/{auth,policies}.go`,
`internal/server/server.go`, admin/config APIs and console capability rendering.

- [ ] Reuse the phase-0 fixed roles: platform-admin, policy-admin, provider-admin,
  budget-admin, auditor and team-admin. Authorize a capability on a resource's
  organization/team, not just membership in one role name.
- [ ] Produce an exhaustive route matrix for reads and writes, including mixed
  policy documents, pricing/provider changes and role-binding changes.
- [ ] Authenticate first. Keep heartbeat, broker and policy-write credentials
  separate; any change to current machine/management channel access needs its
  explicit design and migration coverage.
- [ ] Missing identity/resolver and denied scope fail closed. Test each role,
  cross-team/cross-org access, escalation, read-only users and break-glass access.
- [ ] The server is authoritative. Console hiding does not replace authorization.

Verify: `go test ./internal/adminauth ./internal/controlplane ./internal/server/... -race`.

Commit: `git commit -s -m "feat: authorize management capabilities by resource scope"`.

## Task 8: Complete mutation evidence (P0-04)

**Dependencies:** Tasks 3 and 7. Existing `recordMutation` is a starting point,
not evidence that the complete enterprise mutation contract already exists.

**Existing seams:** `internal/controlplane/policies.go`, `internal/audit`,
provider/model/pricing/role mutation handlers and their durable stores.

- [ ] Specify actor, capability, organization/team, resource, operation,
  generation and canonical before/after hashes for every permitted mutation.
- [ ] Do not persist raw secret-bearing before/after documents or raw IdP claims.
  Reuse the existing audit chain rather than inventing an unverified `Hash` field.
- [ ] Define transaction/outbox ordering so a committed mutation cannot be silently
  unaudited and a failed mutation is not recorded as successful.
- [ ] Return initialization and schema errors to startup. Check transaction,
  required audit, scan and iteration errors; no ignored `Exec` or `Record`.
- [ ] Test create/update/delete, denied authenticated changes, concurrent edits,
  write/audit failures, restart recovery and old/new exact-byte audit fixtures.

Verify the affected audit, control-plane and admin/config packages with `-race`.

Commit: `git commit -s -m "feat: record durable scoped management mutation evidence"`.

## Task 9: Shared durable reservation ledger (P0-05)

**Dependencies:** identity/key contract and an approved shared-state ADR.
The production target is the maintainer's Postgres-only direction, not an
implicitly selected SQLite-plus-Redis architecture.

**Existing seams:** `internal/controlplane/controlplane.go`, `internal/policy/sync.go`,
`internal/proxy`, existing Postgres store/migration patterns.

- [ ] Define atomic reserve/settle/release and idempotent reports against the
  full governed key, not only `(policy, rule, window, dataplane)`.
- [ ] Include token-quota authority alongside monetary authority with distinct
  typed units and balances. A correct microUSD ledger alone does not enforce
  `tokens_per_day`; bind quota reservations and settlement to the governed
  identity/scope and authoritative quota window.
- [ ] Issuing a grant reserves authority centrally immediately. Concurrent
  issuers cannot issue overlapping authority; a disconnected node's spend is
  never discarded just because its registry entry is pruned.
- [ ] Persist outstanding grants, observed spend and uncertain reservations.
  Expiry alone does not prove that all unused-looking authority is refundable.
- [ ] Use versioned, locked migrations and error-returning constructors.
  Reopening a store must not fail because an index already exists; check every
  `Exec`, `Scan`, `rows.Err`, transaction and cleanup result.
- [ ] Test process restart with a persistent database and explicitly future-dated
  grants. A second in-memory database is not a restart of the first; a grant with
  zero expiry is not active.
- [ ] Add real local-Postgres concurrency/restart integration coverage before
  asserting shared enforcement. The isolated SQLite probe below demonstrates
  only persistence and error handling.

Verify control-plane/proxy tests with `-race` and the phase's explicitly configured
local Postgres integration suite; no production credentials or services.

Commit: `git commit -s -m "feat: persist and reserve shared budget authority atomically"`.

## Task 10: Authoritative durable window identities (P0-05)

**Dependency:** Task 9 ledger and approved calendar/lease semantics.

**Existing seams:** `internal/policy/sync.go`, `api/v1alpha1/types.go`,
`internal/controlplane`, `internal/proxy`, `internal/budget`, `internal/tier`.

- [ ] The control plane determines and persists window identity from the rule's
  calendar period/timezone. Data planes consume it; local clocks do not invent
  competing epochs.
- [ ] Separate daily/monthly rules, identity/pool scopes and pricing/reservation
  generations. Define old-window late settlement and duplicate handling.
- [ ] Specify token-quota window semantics separately. The current
  `tokens_per_day` counter uses a duration-based 24-hour window in
  `internal/limiter`; do not silently turn it into a calendar-day budget window.
  Define durable window anchoring and an explicit migration if semantics change.
- [ ] Rollover cannot resurrect spent authority or refund uncertain requests.
  Reconnect/restart cannot re-arm a consumed allowance.
- [ ] Update tier-latch/window consumers consistently; do not leave the current
  interim month key as a competing enforcement authority.
- [ ] Test timezone boundaries, clock skew, delayed reports, outstanding
  reservations, policy edits and restart on both sides of rollover.

Verify the affected policy, control-plane, proxy, budget and tier packages with `-race`.

Commit: `git commit -s -m "feat: distribute durable authoritative budget window identities"`.

## Task 11: Finite rate authority, token quotas and safe rebalance (P0-05)

**Dependencies:** Tasks 7, 9 and 10, plus an approved rate-share wire/consumer contract.

**Existing seams:** `internal/policy/sync.go`, control-plane sync, proxy renewal,
`internal/limiter` and Governor admission. Wire delivery currently lives in
`internal/policy/sync.go`; do not assume `api/v1alpha1/sync.go` already exists.

- [ ] Define token-quota fallback-or-block explicitly, independently of monetary
  premium-pool substitution. A fallback needs its own sufficient quota authority
  and must satisfy all restrictions; it cannot bypass an exhausted hard quota.
  Preserve legacy warn behavior as explicitly non-hard enforcement.
- [ ] Replace optimistic local quota checks with durable reservation/settlement
  for the fleet contract. Count actual usage including cache tiers, retain
  uncertain reservations, and preserve independent team/key/user constraints.
  Do not assume distributing RPM/TPM shares distributes daily token quota.
- [ ] Test near-quota concurrent requests across two data planes/devices, key
  rotation, restart, duplicate reports and quota-window expiry. Total authorized
  tokens must stay within each hard quota; denied requests make no upstream call.
  Fleet enterprise readiness remains blocked until these quota tests pass.
- [ ] Represent unlimited policy separately from a finite allocation. A finite
  zero allocation means **no authority**, not the existing limiter's zero-value
  unlimited sentinel. The consumer must deny/queue it without calling a legacy
  zero-limit configuration path.
- [ ] For each finite RPM/TPM budget, divide integer units with quotient and
  remainder. The sum never exceeds the global value, including when nodes
  outnumber units. Neither a ten-percent floor nor a minimum-one floor is safe.
- [ ] Assign remainder units using stable node ordering and an explicit fairness
  cursor/epoch. Test empty membership, duplicate/unauthorized identities, negative
  inputs, uneven division, very large limits and more nodes than tokens.
- [ ] Bind grants to authenticated node/scope/generation and expiry. Issue/rebalance
  atomically against outstanding authority; do not assume every node switches
  generations simultaneously.
- [ ] Renewal or policy reload must not reset buckets or refill already consumed
  authority. Preserve rate, burst and window semantics in the implementation ADR;
  a per-window unit allocator alone is not a token-bucket implementation.
- [ ] Test join/leave, old/new overlap, dropped renewal, expiry, disconnected nodes
  and replay. In particular, 100 units across 200 nodes cannot authorize 200 units.

Verify control-plane, proxy, limiter and governance tests with `-race`.

Commit: `git commit -s -m "feat: enforce finite fleet rate and token quota authority"`.

## Task 12: Detection, transformation and destination contracts (P0-06)

**Dependency:** approved PII design; inspect the separate ADR-043 implementation
before duplicating its local inspector, routing rules or metadata.

**Existing seams:** `internal/filter`, `plugins/piimask`, shared policy types,
router, provider topology metadata and audit. Broader masking remains a distinct
workstream even if InternalOnly/Block routing is adopted.

- [ ] Distinguish not inspected, complete, unsupported/opaque and failed inspection.
  A zero-valued result is not proof that no protected data exists.
- [ ] Keep detection evidence typed and payload-free: categories, completion,
  versions and decision references, not original entity values.
- [ ] Validate actions and defaults at load. Apply entity overrides and all
  matching policies; Block wins. Model the destination ceiling and required
  transformations independently so an internal-only rule cannot cancel another
  rule's masking requirement.
- [ ] Inspection failure blocks. Incomplete coverage either blocks or stays
  inside an explicitly permitted internal boundary; it never means unrestricted
  external forwarding.
- [ ] External-unmodified requires completed inspection with no protected signal
  and all other authorization/egress checks. This is a policy decision over
  finite detectors, not a guarantee of exhaustive PII recognition.
- [ ] External-masked requires completed required transformations. Any masker
  error, partial coverage, invalid transformed representation or unsupported
  surface blocks before egress. Never discard the masker error.
- [ ] Internal-only requires approved model/provider metadata, RBAC, region,
  capability and budget constraints. Never infer trust from a provider name.

Acceptance matrix for the phase implementation:

| Case | Required result |
|---|---|
| Default/not-inspected detection result | Block or explicit internal containment; no external call |
| Completed inspection, no protected signal | Original policy-permitted route; no implied universal safety guarantee |
| Protected signal with stricter entity/team/user policy | Intersection of restrictions; Block cannot be loosened |
| Required masker fails or masks only some required surfaces | No upstream call, including internal fallback that would bypass required masking |
| Internal target missing, unauthorized, outside region or incompatible | No public fallback |
| Policy reload/rejection/version mismatch | Preserve valid protection or fail closed; never silently remove it |

Commit: `git commit -s -m "feat: define typed protected-data decisions and transformation obligations"`.

## Task 13: Enforce and evidence every egress (P0-06)

**Dependencies:** Task 12 and the actual chosen inspector/transform/router interfaces.

**Existing seams:** all three generation ingresses, both count handlers, router,
provider dispatch, `internal/audit`, metrics, topology store and console writes.

- [ ] Authenticate and inspect original protocol bytes before a lossy canonical
  conversion can hide fields. Cover system/developer content, tool definitions,
  inputs/results, scalar values and the declared unsupported-media cases.
- [ ] Inspect without mutation. When masking is authorized, update every
  representation that a provider can forward and preserve unrelated structural
  fields. Record the intentional cache/cost consequence.
- [ ] Apply the same ceiling to the complete attempt chain: initial selection,
  budget/context substitution, retry, fallback and any classifier call.
  A context preference is not a substitute for a security policy.
- [ ] Generation refusals use their ingress error shape. Count paths use a local
  200 estimate on inspection/policy/readiness refusal and make no upstream call.
- [ ] Record policy/detector versions and planned versus actual attempted
  model/provider/boundary. Do not attest application merely because a control was
  configured; do not serialize provider objects, credentials or detected values.
- [ ] Verify metadata survives file/DB reload and actual console replacement
  writes. Observe-only preferences must preserve authorized legacy request and
  retry behavior; selected alternatives and privacy constraints remain strict.
- [ ] Exercise streaming/nonstreaming, all ingresses/count APIs, unknown content,
  masking failure, denied targets, internal failure/public fallback attempts,
  stale or invalid policy, reload during a request and old/new audit chains.

Verify the relevant server, router, filter, policy, audit and metrics packages
with `-race`, then real assembled gateway tests using fake upstreams.

Commit: `git commit -s -m "feat: enforce protected-data obligations on every egress attempt"`.

## Standalone design probes

These complete test files check two narrow properties; they are not production
ledger/rate-share APIs and must not be pasted over runtime implementations.
Extract each Go fence into its named file in a temporary directory and, from the
repository root, run `go test -race /tmp/pr70-probes/*_test.go`. The SQLite import
uses this repository's existing pure-Go dependency.

### `rate_share_test.go`: finite integer partition

```go
package planprobe

import (
	"fmt"
	"math"
	"slices"
	"testing"
)

// splitFinite partitions explicitly finite authority. Zero is blocked authority,
// never "unlimited". An empty fleet leaves all authority centrally unassigned.
func splitFinite(total int64, nodes int) ([]int64, error) {
	if total < 0 || nodes < 0 {
		return nil, fmt.Errorf("negative finite authority or node count")
	}
	if nodes == 0 {
		return nil, nil
	}
	shares := make([]int64, nodes)
	q, remainder := total/int64(nodes), total%int64(nodes)
	for i := range shares {
		shares[i] = q
		if int64(i) < remainder {
			shares[i]++
		}
	}
	return shares, nil
}

func TestRateSharesStayWithinFiniteLimit(t *testing.T) {
	for _, tc := range []struct {
		total int64
		nodes int
	}{
		{100, 4}, {100, 20}, {100, 200}, {3, 10},
		{0, 2}, {100, 0}, {math.MaxInt64, 3},
	} {
		shares, err := splitFinite(tc.total, tc.nodes)
		if err != nil || len(shares) != tc.nodes {
			t.Fatalf("partition (%d,%d): %v, %v", tc.total, tc.nodes, shares, err)
		}
		var sum int64
		for _, share := range shares {
			if share < 0 || share > tc.total-sum {
				t.Fatal("negative or overcommitted authority")
			}
			sum += share
		}
		if tc.nodes > 0 && sum != tc.total {
			t.Fatalf("assigned %d, want %d", sum, tc.total)
		}
	}
	got, err := splitFinite(5, 3)
	if err != nil || !slices.Equal(got, []int64{2, 2, 1}) {
		t.Fatalf("remainder allocation: %v, %v", got, err)
	}
	for _, args := range [][2]int{{-1, 2}, {1, -1}} {
		if _, err := splitFinite(int64(args[0]), args[1]); err == nil {
			t.Fatal("accepted invalid finite allocation")
		}
	}
}
```

The wire/consumer tests in Task 11 must separately prove that zero shares deny,
unlimited is explicit, and old/new outstanding grants cannot overlap. This pure
partition function establishes none of those distributed guarantees by itself.

### `ledger_test.go`: persistent reopen and schema failure

```go
package planprobe

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func initProbeSchema(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS grants (
			id TEXT PRIMARY KEY,
			expires_at INTEGER NOT NULL
		);
		CREATE INDEX IF NOT EXISTS grants_expiry ON grants(expires_at);
	`)
	if err != nil {
		return fmt.Errorf("initialize probe schema: %w", err)
	}
	return nil
}

func openProbeLedger(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := initProbeSchema(db); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestLedgerSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.db")
	const now int64 = 1000 // fixed test clock, independent of wall-clock date
	db := openProbeLedger(t, path)
	if _, err := db.Exec(
		"INSERT INTO grants (id, expires_at) VALUES (?, ?), (?, ?)",
		"active", now+60, "expired", now-1,
	); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openProbeLedger(t, path) // same FILE, idempotent schema/index
	var count int
	if err := reopened.QueryRow(
		"SELECT COUNT(*) FROM grants WHERE expires_at > ?", now,
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("active grants after restart: %d, want 1", count)
	}
}

func TestSchemaFailureIsReturned(t *testing.T) {
	db := openProbeLedger(t, filepath.Join(t.TempDir(), "closed.db"))
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := initProbeSchema(db); err == nil {
		t.Fatal("schema failure was silently discarded")
	}
}
```

## Phase completion and PR release gates

For every runtime phase, write a failing behavioral regression, run it, implement
the approved delta, and run the relevant tests. Test identifiers and `-run`
filters must agree. A command reporting `[no tests to run]` is not verification.
Do not weaken assertions to make a draft compile.

Then run all repository gates:

```bash
CGO_ENABLED=0 go build -trimpath -o bin/mayu ./cmd/mayu
CGO_ENABLED=0 go build -trimpath -o bin/inferplaned ./cmd/inferplaned
go test ./... -race
go vet ./...
gofmt -l .
bash tests/run-all.sh
git diff --check
```

`gofmt -l .` must have empty output. Shared-state tests additionally require the
phase's local Postgres integration environment; skipping it is not a fleet
acceptance pass. Tests use fakes/local disposable stores, never production
credentials, a real IdP or real model invocation.

Before an enterprise-ready claim, demonstrate agreement among budget/rate/quota
admission authority, actual provider/model, usage/cost ledger and audit across
concurrency, failure, restart, key rotation, multiple data planes and every
protected-data path.

After pushing, inspect AI findings and inline comments for the **latest HEAD**.
Resolve substantive findings against actual code, commit with `-s`, push and
repeat. Missing/failed/incomplete review is not approval. Merge only when the
reviewed SHA is still current, blocking findings are resolved, required checks and
branch protection pass, and the target/dependency PRs match the intended path.
Never disable checks to make this plan or its implementations merge.
