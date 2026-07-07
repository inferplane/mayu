# Phase 1b — Analytics Mode B: shared Postgres store + fenced aggregator

**Spec:** `docs/superpowers/specs/2026-06-26-admin-console-litellm-ux-redesign-design.md` §4.1/§4.6
**ADR:** `docs/decisions/ADR-015-analytics-mode-b-shared-store.md` — read it first; this
plan implements its Decision sections 1–6 verbatim. Do not re-derive the design here.
**Prior phase:** `docs/superpowers/plans/2026-06-29-console-phase1a-analytics-index.md` (Mode A, shipped)

## Scope

Ships Mode B: a real Postgres-backed shared analytics store, a fenced single-writer
aggregator that tails an operator-provided pre-aggregated audit directory, the
`/admin/analytics/health` + `/admin/analytics/rebuild` endpoints, config, capability
wiring, and gateway assembly. Out of scope (ADR-015 Alternatives): a cross-replica
audit collector, Redis-based fencing, incremental rollup counters.

## Global constraints (apply to every task)

- Go 1.25, `gofmt`-clean, `go vet`-clean. Errors wrapped with `%w`.
- Integer µUSD only — no float in cost/spend paths (existing project mandate).
- Secrets via `env:`/`file:` refs only — the Postgres DSN is never inline (§7,
  mirrors `api_key_ref`/ADR-013's `dsn_ref`).
- `pgx/v5` (pure Go, `github.com/jackc/pgx/v5`, no cgo) is the only new dependency —
  add via `go get` in Task 2, do not vendor a second Postgres driver.
- A live Postgres for integration tests is available via env var
  `INFERPLANE_TEST_PG_DSN` (e.g. `postgres://postgres:testpass@localhost:15432/inferplane_test?sslmode=disable`).
  Tests needing Postgres call `t.Skip` if that env var is unset (CI/dev without a
  Postgres instance must not fail — matches the zero-dependency-default acceptance
  criterion: the default build/test path never requires Postgres).
- Every new package gets a package comment; every exported type/func gets a doc
  comment (project convention — see `internal/analytics/index.go`'s existing style).

## Task 1: `analytics.Store` interface + Mode A `Health()`

Extracts the query-surface interface Mode A and Mode B both implement, with zero
behavior change to Mode A.

- Create: `internal/analytics/store.go`
- Modify: `internal/analytics/index.go`
- Test: `internal/analytics/store_test.go`

Exact contract (both Task 2's Postgres store and Task 6's gateway wiring depend on
this signature — do not deviate):

```go
package analytics

// Health reports Mode A/B freshness for /admin/analytics/health (ADR-015 §4).
type Health struct {
	Mode           string // "A" | "B"
	IsLeader       bool   // Mode A: always true (single-replica). Mode B: this replica's lease state.
	LeaseEpoch     int64  // Mode A: 0.
	LagSeconds     int64  // Mode A: always 0 (no ingestion lag for a local sink).
	LastIngestTS   string // RFC3339Nano, "" if never ingested.
	SegmentsTracked int   // Mode A: 0 (no segments — sink-fed).
}

// Store is the query surface analyticsapi depends on — Mode A (local SQLite) and
// Mode B (shared Postgres) both implement it; the gateway picks one per config.
type Store interface {
	Summary(SummaryQuery) (Summary, error)
	TimeSeries(TimeSeriesQuery) ([]DayPoint, error)
	Health() (Health, error)
}

// Rebuilder is implemented only by stores that support an operator-triggered
// rebuild (Mode B; Mode A's *Index does not implement this — analyticsapi
// type-asserts to decide 404 vs 204). Declared once here (not duplicated in
// analyticsapi) so a pgstore.Store signature drift is a compile error in
// pgstore itself, not something only caught by an integration test.
type Rebuilder interface {
	Rebuild(context.Context) error
}
```

Steps:
- [ ] Write `store_test.go`: a table-driven test asserting `*Index` satisfies `Store`
      via `var _ Store = (*Index)(nil)` (compile-time) + a runtime test that
      `Index.Health()` returns `{Mode:"A", IsLeader:true, LeaseEpoch:0, LagSeconds:0,
      SegmentsTracked:0}` and a non-empty `LastIngestTS` after at least one `Ingest`
      call, `""` before any.
- [ ] Add `Health` struct, `Store` interface, and `Rebuilder` interface to `store.go`.
- [ ] Add `func (ix *Index) Health() (Health, error)` to `index.go` — track the last
      ingested record's wall-clock time in a small `sync.Mutex`-guarded field on
      `Index` (set at the end of `ingest`), format as RFC3339Nano.
- [ ] Export `Billable`/`ModelOf`/`DayOf` from `index.go` (were `billable`/`modelOf`/
      `dayOf`; Task 2's `pgstore` package needs these exact same rules so Mode A/B
      classify records identically — export here, in the task that already owns and
      tests this file, rather than letting Task 2 touch `index.go` too).
- [ ] `go build ./... && go vet ./... && gofmt -l internal/analytics/` clean.
- [ ] `go test ./internal/analytics/... -race` green.

## Task 2: `internal/analytics/pgstore` — Postgres store + fenced aggregator

The Mode B backend: schema, idempotent upsert, touched-tuple rollup recompute, lease
fencing, segment tailing, and the `Store` query methods — one cohesive package
(ADR-015 §2/§3). Bundled as one task because the aggregator's ingest loop and the
store's transaction/fencing primitives are not decoupled by a stable public
interface; splitting them across two isolated-worktree implementers risks a
signature mismatch neither test suite would catch until integration. Depends on
Task 1's `Store`/`Health` types (import `internal/analytics`).

- Create: `internal/analytics/pgstore/pgstore.go`
- Create: `internal/analytics/pgstore/lease.go`
- Create: `internal/analytics/pgstore/aggregator.go`
- Test: `internal/analytics/pgstore/pgstore_test.go`
- Test: `internal/analytics/pgstore/lease_test.go`
- Test: `internal/analytics/pgstore/aggregator_test.go`

Exact contract (Task 6's gateway wiring depends on these signatures):

```go
package pgstore

// New opens (and ensureSchema's) a Mode B store against dsn. Pure-Go pgx/v5, no cgo.
func New(ctx context.Context, dsn string) (*Store, error)

// Store implements analytics.Store (Summary/TimeSeries/Health) plus the
// aggregator-only ingest/lease primitives below. Query methods are safe to call
// from any replica; ingest/lease primitives are only ever called by Aggregator.
type Store struct { /* db *pgxpool.Pool, instanceID string */ }

func (s *Store) Summary(analytics.SummaryQuery) (analytics.Summary, error)
func (s *Store) TimeSeries(analytics.TimeSeriesQuery) ([]analytics.DayPoint, error)
func (s *Store) Health() (analytics.Health, error)
func (s *Store) Close() error

var _ analytics.Store = (*Store)(nil)
var _ analytics.Rebuilder = (*Store)(nil) // compile-time — see ADR-015 §1 (round-1 finding 6)

// Rebuild truncates events/rollup_day/checkpoints — the operator-triggered
// recovery path (ADR-015 §6). Takes the SAME lease-row FOR UPDATE lock an
// ingest transaction's fencing check takes, so it can never interleave with
// a concurrently committing batch (ADR-015 §6, round-1 finding 4). The next
// poll tick re-tails every tracked segment from byte 0.
func (s *Store) Rebuild(ctx context.Context) error

// AggregatorConfig configures the tailing loop (ADR-015 §3).
type AggregatorConfig struct {
	AggregatedAuditDir string
	PollInterval       time.Duration // default 5s
	LeaseTTL           time.Duration // default 15s
	MaxLinesPerTick     int          // default 5000
}

// NewAggregator builds the tailing/fencing loop bound to store s.
func NewAggregator(s *Store, cfg AggregatorConfig) *Aggregator

// Run blocks until ctx is cancelled, polling at cfg.PollInterval: attempt lease
// acquire/renew (lease.go), and if leader, tail tracked segments and ingest
// batches atomically. Never returns a non-nil error for a lost-leadership event
// (that is normal steady-state, not a failure) — only for a fatal store error
// (e.g. the DB connection is permanently gone).
func (a *Aggregator) Run(ctx context.Context) error
```

Schema (`ensureSchema`, called from `New`, mirrors `internal/providerstore/sqlite.go`'s
`BEGIN`/`CREATE TABLE IF NOT EXISTS`/`COMMIT` pattern, using
`SELECT pg_advisory_lock(<fixed int64>)` / `pg_advisory_unlock` in place of SQLite's
file-level `BEGIN EXCLUSIVE` for cross-process migration serialization):

```sql
CREATE TABLE IF NOT EXISTS events (
  id TEXT PRIMARY KEY, day TEXT NOT NULL DEFAULT '', team TEXT NOT NULL DEFAULT '',
  model TEXT NOT NULL DEFAULT '', provider TEXT NOT NULL DEFAULT '',
  status INTEGER NOT NULL DEFAULT 0, input_tokens BIGINT NOT NULL DEFAULT 0,
  output_tokens BIGINT NOT NULL DEFAULT 0, cache_read BIGINT NOT NULL DEFAULT 0,
  cache_creation BIGINT NOT NULL DEFAULT 0, cost_micros BIGINT NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS events_day ON events(day);
CREATE INDEX IF NOT EXISTS events_team ON events(team);
CREATE TABLE IF NOT EXISTS checkpoints (
  segment TEXT PRIMARY KEY, byte_offset BIGINT NOT NULL DEFAULT 0,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS lease (
  id TEXT PRIMARY KEY, holder TEXT NOT NULL, epoch BIGINT NOT NULL, expires_at TIMESTAMPTZ NOT NULL
);
CREATE TABLE IF NOT EXISTS rollup_day (
  day TEXT NOT NULL, team TEXT NOT NULL, model TEXT NOT NULL,
  input_tokens BIGINT NOT NULL DEFAULT 0, output_tokens BIGINT NOT NULL DEFAULT 0,
  cost_micros BIGINT NOT NULL DEFAULT 0, request_count BIGINT NOT NULL DEFAULT 0,
  PRIMARY KEY (day, team, model)
);
```

Steps:
- [ ] `go get github.com/jackc/pgx/v5` (+ `pgxpool`); `go mod tidy`.
- [ ] Write `pgstore_test.go` (skips if `INFERPLANE_TEST_PG_DSN` unset): `New` creates
      the schema idempotently (calling `New` twice against the same DSN doesn't
      error); a direct `INSERT ... ON CONFLICT (id) DO UPDATE` ingest helper (call it
      `upsertEvent`, unexported, exercised via a test-only exported wrapper or by
      testing `Summary`/`TimeSeries` after a manual `db.Exec` seed) correctly updates
      an existing row's `cost_micros` on a same-ID re-ingest (proves upsert, not
      insert-or-ignore — the Mode A→B behavior change ADR-015 §2 calls for);
      `Summary`/`TimeSeries` read from `rollup_day`, not a live `events` `GROUP BY`
      (assert by seeding `rollup_day` directly with a value `events` alone wouldn't
      produce, and confirming the query returns it); `Health()` reflects
      `checkpoints`/`lease` state; `Rebuild` truncates all three tables AND bumps
      `lease.epoch` by exactly 1 without changing `holder` (round-2 finding 1 —
      assert the epoch delta directly, not just via the aggregator-level race
      test in `aggregator_test.go`); a
      **crash-recovery idempotency test** (round-1 kiro-cli finding, defense in
      depth): ingest a batch, close the `Store`, open a fresh `Store` against the
      same DSN (simulating a process restart), re-ingest the SAME batch — assert
      `events`/`rollup_day` are byte-for-byte identical to a single ingest (proves
      the atomic-transaction claim, not just asserts it).
- [ ] Implement `pgstore.go`: `Store`, `New`/`ensureSchema`, `Summary`/`TimeSeries`
      (query `rollup_day`, same `GROUP BY`/ordering semantics as Mode A's
      `index.go:194`/`:241` — reuse those SQL shapes against the new table), `Health`,
      `Close`, `Rebuild` (§ below). An internal `ingestBatch(ctx, tx, holder string,
      epoch int64, records []audit.Record, checkpointUpdates map[string]int64) error`
      (checkpoints are a MAP — segment name → new byte offset — not a single value,
      since one tick's batch can span multiple segments; round-1 finding 5) performs,
      in order, inside the ONE transaction the caller (`aggregator.go`) began: 1) the
      fencing lock — `SELECT epoch FROM lease WHERE id='mode_b_aggregator' AND
      holder=$1 FOR UPDATE`; if the returned epoch != the caller's `epoch` param,
      return a sentinel `errFenced` and let the caller roll back (round-1 finding 1:
      `FOR UPDATE` here, not a bare `SELECT`, is what makes this actually exclusive —
      it blocks any concurrent lease acquire/renew/Rebuild until this transaction
      ends); 2) for each billable record (`analytics.Billable`/`ModelOf`/`DayOf`,
      exported by Task 1 — no `internal/analytics` file changes needed here) an
      `events` upsert; 3) `INSERT INTO rollup_day (...) SELECT day, team, model,
      SUM(...), COUNT(*) FROM events WHERE (day,team,model) IN (<touched tuples from
      this batch>) GROUP BY day, team, model ON CONFLICT (day,team,model) DO UPDATE
      SET ... = excluded.*`; 4) one `checkpoints` upsert per entry in
      `checkpointUpdates`.
