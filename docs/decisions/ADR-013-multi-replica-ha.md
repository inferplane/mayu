# ADR-013: Multi-replica HA — shared state design (implementation deferred)

**Date:** 2026-06-14
**Status:** Proposed — **design refined via a 3-family gate (codex + gemini +
kiro); implementation deferred** (requires a Postgres + Redis/Valkey environment
to build and verify; this environment has neither). No code ships with this ADR.
Gate folded in: atomic reserve/settle-delta/rollback counter contract (the
over-spend-race CRITICAL), cross-replica topology invalidation + write-conflict
handling, an explicit HA state inventory (breaker/JWKS stay local), Redis
integer/time/retry specifics, secret-ref hardening (incl. Redis auth), and an
audit segment-collection contract. Status stays Proposed (not "accepted") until
an implementation ADR validates these against a real PG/Redis env.
**Related:** §5.3 (rate/quota), §5.4 (audit), ADR-006 (live topology), ADR-008
(provider store), ADR-012 (per-instance audit anchoring); the `keystore.Store` /
`limiter` / `budget` / `providerstore.Store` interfaces (the swap seams)

## Context

inferplane runs today as a **single binary with instance-local state**: a SQLite
key store and **in-memory** two-phase governance (the `limiter` token buckets and
the `budget` µUSD counters). That is the zero-dependency default and must stay.

**HA = multiple replicas behind a load balancer.** Three pieces of state are no
longer correct when instance-local:

1. **Key store** — a key issued on replica A must resolve on replica B.
2. **Quota / budget counters** — daily/monthly token windows and the monthly
   µUSD spend ceiling must be **shared and atomic** across replicas, or each
   replica enforces its own copy and the real limit is `N ×` the configured one.
3. **Rate limit** — the per-team RPM/TPM token bucket must be **summed across
   replicas** (a 100-RPM team on 4 replicas must still see 100 RPM total, not
   400).

The good news: each is already behind an **interface** (`keystore.Store`,
`limiter`, `budget`), so HA is **new backend implementations**, not a rewrite —
consistent with the project's "interface, not rewrite" posture (data.md).

## Decision (design)

**Shared state via Postgres (durable) + Redis/Valkey (atomic counters), each a
new backend behind the existing interface; in-memory/SQLite stays the default.**

### 1. Key store → Postgres (`keystore.Store`)

A `PostgresStore` implementing the existing `keystore.Store`. The SQLite schema
is **already Postgres-portable** (TEXT/INTEGER only, by design — keystore CLAUDE
note), so the DDL maps directly; the only changes are the driver (`pgx`),
placeholders (`$1`), and `ON CONFLICT`. Selected by `key_store.type: "postgres"`
+ a `dsn` **secret ref** (never inline, §7). The `providerstore.Store` (ADR-008,
same portable DDL) gets the same Postgres backend so the topology store is shared
too — but sharing the store is not enough (gate):

- **Cross-replica topology invalidation.** ADR-008's write path mutates the DB
  then reloads the WRITER replica's `live.State`; other replicas would keep
  serving STALE topology. Fix: a broadcast — **Postgres `LISTEN/NOTIFY`** (or a
  Redis Pub/Sub channel) carrying a topology **generation id**; every replica
  subscribes and triggers the SAME `reloadLocked()` on bump. So a UI write on
  replica A re-publishes the generation on B…N within the notify latency.
- **Write-conflict / lost-update.** Two replicas writing the provider store
  concurrently need conflict detection: a `generation` (or `updated_at`) column
  with optimistic-lock compare-and-set on write (reject + retry on mismatch), so
  a lost update can't silently drop a registration.
- **File-config topology (no provider store).** Hot-reload is still per-replica
  SIGHUP; in HA the operator's orchestration must signal all replicas (a K8s
  ConfigMap rollout restarts/signals every pod) — documented, since there is no
  shared store to broadcast through in that mode.

