# ADR-015: Analytics Mode B — shared Postgres store + fenced single-writer aggregator

**Date:** 2026-07-07
**Status:** Accepted (implementation lands with this ADR — a real Postgres test
environment is available, unlike ADR-013's deferred HA program at the time it
was written).
**Related:** §4.1/§4.6 of `docs/superpowers/specs/2026-06-26-admin-console-litellm-ux-redesign-design.md`
(the console UX spec that named Mode A/B); `docs/superpowers/plans/2026-06-29-console-phase1a-analytics-index.md`
(Phase 1a, Mode A — shipped); ADR-013 (multi-replica HA design — the Postgres
config-shape/secret-ref conventions this ADR reuses); ADR-012 (per-instance
audit anchoring — the segment/collection framing this ADR builds on);
`internal/analytics/` (Mode A, extended here); `internal/providerstore/`
(the SQLite migration-discipline reference).

## Context

Phase 1a shipped **Mode A**: a single-replica, disposable SQLite index derived
from the audit `Sink` fan-out, queried by `internal/server/analyticsapi/`. It
explicitly deferred two things to "Phase 1b" (its plan doc, out-of-scope list):
**Mode B** (a shared, cluster-wide analytics store with a fenced single-writer
aggregator) and the `/admin/analytics/health` drift/health endpoint.

Two design questions were left open by the console spec and are decided here:

1. **§4.6 (round-2 review, unresolved prerequisite): how does the Mode B
   aggregator collect audit across replicas?** ADR-013 §4 frames audit as
   per-instance segments an external collector aggregates — but no such
   collector exists in this codebase, and building one (segment discovery,
   finalization markers, a shared prefix contract) is a distinct, large piece
   of work matching ADR-013's own deferred HA program.
   **Decision: out of scope here.** Mode B's aggregator takes an
   **operator-provided, already-aggregated audit source** — a directory of
   JSONL segment files the operator's own log-shipping (rsync, Vector,
   Fluentd, a shared volume, whatever they already run) keeps merged across
   replicas. inferplane does not ship a cross-replica collector. This mirrors
   the "declare Mode B's input to be an operator-provided source" alternative
   the spec explicitly named as acceptable.
2. **Backend: keep the shared store SQLite-only (Postgres-portable schema,
   not-yet-Postgres), or ship a real Postgres backend now?**
   **Decision: ship real Postgres now**, via `pgx` (pure Go, no cgo — keeps
   `CGO_ENABLED=0`). Unlike ADR-013 at the time it was written ("this
   environment has neither" Postgres nor Redis to build/verify against), a
   Postgres 16 instance is available for this work's integration tests. This
   ADR is accordingly the **first real Postgres-backend implementation** in
   the project — narrower in scope than ADR-013's full HA program (which also
   covers `keystore.Store`, `providerstore.Store`, and Redis-backed
   governance), but it validates the same "new backend behind the existing
   interface, zero-dependency default preserved" pattern ADR-013 designed.

## Decision

### 1. `analytics.Store` interface — Mode A and Mode B behind one seam

Extract an interface `Store` (`internal/analytics/store.go`) with the query
surface `analyticsapi.Querier` already needs (`Summary`, `TimeSeries`) plus
`Health() (Health, error)`. `*Index` (existing Mode A SQLite) and the new
`*PGStore` (Mode B Postgres) both implement it. `analyticsapi` already only
depends on the structural `Querier` interface — no ingress-side change. The
gateway assembly (`cmd/inferplane/gateway.go`) picks ONE store per config and
hands it to `analyticsapi`; Mode A and Mode B are never both queried by the
same replica.