- [ ] `Rebuild(ctx)`: one transaction — `SELECT epoch FROM lease WHERE
      id='mode_b_aggregator' FOR UPDATE` (same row lock as `ingestBatch`'s fencing
      step; Postgres's lock queue means this and any in-flight `ingestBatch`
      transaction can never interleave — round-1 finding 4), `UPDATE lease SET
      epoch = epoch + 1 WHERE id='mode_b_aggregator'` (round-2 finding 1: bumping
      epoch here, even with `holder` unchanged, is what invalidates a tick that
      already read a NOW-stale checkpoint offset before Rebuild started — the
      row lock alone only excludes concurrent COMMITS, not stale in-memory state a
      tick captured before Rebuild ran; that tick's `ingestBatch` fencing check
      will now see a changed epoch and abort, forcing a clean re-read), then
      `TRUNCATE events, rollup_day, checkpoints`, then commit.
- [ ] Write `lease_test.go` (skips if DSN unset): two `Store`s (same DSN, different
      `instanceID`) racing `tryAcquireLease` — exactly one wins per call; the loser's
      subsequent fenced `ingestBatch` call (passing the STALE epoch it thinks it
      holds) is rejected with `errFenced`; after the winner's lease naturally
      expires (short TTL in the test), the other can acquire with a strictly higher
      epoch; **renewal by the SAME holder does NOT bump the epoch** — assert epoch
      unchanged across 2 consecutive renewals by the same `instanceID` (this is the
      exact case round-1 found broken: the naive `epoch=epoch+1` SQL bumped on every
      renewal, contradicting this very assertion); a fencing check racing a
      concurrent `tryAcquireLease` (both started, one commits first) — the loser
      of the race sees the FOR UPDATE lock block until the winner commits, then
      reads the winner's new epoch (proves the lock, not just the CASE logic, is
      what closes the race).