### 2. Quota + budget → Redis/Valkey two-phase (`limiter` / `budget`)

The two-phase contract maps to **atomic Redis Lua scripts** (one round-trip;
no read-modify-write race across replicas). The contract is **reserve →
settle-delta → rollback** for BOTH quota and budget (gate CRITICAL — a read-only
PreCheck lets N replicas all pass the ceiling and over-spend):

- **PreCheck (reserve, atomic):** the Lua script does `if current + reservation >
  limit then return DENY else INCRBY key reservation; return ALLOW` — the check
  and the increment are one atomic op, so concurrent replicas cannot all slip
  under the ceiling. The reservation is the estimated tokens (quota) / estimated
  µUSD (budget).
- **Settle (delta, atomic):** after the upstream returns, `INCRBY key (actual −
  reservation)` corrects the reserved estimate to the true cost (the delta may be
  negative). A denied/short request thus never overcharges.
- **Rollback / crash safety:** a replica crash between PreCheck and Settle would
  otherwise leak a reservation. Mitigation: reservation keys carry the **window
  TTL** (so a leaked reservation expires with its window — bounded over-count),
  and Settle is **synchronous in the request path** (same as the in-memory
  governor today). A pre-upstream reject path explicitly decrements the
  reservation.
- **Integer-only, bounded (gate):** budget is **integer µUSD** — `INCRBY` on
  micros is exact (no float; the Lua does only integer compare/add — preserves
  the integer-µUSD mandate). The script rejects out-of-range / non-integer
  reservations.
- Keys carry the window (`team:quota:day:2026-06-14`, `team:budget:month:2026-06`)
  so expiry is natural and no sweeper is needed.

### 3. Rate limit → Redis distributed token bucket (`limiter`)

A Redis Lua **token-bucket / GCRA** keyed per team: all replicas refill/drain the
SAME bucket, so enforcement is summed, not per-replica. Specifics (gate): the
script uses the **Redis server clock** (`redis.call('TIME')`) — not replica wall
clocks, which skew — for refill; all token math is **integer** (tokens, not
fractional); keys are `team:rate:rpm` / `team:rate:tpm`; it returns allow/deny +
**retry-after** seconds atomically. The in-memory `limiter` stays the default;
the Redis impl is selected when a shared `governance_store` block is configured.

### 4. Audit chain across replicas

The audit hash chain is **already per-instance segmented** (§5.4, ADR-012: each
process run is its own chain, distinct `instance` id = host + ULID, so N replicas
never collide) — so N replicas produce N independently-verifiable chains, each
anchored separately (ADR-012). No shared *counter* state is needed. The HA
**collection contract** (gate): each replica must ship its file sink + its WORM
anchors to a shared/aggregated location (a shared log store, or per-replica S3
prefixes the auditor enumerates), and `inferplane audit verify` runs **per
segment**; the auditor verifies every replica's segment + its anchors. (No
single global chain — that would reintroduce a shared writer / SPOF.)

### 5. HA state inventory (local vs shared)

| State | HA placement | Rationale |
|---|---|---|
| Virtual keys | **Postgres** (shared) | must resolve on any replica |
| Provider/model topology | **Postgres** + NOTIFY/generation | shared + cross-replica invalidation (§1) |
| Quota / budget counters | **Redis** (shared, atomic) | exact summed enforcement (§2) |
| Rate-limit bucket | **Redis** (shared, atomic) | summed RPM/TPM (§3) |
| Audit chain | **per-instance** (no shared state) | independent segments + anchors (§4) |
| Circuit-breaker state | **instance-local (intentional)** | each replica learns an upstream is down within a few requests; a shared breaker is a possible follow-up but not required — fail-fast recovery makes the N×-probe cost acceptable |
| OIDC JWKS / discovery cache | **instance-local (intentional)** | standard; N× IdP fetches are within normal IdP tolerance |
| Live topology generation (in-mem) | **instance-local cache**, invalidated by the shared NOTIFY (§1) | the cache is local; consistency comes from the broadcast |

