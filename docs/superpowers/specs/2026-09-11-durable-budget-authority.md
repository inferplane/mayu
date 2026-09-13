# Durable global budget authority

The user requested implementation and merge of the remaining Postgres-backed
budget authority. This extends ADR-044; it does not introduce a central inference
proxy or an inference-time database call.

## Safety contract

The authoritative Postgres database reserves each grant before returning it.
For each policy/rule/subject/window, closed consumed authority plus all open grant
amounts cannot exceed the hard limit at issuance. Concurrent control planes share
database locks and database time. A lost reply, retry, restart, expired grant,
counter decrease or deleted dataplane never refunds authority.

Nodes persist grants and per-attempt reservations in a private SQLite journal.
Every billable attempt reserves a conservative microUSD bound against every
matching hard policy budget atomically before provider dispatch. A known complete
result consumes actual cost and returns the proven unused local reservation.
Timeout, cancellation, malformed/missing usage and interrupted streams retain the
full bound as uncertain authority. Actual observed cost remains a separate value.
Journal failure denies; no memory fallback exists in durable mode.

The price bound uses the immutable attempt pricing generation and verified model
context/output bounds, including the most expensive cache tier and upward rounding.
Missing metadata/pricing or overflow denies. Operator declarations must match the
provider's enforced limits; an observed bound violation poisons further admission
and is never silently treated as a safe refund. This is not an invoice guarantee.

## Persistence and epochs

Each process has a fresh boot ID. Startup atomically fences previous journal users,
burns every old OPEN grant as uncertain, including credit never used locally,
and classifies unfinished attempts as uncertain. Old grants are never re-armed.
An already closed/refunded server grant acknowledges that recovery burn without
charging or refunding twice. Only explicitly closed, fully accounted grants
return unused credit during normal operation.
Report retries use durable sequences and an unguessable request capability.

Window IDs derive from policy/rule/subject/period plus database-clock UTC calendar
boundaries, not local clocks or counter decreases. Late reports update their
original window. Delete/re-add and same-period policy edits retain that window's
liabilities. Hard limits reduced below existing liabilities issue no new credit.
Existing leases remain finite previously-issued authority until they expire or are
retired; policy edits are not instantaneous revocation of disconnected nodes.

Node expiry uses server time minus the full measured sync round-trip and a local
monotonic deadline. Restart retires deadlines instead of trusting a persisted wall
clock. The durable profile requires UTC budget calendars.

## Shared policy and control-plane replicas

Enable with `INFERPLANED_DURABLE_BUDGETS=true` and the existing secret environment
reference `INFERPLANED_POLICY_DSN`. Policy and authority use the same Postgres
database. Each authority transaction reads the policy table under a SHARE lock,
so a stale process cache cannot restore older limits. Account locks follow stable
key order. Policy writes are short existing transactions and cannot interleave an
issuance transaction's policy read.

Hard and soft/accounting budgets remain distinct. Soft windows receive durable
meter reports, not admission grants. Budget-tier activation is derived and latched
in Postgres per owned window. It survives CP failover. User-only budgets cover the
same configured opaque user across teams; team+user selectors narrow that scope.
Trusted data planes resolve identity from authenticated server-side credentials;
this does not invent a new OIDC identity/migration system.

## Data-plane integration

Opt in with `control_plane.authority.journal_path`. Require initial synchronization,
durable protocol negotiation, UTC windows, a private journal and bounded model
metadata. Legacy mode remains compatible but is not advertised as durable.
Old clients cannot obtain authority from a durable server. Upgrade the fleet before
enabling the profile.

The heartbeat carries `policy.AuthorityRequest/Response` (`escrow-v1`), no new
inference endpoint. The background syncer is woken when a request needs grants;
the request fails retryably while authority is being obtained, rather than waiting
on a central RPC. Valid local credit continues during a CP/DB outage until expiry.
Missing or exhausted credit never permits an expensive or cheap model to bypass a
hard total cap. All four generation ingresses and retries participate. Count-token
paths remain local/200 and never reserve billable authority.

## Scope and acceptance

This implements durable global GovernancePolicy money budgets for node-local mayu
fleets and interchangeable control-plane replicas. Local key/rate/quota mechanisms
remain additional existing controls; it does not claim that SQLite key stores or
rate buckets magically become a shared-gateway HA deployment.

Required evidence: real local Postgres concurrent issuers, process/store reopen,
idempotent grants and reports, no expiry refund, owned window rollover, stale CP
policy caches, key rotation/user scope, local atomic reservations, restart/uncertain
outcomes, outage continuation then expiry denial, all ingresses/retries and pure-Go
static builds. CI must exercise Postgres tests, not skip them. Latest-HEAD AI review
and all required checks precede the already-authorized merge.
