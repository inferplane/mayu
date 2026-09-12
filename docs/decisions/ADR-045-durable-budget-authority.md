# ADR-045: Durable global budget escrow

Status: Accepted implementation design, 2026-09-11.
Extends ADR-034/044 and narrows the shared-state direction in ADR-013 to
Postgres-authoritative monetary budgets for node-local fleets.

Shared-profile extension: [ADR-046](ADR-046-shared-governance.md) adds synchronous shared keys/rate/quota admission using the same policy money accounts. The local/no-inference-DB statements below describe the ADR-045 node-local profile.

## Decision

Postgres reserves the full amount of each grant before returning it. Independent
control-plane replicas share the same account rows, transaction locks, policy
table and database clock. They cannot issue the same remaining budget twice.
No inference request calls Postgres or the control plane.

The durable profile uses `INFERPLANED_DURABLE_BUDGETS=true` with the existing
`INFERPLANED_POLICY_DSN`. Policy and authority must use the same database/schema.
Every grant transaction reads the authoritative policy table under a SHARE lock;
old process caches cannot restore older limits. Account locks follow stable key
order. The schema migration is transactional and errors are fatal to attach.

`escrow-v1` extends the existing heartbeat. An authority response contains the
complete policy set, generation and budget definitions even when the set is
empty. The client verifies definitions against that policy bundle. Missing or
incompatible authority does not fall back to the old allowance protocol.

## Accounts, windows and reports

An account is keyed by policy, rule, configured subject and calendar period.
Database-clock UTC window IDs identify the exact start/end. A document edit,
delete/re-add, process restart or decrease in a reported counter cannot create a
fresh balance for the same account/window. Late reports affect their original
window. Increasing a limit is explicit policy administration; reducing a limit
below existing liabilities cannot revoke previously issued offline authority.

For each hard window, open grant amounts plus terminal consumed authority form
the encumbrance. Issuance never takes that total above the then-current limit.
Expiry and node disappearance do not refund anything. A normal, first closure
returns only proven unused authority; conflicting duplicates or negative/overflow
reports reject the entire transaction.

Soft/accounting budgets have durable cumulative meters rather than admission
grants. Tier activation and latches persist in Postgres. User-only money budgets
refer to the same configured opaque user across teams; team+user subjects retain
their narrower scope. This uses existing authenticated credential attribution,
not a new identity-provider or migration system.

## Local journal

Mayu reserves a conservative microUSD bound atomically against every matching
hard policy before each provider attempt, including fallback attempts. The
private SQLite journal commits before returning the permit. Complete, valid usage
consumes actual cost and releases the proven unused local reservation. Missing
usage, interruption, cancellation, crashes or uncertain finalization retain the
bound as uncertain authority. Observed cost is recorded separately.

The bound is computed with upward integer rounding from the immutable pricing
generation and operator-verified model context bounds, including cache tiers and
output independently. No missing metadata/rate or arithmetic overflow is treated
as a free request. Provider violations of declared bounds poison admission and
produce explicit overrun accounting without inventing observed dollars. Unknown
cache-write TTLs retain uncertainty; multiple generation choices are refused.
Bedrock SDK generation retries are disabled so gateway attempts own reservations. This bounds configured-price accounting,
not arbitrary later invoices or a malicious provider.

Each process has a fresh boot identity. Startup fences other users of the same
journal, burns all old open grants as uncertain, and never re-arms their credit.
That conservative burn also protects against stale journal restores. If Postgres
already knows a grant was normally closed/refunded, stale recovery cannot refund
or charge it a second time. Pending/unacknowledged liabilities survive cleanup;
bounded local receipt retention prevents lifetime journal growth.

Lease/window deadlines use server time minus the full sync round-trip, anchored
to the local monotonic clock. Restart retires deadlines instead of trusting a
persisted wall clock. The profile requires UTC budget calendars, a stable node
ID, machine authentication, a private journal and initial synchronization.

## Failure and operational boundaries

Grant requests run through the background heartbeat. Missing local credit wakes
that worker and returns a retryable refusal; it does not put a central RPC on the
inference path. Valid local authority continues during a database/control-plane
outage until expiry. An expired or exhausted hard budget denies even a cheap
model. Database health is reflected in the control-plane readiness endpoint.

Use an HA Postgres service and multiple control-plane instances behind a stable
endpoint. Do not clone a running journal or share it between independent nodes;
a second opener fences the first. All data planes are trusted enforcement
components with machine credentials; this protocol cannot prevent a compromised
node with direct upstream credentials from bypassing the gateway.

This implementation supplies durable **GovernancePolicy monetary authority**.
It does not turn local virtual-key/topology SQLite stores or rate/quota buckets
into an interchangeable shared-gateway deployment. Those are separate controls,
not exceptions to the global money budget. Upgrade policy producers/consumers
before enabling the durable profile; legacy mode remains explicitly nondurable.

## Verification

Real disposable Postgres tests exercise simultaneous issuers, two independent
node journals, repeated/lost replies, store reopen, node restart, stale snapshots,
policy edits, UTC rollover, historical settlement, one-time refunds, overruns and
atomic invalid-batch rejection. Assembled tests send inference through two mayu
instances sharing two control planes and one budget. Every generation ingress
has reservation/uncertainty tests; count-token requests remain nonbillable/200.