Instance-local choices are **deliberate trade-offs**, recorded here so they are
not accidental gaps.

### Config shape (illustrative)

```json
"key_store":   { "type": "postgres", "dsn_ref": { "env": "INFERPLANE_PG_DSN" } },
"provider_store": { "type": "postgres", "dsn_ref": { "env": "INFERPLANE_PG_DSN" } },
"governance_store": { "type": "redis", "addr_ref": { "env": "INFERPLANE_REDIS_ADDR" } }
```

Connection secrets — the Postgres DSN, the Redis address/password, and any TLS
client material — are **secret refs only** (`env:`/`file:`), validated like
`api_key_ref`: an inline DSN/Redis URL is **rejected at load** (§7), and they are
**redacted from logs/errors** (a DSN often embeds the password). Absent any
`*_store` block → today's SQLite + in-memory single-binary behavior, unchanged.

**Zero-dependency default is a hard acceptance criterion:** with no Postgres/
Redis config the binary must not import-active, connect, migrate, or require
either at startup or in tests — the default boot path stays pure-Go SQLite +
in-memory (the HA drivers compile in but are inert, like the OTel/S3 SDKs).

**Redis failure policy:** the shared governance store is on the critical path
(unlike best-effort tracing/anchoring) — a Redis outage must **fail closed for
enforcement** (deny when the limiter/budget cannot be consulted) or be an
explicit operator-chosen fail-open; the implementation ADR pins this. (This is
the one HA backend that is NOT best-effort.)

## Alternatives considered

1. **Sticky sessions (LB pins a team to one replica).** Rejected — it makes
   per-team state instance-local "by routing," but a replica restart/rebalance
   loses counters and re-pins, and one hot team can't scale past one replica. It
   defeats the point of HA.
2. **Per-replica quota = limit / N.** Rejected — imprecise (uneven LB → some
   replicas throttle early, others never), and N changes on scale events; billing
   ceilings must be exact (integer-µUSD mandate), not divided-and-hoped.
3. **Gossip / CRDT counters across replicas.** Rejected — eventually-consistent
   counters under-count briefly, risking over-spend past a budget ceiling; the
   complexity dwarfs a Redis INCRBY, which is atomic and exact.
4. **A leader replica owns counters (others RPC to it).** Rejected — the leader
   is an SPOF and a bottleneck; Redis/Valkey is the purpose-built, HA-able shared
   counter.
5. **Postgres for the counters too (skip Redis).** Considered — works, and avoids
   a second dependency, but high-RPM atomic counter UPDATEs contend on row locks;
   Redis is the right tool for hot counters. Operators who refuse Redis can run a
   single-replica (today's path) or accept Postgres-counter contention (a future
   knob). Not the default HA design.
6. **Ship now without a Postgres/Redis env.** Rejected — unverifiable code
   (no integration test possible here) would violate the project's gate
   discipline. The design is recorded; implementation lands in an env with both.

## Consequences

- HA is a **bounded, interface-level addition**: `keystore.Store` /
  `providerstore.Store` Postgres impls + `limiter`/`budget` Redis impls, selected
  by config; the core (handlers, governance two-phase contract, router, audit)
  is unchanged.
- The **single-binary, zero-dependency default is preserved**: no `*_store`
  config → SQLite + in-memory, exactly as today.
- The integer-µUSD billing mandate holds across replicas (Redis `INCRBY` on
  micros is exact; no float, no CRDT under-count).
- Audit needs no shared state (per-instance chains + per-instance anchoring).
- **Implementation deferred**: it requires a Postgres + Redis/Valkey environment
  to build (drivers), test (integration), and verify (summed enforcement,
  failover). Tracked as the next milestone once such an env is available; an
  implementation ADR will supersede this design ADR at that time.
