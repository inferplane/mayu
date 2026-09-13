# Shared gateway governance

The user requested completion of shared virtual-key storage and global rate/token
quotas after ADR-045. Implement and merge this work under the existing latest-HEAD
review and CI requirements.

## Deployment contract

Add an explicit **shared gateway profile**, implementing ADR-013's shared-state
contract using Postgres only. Default SQLite/in-memory and ADR-045's node-local
escrow profile retain their behavior. Shared gateways authenticate and reserve
against Postgres synchronously; they do not call the control-plane HTTP service
for inference. A replicated Postgres endpoint and multiple gateways remove
individual-process failure points. Database/partition failure denies new shared
admissions; this profile does not promise disconnected operation.

Configuration:

```json
{
  "key_store": {"type": "postgres", "dsn_ref": {"env": "INFERPLANE_PG_DSN"}},
  "governance_store": {"type": "postgres"},
  "budget_timezone": "UTC",
  "control_plane": {
    "url": "https://control.example.invalid",
    "token_ref": {"env": "INFERPLANE_CONTROL_TOKEN"},
    "require_sync": true
  }
}
```

The shared key/governance profile uses the same Postgres database/schema as
inferplaned's durable policy/budget authority. It negotiates `escrow-v1` to receive
the complete policy/tier bundle, but requests no node-local grants. It cannot
also configure `control_plane.authority`. A shared admission books policy money
against ADR-045's existing `authority_accounts`; switching profiles or mixing
shared gateways with node-local gateways cannot mint a second policy budget.

Reject unknown backend types, inline DSNs, incompatible profile combinations,
non-UTC calendars, local policy files and mutable SQLite provider topology in the
shared profile. Backend/DSN/profile changes require restart. Shared topology is a
common versioned file/ConfigMap rollout, not per-replica admin writes.

## Keys and authoritative metadata

Implement the complete Store, TeamStore and KeyEnsurer contracts using existing
pgx dependencies and integer BIGINT limits. Preserve hashes, IDs, revocations,
expiry, ownership, metadata and every team restriction. Only generated plaintext
is returned once; no plaintext is stored or logged. Database errors never reveal
the DSN, hash, key or SQL parameters.

Resolve key and team metadata in one repeatable-read snapshot with database time.
Attach a wire-invisible team snapshot and revision digest to Principal. Ingress
guardrail/region selection uses that snapshot. Shared admission rechecks the
key/team revision before dispatch; a concurrent revoke, permission edit or team
transfer refuses instead of invoking with stale permissions.

Admin revocation supports a compare-and-set operation against the authorized
principal snapshot so a team transfer cannot race a team-admin authorization.
Replica bootstrap is insert-only, verifies conflicting declarations and preserves
existing revocation tombstones. It never replays config over admin changes.
Provide explicit SQLite-to-Postgres import preserving full hashes, revoked keys,
timestamps and teams transactionally; operators drain/freeze old writers before
cutover. Imports refuse conflicting destination identities and are idempotent.

## Atomic shared admission

Each provider attempt reserves once before dispatch and settles once afterward.
One transaction reads fresh policies and shared key/team rows under consistent
locks, checks every applicable constraint, and commits all reservations or none.
No process-local limit can authorize a shared request.

Dimensions:

* Team/key RPM and TPM use a shared token bucket, burst equal to the configured
  per-minute limit. Policy rate rules additionally support team, user and
  team+user selectors, with separate stable identities.
* Team `tokens_per_day` uses a database-clock UTC calendar-day window.
* New `tokenQuota: {limitTokens, period}` policy rules support CalendarDay and
  CalendarMonth, require FailClosed, and support the same subject selectors.
  Non-shared data planes reject these rules and user-scoped rate rules.
* Team/key calendar-day/month monetary limits are shared too.
* Policy monetary budgets use the existing ADR-045 accounts and tier judgment.
  Soft switching meters never become hard admission caps.

Counters retain identity across policy edits/restarts. Every applicable hard
constraint must pass; warn sources cannot relax blocking sources. Fresh DB policy
reads prevent an old replica restoring old limits. Counter locks follow one
stable order. Rate refill uses database time and exact integer/fixed-point
arithmetic, preserving deficit when limits change.

Reserve a conservative token bound (five declared model-window token classes)
and the existing checked monetary bound. Known complete usage releases only the
unused portion, against the original captured accounts/windows. Cache-write totals
and TTL breakdowns must never be double-counted. Token quantity can be known even
when its cache-price TTL is unknown. Partial/missing/ambiguous outcomes retain
their bounds. Invalid or excessive usage freezes affected accounts. Cancellation
before provider dispatch can explicitly return the unused reservation; crashes
and ambiguous commit results do not implicitly refund.

Finish/cancel retries are idempotent; conflicting terminal outcomes fail closed.
Minute/day boundaries cannot redirect an old settlement into a new quota window.
Global usage reporting exposes subject-authorized counter scopes and identifies
committed reservations separately from known consumption.

## Assembly and HTTP behavior

Use the existing requestpolicy reservation seam across Anthropic, OpenAI Chat,
Bedrock and Responses, including streams and fallback attempts. Shared mode skips
legacy local limit mutation; audit and protocol translation remain intact.
The control-plane remains off the inference HTTP path. Database health affects
readiness. Count-token paths remain local/200 during storage unavailability and
must never call an upstream without valid identity.

Return 429 for rate/token exhaustion, 402 for money exhaustion and retryable 503
for shared authority failure or a changed authentication snapshot. Existing auth
failures remain 401. Error text contains no secrets or unbounded metric labels.

## Delivery and acceptance

Add an ADR superseding the relevant shared-key/counter portions of ADR-013,
operator migration/HA documentation, a runnable example and honest chart guards.
Do not claim replicated topology for an optional SQLite provider store. Local
audit segments require independent per-replica storage/collection.

Real disposable Postgres tests must cover two independent pools and gateways:
key creation/revocation across replicas, immutable bootstrap, atomic competing
admissions, team/key/user scope, quota boundaries, mixed ADR-045/shared money,
policy cuts, restart, failed transactions, unknown usage, replay and outages.
All existing mandatory tests/builds, native CRD validation, Codex CLI acceptance,
independent review and latest-HEAD AI/CI checks precede the authorized merge.
