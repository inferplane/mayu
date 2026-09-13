# ADR-046: Postgres shared gateway governance

Status: Accepted implementation design, 2026-09-12.
Supersedes ADR-013's shared-key/rate/quota backend choices. Complements ADR-045;
its node-local monetary escrow profile remains available.

## Decision and failure boundary

An explicit `key_store.type: postgres` plus `governance_store.type: postgres`
profile supports multiple gateways against one authoritative Postgres
database/schema. Key resolution and admission are synchronous transactions.
No inference request calls the control-plane HTTP service. This profile requires
an HA database endpoint for availability and refuses new requests on database
failure; it does not offer disconnected key/rate/quota enforcement.

The default SQLite/in-memory profile retains its single-replica limit.
ADR-045's node-local profile retains local monetary reservations and finite
outage authority. Shared mode cannot also enable its local journal. A common
versioned file rollout supplies topology; SQLite provider-store admin mutation is
rejected in shared mode. This does not implement ADR-013's proposed distributed
mutable topology store.

## Shared identity

Postgres implements the existing key/team interfaces with BIGINT limits, hashed
keys, revocation tombstones and all existing metadata. Resolve reads key and team
in one repeatable-read snapshot using database time. The Principal carries an
internal revision and team snapshot; neither is serialized. Ingress region and
guardrail selection use that snapshot. Admission checks the current revision,
so a permission edit or revoke between authentication and dispatch cannot widen
the request.

Admin and self-revocation compare the authorized revision in the write transaction.
Replica bootstrap registers immutable original declaration fingerprints. Repeating
that declaration does not overwrite later admin edits or recreate deleted rows;
conflicting replica declarations refuse startup.

SQLite import copies raw hashes, IDs, timestamps and revocations in one destination
transaction. It neither regenerates plaintext nor reads a public List projection
that would omit revoked keys. Operators must drain/freeze old writers before
cutover. A conflicting import rolls back, and an identical retry is idempotent.

## Global admission and accounting

One transaction locks definitions and relevant counters in a stable order,
checks every applicable constraint and reserves all or none. It covers:

- Team/key RPM and TPM, plus team/user/team-user policy rate rules.
- Team daily token quotas and policy `tokenQuota` day/month windows.
- Team/key day/month monetary limits.
- Policy monetary budgets in the existing ADR-045 `authority_accounts`.

Policy monetary reservations therefore compete with outstanding node-local
grants; switching profiles cannot create a second budget. Soft pending money
remains a capacity liability but does not activate a soft switching tier until
it becomes settled or uncertain consumption.

Rate buckets use database time and exact fixed-point debt, including the original
reservation segments. Refilling an old segment cannot make its later cancellation
credit an unrelated newer request. Calendar counters use explicit UTC windows.
Rule identity survives edits and restarts; terminal settlement uses the captured
original windows, even after policy deletion or midnight.

The caller reserves conservative token and microUSD bounds from its immutable
model/pricing snapshot. Known complete observations release only proven unused
amounts. Token quantity may be known while cache-price TTL is unknown; those
dimensions settle independently. Partial, missing or ambiguous results retain
their bounds. Invalid usage freezes affected accounts. Explicit pre-dispatch
cancellation can refund; crashes, expiry and ambiguous commits cannot.

Exact terminal replays are idempotent; conflicting outcomes refuse. Open permits after crashes/ambiguous acknowledgements remain pending rather than
automatically converting to terminal consumption. Their commitments still bind
hard budgets. Historical permit/account data is retained; this change does not add a pruning or frozen
account reconciliation API.

## Policy and runtime binding

Postgres assigns one persistent authority namespace. A shared gateway's background
`escrow-v1` client requests no grants and verifies both namespace and policy
generation against its own database. Equal policies in separate databases are
insufficient. The router captures a policy generation atomically with its rules,
and admission compares it with fresh DB policy before dispatch. Changes during a
request cause a retryable refusal rather than mixing policy generations.

`tokenQuota` requires FailClosed. Non-shared gateways reject it and retain a
mandatory-rule rejection gate. User-scoped rates are enabled only with the shared
enforcer. Producers, consumers and the CRD must be upgraded before activation.

Every generation ingress, stream and fallback attempt uses the shared reservation
seam. Shared mode disables legacy local counter mutations. Counts remain local;
unavailable identity storage returns local zero counts without provider calls.
Global `/v1/usage` reports committed/reserved amounts explicitly. Rate/token
exhaustion is 429, money exhaustion 402, storage or snapshot mismatch 503.

## Deployment and validation

Shared replicas need separate audit files/WALs. The Helm chart rejects multiple
replicas in local mode. Persistent shared mode uses a StatefulSet with per-replica
claims; multiple replicas require distinct nodes and a disruption budget.
Database replication/failover and collection of every audit segment remain
operator deployment responsibilities.

Validation covers independent pools and assembled gateways, key creation/revocation
across replicas, snapshot changes, immutable bootstrap/import, concurrent
reservations, all subject scopes, exact refill, policy edits, rollover, uncertainty,
replay, restart and database/control-plane failures. These tests establish the
configured accounting contract, not a production throughput benchmark or an
external provider invoice guarantee.