- [ ] Implement `lease.go`: `tryAcquireLease(ctx, db, holder string, ttl
      time.Duration) (epoch int64, ok bool, err error)` (the SQL `$2` param is
      `int64(ttl.Seconds())` — a plain number, never a precomputed timestamp) —
      its OWN transaction (commits
      immediately, does not hold `FOR UPDATE` across an ingest batch): `SELECT
      epoch, holder FROM lease WHERE id='mode_b_aggregator' FOR UPDATE`, then
      `UPDATE lease SET holder=$1, epoch = CASE WHEN holder=$1 THEN epoch ELSE
      epoch+1 END, expires_at = now() + ($2 * interval '1 second') WHERE
      id='mode_b_aggregator' AND (expires_at < now() OR holder=$1)` (the `CASE` is
      the round-1 fix — epoch bumps ONLY on an actual handover, never a
      same-holder renewal; `now() + ($2 * interval ...)` is the round-2 fix —
      `$2` is a plain seconds count, NOT a precomputed Go `time.Time`/timestamp,
      so the expiry is computed entirely by the database's own clock, immune to
      replica clock skew) + `INSERT ... ON CONFLICT DO UPDATE ... WHERE ...` for
      the first-ever row; commit; return the resulting epoch and whether `$1` is
      now the holder. The CALLER (aggregator.go's `tick`) holds onto this
      returned epoch for the rest of the tick — it's what `ingestBatch`'s fencing
      check must match EXACTLY. `errFenced` (sentinel error type) lives here too,
      returned by `ingestBatch`'s fencing check in `pgstore.go`.
- [ ] Write `aggregator_test.go` (skips if DSN unset): given a temp dir with 2 fixture
      JSONL segment files (one fully consumed by a pre-seeded checkpoint, one fresh),
      one `Run` tick (call the tick function directly, don't rely on the poll-sleep
      loop in tests) ingests only the fresh segment's new lines, advances its
      checkpoint, and a second identical tick is a no-op (idempotent — checkpoint
      already at EOF); a segment with a crash-truncated trailing line (no `\n`) does
      NOT ingest that partial line AND leaves `checkpoints.byte_offset` pinned to
      the byte position immediately before the incomplete bytes (round-1 finding 7
      — assert the exact offset, not just "record not ingested"; then append the
      rest of the line + a trailing `\n` and run one more tick — assert the
      now-complete record IS ingested, proving nothing was permanently lost); a
      batch spanning BOTH fixture segments in one tick advances BOTH their
      checkpoints in the one transaction (proves the `map[string]int64` shape,
      round-1 finding 5); **a segment containing ONLY malformed/unparseable
      complete lines up to a small `MaxLinesPerTick`, followed by a valid line
      beyond that budget** — the first tick advances the checkpoint past the
      malformed lines (round-2 finding 2: the checkpoint MUST advance even
      though zero billable records were produced this tick — assert
      `checkpoints.byte_offset` moved and `events` stayed empty for that tick),
      and a second tick then reaches and ingests the valid line (proves no
      permanent starvation); **a `Rebuild` call racing a tick that already read
      a stale checkpoint before the rebuild** (round-2 finding 1: seed a
      checkpoint at a nonzero offset, capture the tick's acquired epoch, call
      `Rebuild`, THEN run `ingestBatch` with the pre-rebuild epoch/checkpoint —
      assert it returns `errFenced` rather than silently writing a
      stale-based checkpoint into the freshly truncated table); when NOT the
      lease-holder (simulate by pre-seeding a live lease held by a different
      `holder`), a tick does nothing and returns no error (graceful non-leader
      idle, not an error state).
- [ ] Implement `aggregator.go`: `Aggregator`, `NewAggregator`, `Run` (poll loop:
      sleep `PollInterval`, call an unexported `tick(ctx)` that: 1) calls
      `tryAcquireLease`, keeping the returned epoch for the rest of this tick; 2)
      if not leader, returns nil; 3) if leader, lists `AggregatedAuditDir`, sorts
      lexically, for each segment reads from its checkpoint offset up to
      `MaxLinesPerTick` complete lines total across all segments in this tick
      (tracking each segment's new offset in a `map[string]int64`, stopping a
      segment's read exactly at the start of an incomplete trailing line),
      unmarshal each line as `audit.Record` (skip malformed lines, matching Mode
      A's `Replay`), and **whenever `checkpointUpdates` is non-empty — regardless
      of whether any record parsed as billable (round-2 finding 2)** — runs
      `ingestBatch` (passing the tick's captured epoch) in one transaction — on
      `errFenced`, roll back and return nil (not an error — just means
      leadership was lost, or a Rebuild ran, since the epoch was captured; try
      again next tick, which will see fresh state)).
- [ ] `go build ./... && go vet ./... && gofmt -l internal/analytics/pgstore/` clean.
- [ ] `go test ./internal/analytics/pgstore/... -race` green (will `SKIP`, not fail,
      without `INFERPLANE_TEST_PG_DSN` — confirm both: run once with the env var set
      against the local test Postgres, and once unset, both green).

## Task 3: config — `analytics.mode_b` block

- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

Steps:
- [ ] Write tests: a config with `analytics.mode_b.dsn_ref.env` + `aggregated_audit_dir`
      parses into a new `AnalyticsModeB` struct on `Config.Analytics`; an inline `dsn`
      key (not `dsn_ref`) is rejected at load with a message that never echoes a
      would-be value (mirrors the existing inline-`api_key` rejection test style);
      `dsn_ref` goes through the same `ValidateSecretRef` shape check as
      `api_key_ref` (a non-shaped env name is rejected); absent `mode_b` block leaves
      the existing Mode A/off behavior untouched (regression-check the existing
      `TestResolveAnalytics`-style test still passes unmodified).
- [ ] Add `AnalyticsModeB struct { DSNRef SecretRef; AggregatedAuditDir string;
      PollInterval, LeaseTTL time.Duration }` (JSON tags: `dsn_ref`,
      `aggregated_audit_dir`, `poll_interval`, `lease_ttl` — durations as Go duration
      strings, matching the project's existing `time.Duration` JSON convention if one
      exists; check `drain_grace`'s field for the pattern to copy) to the `Analytics`
      config struct; probe-reject an inline `dsn` key the same way `ParseProviderWrite`
      probes `api_key` (§7); validate `DSNRef` via `config.ValidateSecretRef` inside
      `ResolveAnalytics` (or wherever provider/analytics refs are resolved today) —
      resolve to a plaintext DSN string held only in-memory, never logged.
- [ ] `go build ./... && go vet ./... && gofmt -l internal/config/` clean.
- [ ] `go test ./internal/config/... -race` green.

## Task 4: `analyticsapi` — health + rebuild endpoints

- Modify: `internal/server/analyticsapi/analyticsapi.go`
- Modify: `internal/server/server.go`
- Test: `internal/server/analyticsapi/analyticsapi_test.go`

Steps:
- [ ] Write tests: `GET /admin/analytics/health` (full-admin only, mirrors the
      existing summary/timeseries authz test pattern) returns the querier's
      `Health()` as JSON; `POST /admin/analytics/rebuild` calls the querier's
      `analytics.Rebuild(ctx) error` and returns 204, or 405 if the configured
      querier doesn't implement `analytics.Rebuilder` (Mode A has no rebuild
      endpoint — a type assertion at request time, not an interface requirement on
      `Querier`, so Mode A's `*Index` needn't grow a no-op `Rebuild`).
- [ ] Extend `Querier` interface with `Health() (analytics.Health, error)` (breaking
      change to the interface — Mode A's `*Index` already gained this in Task 1, so
      no Mode A code changes here); add `HealthHandler`, `RebuildHandler` (the latter
      does a runtime `if r, ok := querier.(analytics.Rebuilder); ok { ... } else {
      405 }` — `analytics.Rebuilder` is Task 1's interface, imported here, NOT
      redeclared: round-1 finding 6 was exactly this package defining its own copy
      with no compile-time link to `pgstore.Store`'s actual `Rebuild` signature).
- [ ] Mount both in `internal/server/server.go` alongside the existing
      `/admin/analytics/*` routes (same `AdminAuth`/`requireAdmin` gating) —
      declared as a Modify target above since this was a round-1 finding 8 gap
      (the file wasn't listed even though the steps always required touching it).
- [ ] `go build ./... && go vet ./... && gofmt -l internal/server/analyticsapi/ internal/server/` clean.
- [ ] `go test ./internal/server/analyticsapi/... ./internal/server/... -race` green.

## Task 5: capabilities — `analytics_index: "B"`

- Modify: `internal/server/configapi/capabilities.go`
- Test: `internal/server/configapi/capabilities_test.go`

Steps:
- [ ] Write a test: given a capabilities projection input indicating Mode B is
      configured (whatever field the gateway assembly will pass in — check how
      `analytics_index: "A"` is currently signaled from `cmd/inferplane` into this
      package today and add the parallel `"B"` case the same way), the endpoint
      returns `"analytics_index": "B"`.
- [ ] Wire the `"B"` enum value (already declared per the earlier Phase 1a
      exploration — confirm and use it, don't redeclare).
- [ ] `go build ./... && go vet ./... && gofmt -l internal/server/configapi/` clean.
- [ ] `go test ./internal/server/configapi/... -race` green.

## Task 6: gateway wiring — pick Store, run the aggregator, mount everything

The integration task: depends on Tasks 1–5's concrete types (not just their test
contracts), so it must run after all of them land — it is listed last and touches
only `cmd/inferplane/gateway.go` (+ its own new test file), which no other task
touches, so run it strictly after Task 5 completes even though the harness's
file-overlap wave check alone wouldn't force that ordering (see plan header:
`harness.parallel_tasks` is set to `1` for this run specifically so tasks execute
in this listed order, not by automatic wave-grouping — do not re-enable
parallelism for this plan without re-checking every task's real dependencies).

- Modify: `cmd/inferplane/gateway.go`
- Test: `cmd/inferplane/analytics_mode_b_test.go`

Steps:
- [ ] Write an integration test (skips if `INFERPLANE_TEST_PG_DSN` unset): boot a
      gateway with `analytics.mode_b` configured pointing at the test Postgres +
      a temp aggregated-audit-dir seeded with one fixture segment; after letting the
      aggregator run (poll `GET /admin/analytics/health` until `last_ingest_ts` is
      non-empty, bounded by a short timeout — `tick` is deliberately unexported per
      Task 2, so this test only ever observes the aggregator through the public
      HTTP surface, never by reaching into `pgstore` internals), `GET
      /admin/analytics/summary` reflects the fixture's billable records; `GET
      /admin/capabilities` reports `analytics_index: "B"`; on gateway shutdown the
      aggregator's `Run` goroutine exits cleanly (no goroutine leak — reuse whatever
      leak-check pattern existing gateway tests use, if any, else just assert
      `g.serve`'s shutdown returns within the test's timeout).
- [ ] In `newGateway`: when `cfg.Analytics.ModeB != nil`, open a `pgstore.Store`
      instead of (or in addition to — see ADR-015 §1, Mode A stays wired but simply
      unused) the local `*analytics.Index`; hand the `pgstore.Store` to
      `analyticsapi` as the `Querier` (it already satisfies `Querier` structurally:
      `Summary`/`TimeSeries`/`Health`, plus `Rebuilder` via `Rebuild`); construct a
      `pgstore.Aggregator` and start `Run` on the gateway's lifecycle context in
      `serve` (alongside the existing `reloadWorker`/`anchorWorker` goroutines — same
      "cancel + wait for done channel" shutdown pattern already used there); close
      the `pgstore.Store` in `serve`'s shutdown defer chain, same position `g.pstore`
      already occupies (best-effort, non-required).
- [ ] Wire the capabilities `analytics_index` signal to `"B"` when Mode B is active
      (Task 5's projection).
- [ ] `CGO_ENABLED=0 go build -trimpath ./...` succeeds (static build unaffected by
      the new pure-Go `pgx` dependency).
- [ ] `go build ./... && go vet ./... && gofmt -l .` clean (whole repo).
- [ ] `go test ./... -race` green (with `INFERPLANE_TEST_PG_DSN` set against the
      local test Postgres) AND green a second time with it unset (zero-dependency
      default path — confirms Mode B code is fully inert without config, per ADR-015
      §5's acceptance criterion).
- [ ] `bash tests/run-all.sh` green.

## Out of scope (explicit, per ADR-015)

Cross-replica audit collector (operator brings their own aggregation); Redis-based
fencing; incremental rollup counters; `chain_verification_status` in the health
payload; auto-rebuild (rebuild is operator-triggered only, this phase); UI changes
(the `/admin/analytics/health` staleness banner is a follow-up front-end task, not
part of this backend-only plan — matches Phase 1a's own backend-first split).