`internal/analytics` also declares `Rebuilder interface { Rebuild(context.Context)
error }` alongside `Store` (round-1 finding: `analyticsapi` originally
redeclared this interface itself and only checked it via a runtime type
assertion, so a Postgres-side signature drift would surface only at
integration time, not at either side's own compile step). `pgstore.Store`
carries a compile-time `var _ analytics.Rebuilder = (*Store)(nil)` assertion;
`analyticsapi`'s handler still does the SAME runtime `if r, ok :=
querier.(analytics.Rebuilder); ok` check to decide 404 vs 204 (Mode A has no
`Rebuild` method and stays that way — the runtime check is still needed to
pick 404 for Mode A, but drift between the two `Rebuild` signatures is now
impossible to compile).

Mode A's ingestion (the async `audit.Sink` adapter, `sink.go`) is **unchanged**
and is simply not wired up when Mode B is configured — per §4.6's "1→N
transition," the local SQLite index is left alone (still rebuildable, just
unused) rather than actively drained.

### 2. Postgres schema — same discipline as Mode A, plus what Mode B needs

Reuses Mode A's `events` columns verbatim (TEXT/INTEGER-only, already
Postgres-portable) but with a real **upsert** instead of insert-or-ignore —
Mode A's first-write-wins can't correct a late-settled cost; Mode B uses
`INSERT ... ON CONFLICT (id) DO UPDATE SET ... = excluded.*` (the
`providerstore.go` pattern), so a re-ingested or corrected record for the same
ULID overwrites cleanly. Three additional tables:

```sql
CREATE TABLE IF NOT EXISTS checkpoints (
  segment       TEXT PRIMARY KEY,   -- the aggregated-source file name
  byte_offset   BIGINT NOT NULL DEFAULT 0,
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS lease (
  id            TEXT PRIMARY KEY,   -- singleton row, id = 'mode_b_aggregator'
  holder        TEXT NOT NULL,      -- opaque instance id (host + ULID, like audit's)
  epoch         BIGINT NOT NULL,
  expires_at    TIMESTAMPTZ NOT NULL
);
```

`expires_at`/`updated_at` are `TIMESTAMPTZ`, not `TEXT` — a plan-gate finding
(round 1, confirmed by 2 independent reviewers): TEXT can't be `<`-compared
against `now()` without a cast, and RFC3339Nano's variable-length fractional
seconds make lexical string comparison unreliable at sub-second precision.
The rest of the schema stays TEXT/INTEGER-only (mirroring Mode A for the
`events`/`rollup_day` shapes), but that rationale never applied to `lease`/
`checkpoints` in the first place — Mode B's own store is Postgres from day
one, not a dual SQLite/Postgres target, so there is nothing to stay portable
with for these two columns.

```sql
CREATE TABLE IF NOT EXISTS rollup_day (
  day TEXT NOT NULL, team TEXT NOT NULL, model TEXT NOT NULL,
  input_tokens BIGINT NOT NULL DEFAULT 0, output_tokens BIGINT NOT NULL DEFAULT 0,
  cost_micros BIGINT NOT NULL DEFAULT 0, request_count BIGINT NOT NULL DEFAULT 0,
  PRIMARY KEY (day, team, model)
);
```

`rollup_day` exists so `Summary`/`TimeSeries` don't `GROUP BY` the full
`events` table on every query once it's large (Mode A's live-`GROUP BY`
approach doesn't scale to a cluster-wide, multi-replica-fed table). Per §4.6
("rollups are derived only from the deduped immutable event table, never
incremented blindly"), the aggregator recomputes `rollup_day` rows **only for
the `(day, team, model)` tuples touched by the current batch**, via
`INSERT ... SELECT ... FROM events WHERE day IN (...) GROUP BY ... ON CONFLICT
DO UPDATE`, inside the same transaction as the event upserts — so a re-applied
batch (crash-retry) is idempotent by construction, never double-counted.

**Invariant this recompute scheme relies on**: a corrected record (same ULID,
re-upserted) never changes its `(day, team, model)` bucket — only `cost_micros`
and the token counts are ever corrected post-hoc; a request's team/model/day
are fixed at creation and never reassigned. This holds today (audit records
have no "re-team" or "re-model" operation). If that ever changes, a bucket-
reassigning correction would leave the OLD tuple's rollup stale (it's never
revisited, since recompute only touches tuples the current batch's rows
belong to NOW) — a full-tuple rebuild would be needed at that point, not a
per-batch touched-tuple recompute. Flagged here rather than built for now
(no reassignment operation exists to test against).

Schema created via `ensureSchema` mirroring `providerstore.sqlite.go`'s
`BEGIN`/`CREATE TABLE IF NOT EXISTS`/`COMMIT` migration discipline (Postgres
uses `SELECT pg_advisory_lock(847001)` / `pg_advisory_unlock(847001)` in place
of SQLite's file-level `BEGIN EXCLUSIVE` for the same cross-process
serialization — `847001` is this feature's reserved lock key; round-2 minor
finding: pin a literal here so a future Postgres-backed `keystore`/
`providerstore` migration (ADR-013) picks a different one rather than
colliding on an unspecified value).

### 3. Fenced single-writer aggregator

One goroutine per replica; only the lease-holder does work. **Revised after
plan-gate round 1** (2 independent reviewers, both CONFIRMED): the original
"SELECT-then-write" fencing check is a check-then-act race under Postgres's
default READ COMMITTED isolation — a `SELECT` does not lock the row, so
another replica can steal the lease and commit between this transaction's
fencing check and its own commit, letting a stale leader's writes land after
it has lost leadership. Fixed below via row-level locking, not a bare read.

- **Lease acquire/renew**: `SELECT epoch, holder FROM lease WHERE
  id='mode_b_aggregator' FOR UPDATE` (blocks until any concurrent
  acquire/renew/rebuild — see below — releases the row), then in the SAME
  transaction: `UPDATE lease SET holder=$1, epoch = CASE WHEN holder=$1 THEN
  epoch ELSE epoch+1 END, expires_at = now() + ($2 * interval '1 second')
  WHERE id='mode_b_aggregator' AND (expires_at < now() OR holder=$1)` — `$2`
  is the TTL in seconds (a plain number), and the expiry is computed **by the
  database's own clock**, never the caller's. **Revised after plan-gate round
  2** (both reviewers CONFIRMED): the original `expires_at=$2` took an
  app-precomputed timestamp, mixing the writing replica's wall clock into a
  value the READING side compares against `now()` — under replica clock skew
  one holder could write an expiry far in the future (blocking failover past
  the configured TTL) or already in the past (causing churn). Computing it in
  SQL keeps both the write and the comparison on the same, single clock. The
  `CASE` makes the epoch bump **conditional on an actual handover** — a
  same-holder renewal leaves the epoch unchanged, matching the "stable epoch
  across a poll loop" invariant (the original unconditional `epoch=epoch+1`
  bumped on every renewal too, contradicting that invariant and this ADR's own
  test spec — round-1 finding). `INSERT ... ON CONFLICT DO UPDATE ... WHERE
  ...` handles the first-ever row. This acquire/renew runs in its OWN
  transaction (commits immediately once the `UPDATE` returns), separate from
  the ingest transaction below — holding `FOR UPDATE` open for an entire
  multi-thousand-row ingest batch would needlessly serialize renewal against
  ingest on the SAME replica. **The caller (a tick) keeps the epoch this call
  returns** — that captured value is what the ingest transaction's fencing
  check (below) must match EXACTLY, not just "same holder." Poll interval and
  lease TTL are config (default 5s poll / 15s TTL — three missed polls before
  another replica can take over).
- **Fencing on write (row-locked AND epoch-pinned to tick start)**: an ingest
  batch's transaction starts with `SELECT epoch FROM lease WHERE
  id='mode_b_aggregator' AND holder=$1 FOR UPDATE` — this BOTH checks fencing
  AND locks the lease row for the remainder of the transaction, so no other
  replica's acquire/renew (which itself needs the same row lock) can succeed
  until this transaction commits or rolls back (closes the round-1 race,
  finding 1). The check requires an **exact match against the epoch the tick
  captured when it called acquire/renew at tick START**, not merely "epoch
  belongs to holder=$1" — see Rebuild below for why an exact match (not just
  same-holder) is required. A mismatch aborts immediately (no writes
  attempted) and the tick simply retries fresh next poll.
- **Segment discovery + tailing**: the configured directory is listed each
  poll tick; files are processed in lexical-sort order (the operator's
  aggregation is expected to name segments sortably, e.g. by rotation
  timestamp — documented, not enforced); each segment resumes from its
  `checkpoints` row (0 if new); only complete (`\n`-terminated) lines are
  read, mirroring Mode A's `Replay` crash-truncation handling — and the
  checkpoint's `byte_offset` is pinned to the byte position immediately
  BEFORE any incomplete trailing bytes (never past them), so a line
  completed by a later append is still ingested, not permanently skipped
  (round-1 finding — the original spec said "don't ingest the partial line"
  without pinning where the checkpoint stops).
- **Atomic ingest unit**: per batch (bounded by a max-lines-per-tick, default
  5000, to bound transaction size and poll latency, and possibly spanning
  MULTIPLE segments in one tick), one transaction: the fencing `SELECT ...
  FOR UPDATE` above + event upserts + touched-tuple rollup recompute + a
  checkpoint advance **per segment represented in the batch** (a
  `map[segment]byte_offset`, not a single value — round-1 finding: a batch
  spanning two segments needs two checkpoint rows advanced atomically, not
  one). All of it succeeds or none does. **This transaction runs whenever
  `checkpointUpdates` is non-empty — i.e. whenever any bytes were consumed
  this tick — never gated on "are there billable records"** (round-2 finding:
  a tick that reads only malformed/unparseable complete lines up to
  `MaxLinesPerTick` has zero billable records but MUST still advance past
  those bytes, or the same malformed lines are re-read forever, permanently
  blocking any later-arriving valid line in that segment. Event/rollup writes
  are simply empty for such a batch; the checkpoint advance still happens.)
- **ULID ordering**: `day` (the bucketing key `TimeSeries` groups by) is
  derived from each record's own timestamp field already (Mode A's `dayOf`,
  reused) — records are bucketed by their own content, not arrival order, so
  cross-segment merge and replica clock skew never misplace a day bucket.
  (The spec's "order by ULID, not wall clock" concern is satisfied by this —
  bucketing was never wall-clock-based even in Mode A.)

### 4. `/admin/analytics/health` + capability wiring

New endpoint, full-admin only (matches Phase 1a's `/admin/analytics/*` authz):

```json
{
  "mode": "A" | "B" | "off",
  "is_leader": true,
  "lease_epoch": 42,
  "lag_seconds": 3,
  "last_ingest_ts": "2026-07-07T08:00:00Z",
  "segments_tracked": 3
}
```

`lag_seconds` is defined precisely (round-1 finding — the field existed but
its formula didn't): `now() - max(checkpoints.updated_at)` across all tracked
segments; `0` with `segments_tracked: 0` means "aggregator has never
completed a tick yet" (not "caught up" — the UI distinguishes the two via
`segments_tracked`).

`chain_verification_status` (named in the console spec's capability shape) is
**explicitly out of scope** here: re-verifying the tamper-evident hash chain is
`audit verify`'s job, orthogonal to analytics lag/freshness, and wiring it in
would mean the analytics package re-implements audit-chain verification for no
query-facing benefit. Documented as a deliberate trim, not an oversight.

`GET /admin/capabilities`'s `analytics_index` field flips from `"A"` to `"B"`
when a Mode B store is configured and reachable at boot (config presence, not
a live health check — matches Mode A's boot-time capability resolution).

### 5. Config + secrets (ADR-013 conventions, reused verbatim)

```json
"analytics": {
  "mode_b": {
    "dsn_ref": { "env": "INFERPLANE_ANALYTICS_PG_DSN" },
    "aggregated_audit_dir": "/mnt/shared/audit-aggregate",
    "poll_interval": "5s",
    "lease_ttl": "15s"
  }
}
```

`dsn_ref` is a secret ref (`env:`/`file:`, never inline) — validated exactly
like `api_key_ref` (§7); an inline `dsn` key is rejected at config load, and
the DSN is never echoed in an error (it can embed a password). Absent
`analytics.mode_b` → today's Mode A/off behavior, unchanged; the `pgx` import
compiles in but is inert (no connection attempt) with no `mode_b` block — the
zero-dependency default holds, per ADR-013's own acceptance criterion.

### 6. Rebuild / recovery

`/admin/analytics/health` surfaces staleness (`lag_seconds` past a
config-file-defined threshold triggers a UI banner, client-side — no new
server logic needed beyond exposing the number). Rebuild is **operator-
triggered**: a `POST /admin/analytics/rebuild` admin action that truncates
`events`/`rollup_day`/`checkpoints` and lets the aggregator re-tail from byte 0
on all tracked segments. No auto-rebuild in this phase (the spec allows
gating it behind a flag later; YAGNI for now — an operator noticing a stale
banner can hit one button).

**Rebuild fencing (round-1 finding — the original text called this "safe to
call while the aggregator is running" with no synchronization mechanism):**
`Rebuild` takes the SAME `SELECT ... FOR UPDATE` lock on the `lease` row that
an ingest transaction's fencing check takes (§3), inside the SAME transaction
as the truncate. Postgres's row-lock queueing means Rebuild and an in-flight
ingest batch (one that has ALREADY reached its `FOR UPDATE` fencing check)
can never interleave — whichever transaction's `FOR UPDATE` acquires the row
first runs to completion (commit or rollback) before the other proceeds.

That much closes the round-1 concern (a rebuild can't be half-undone by a
concurrently COMMITTING batch), but round 2 found it doesn't close the whole
race: a tick reads its segments' checkpoints (a plain, unlocked read) BEFORE
it ever reaches the ingest transaction's fencing check. If Rebuild runs and
commits entirely inside that window — after the read, before the fencing
check — the tick's fencing check still sees the same holder+epoch (Rebuild
alone doesn't change either), passes, and writes a checkpoint advance
computed from the STALE pre-rebuild offset into the now-truncated table,
permanently skipping whatever bytes existed before that stale offset.

**Fix: `Rebuild` bumps `lease.epoch` too** (`epoch = epoch + 1`, `holder`
unchanged) inside its truncate transaction, even though it isn't a handover.
The fencing check in §3 is accordingly an EXACT match against the epoch the
TICK captured at its own tick-start acquire/renew call — not "any epoch
belonging to holder=$1" — so ANY epoch change since the tick started
(a leadership handover OR a rebuild) invalidates that tick's entire batch.
The tick simply retries fresh next poll, re-reading checkpoints that are now
either post-rebuild-zero or post-handover-current, never stale.

## Alternatives considered

1. **Build the cross-replica collector too (§4.6's larger-scope option).**
   Rejected for this phase — doubles the surface (a new segment-discovery/
   finalization-marker contract touching ADR-012/013) for a problem operators
   already solve with existing log-shipping tools. Revisit if operators report
   the "already-aggregated source" requirement is a real deployment burden.
2. **Keep Mode B SQLite-only, defer Postgres to when ADR-013's full HA program
   ships.** Rejected — a real Postgres test env is available now (unlike when
   ADR-013 was written), and Mode B analytics is a self-contained, lower-
   blast-radius place to validate the pattern before the higher-stakes
   keystore/provider-store Postgres backends.
3. **Redis for the lease/fencing (matches ADR-013's counter design) instead of
   a Postgres lease row.** Rejected — introduces a second new dependency for
   one singleton row; a Postgres `UPDATE ... WHERE` CAS is sufficient for a
   5-second-poll leader election (this is not a hot-path atomic counter like
   quota/budget, where ADR-013 specifically chose Redis for throughput).
4. **Incremental rollup counters (`rollup.spend += x` per event) instead of
   recompute-from-events per touched tuple.** Rejected per §4.6 explicitly —
   blind increments double-count on any re-applied batch (crash-retry,
   fencing-aborted batch); recompute-from-source is the only idempotent option
   given events are themselves idempotently upserted.

## Consequences

- `internal/analytics/` grows a `Store` interface + a new `postgres.go` (or a
  `mode_b` subpackage) — Mode A's existing code is untouched except for the
  interface extraction, which is additive (no behavior change, confirmed by
  Mode A's existing test suite staying green).
- First real Postgres dependency in the project (`pgx`, pure Go). `go.mod`
  gains it; `CGO_ENABLED=0` static build is unaffected.
- Validates the ADR-013 Postgres-backend pattern at small scope before the
  larger keystore/provider-store HA program; that ADR's implementation can
  reuse this one's `ensureSchema`/lease/secret-ref conventions directly.
- No cross-replica audit collector ships — operators running Mode B must
  already have (or set up) their own log aggregation into one directory.
  Documented, not silently assumed.
- Analytics health/staleness becomes observable (`/admin/analytics/health`)
  for the first time — Mode A had no equivalent, an intentional Phase 1a gap
  now closed for Mode B (Mode A stays without it; single-replica local state
  has nothing to be stale relative to).
