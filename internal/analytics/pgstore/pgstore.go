// Package pgstore implements Analytics Mode B's shared Postgres store.
package pgstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/inferplane/inferplane/internal/analytics"
	"github.com/inferplane/inferplane/internal/audit"
)

const schemaLockKey int64 = 847001

const schema = `
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
ALTER TABLE rollup_day ADD COLUMN IF NOT EXISTS cache_read BIGINT NOT NULL DEFAULT 0;
ALTER TABLE rollup_day ADD COLUMN IF NOT EXISTS cache_creation BIGINT NOT NULL DEFAULT 0;`

// Store implements analytics.Store against a shared Postgres database.
type Store struct {
	db         *pgxpool.Pool
	instanceID string
}

var _ analytics.Store = (*Store)(nil)
var _ analytics.Rebuilder = (*Store)(nil)

// queryTimeout bounds every Mode B query server-side (Postgres
// statement_timeout) against a SLOW query — a statement that reaches Postgres
// but runs too long. It does NOT bound network-level unreachability (a
// partitioned/down DB host hits the OS TCP timeout, ~2 minutes on Linux,
// before Postgres ever sees the statement); analytics.Store's query methods
// take no context (matching Mode A's in-process SQLite, which has no
// analogous risk), so this is the seam available to bound the risk this
// package's queries CAN hit without widening that shared interface.
const queryTimeout = "10000" // ms

// New opens a Mode B Postgres store and ensures its schema exists.
func New(ctx context.Context, dsn string) (*Store, error) {
	// Parse the DSN as its OWN step, deliberately not wrapped with %w: pgx's
	// own parse-failure error embeds the connection string verbatim (it can
	// carry a password), and this repo's convention (§7 gate C1) is that a
	// secret-bearing value never appears in a returned/logged error. A
	// connection-level failure from a successfully-parsed config (auth,
	// network, ...) does not embed the DSN, so it's safe to wrap normally.
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("open analytics postgres store: invalid dsn (check dsn_ref resolves to a valid postgres:// connection string)")
	}
	cfg.ConnConfig.RuntimeParams["statement_timeout"] = queryTimeout
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open analytics postgres store: %w", err)
	}
	s := &Store{db: db}
	if err := db.Ping(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping analytics postgres store: %w", err)
	}
	if err := s.ensureSchema(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the underlying Postgres connection pool.
func (s *Store) Close() error {
	s.db.Close()
	return nil
}

// ensureSchema serializes migration across replicas via a Postgres session-
// level advisory lock. pg_advisory_lock/unlock are tied to the SPECIFIC
// connection that acquired them — pgxpool.Exec/Begin each independently check
// out a (possibly different) pooled connection, so calling lock/tx/unlock as
// three separate pool operations does NOT actually serialize anything (the
// unlock call would run on a random connection, almost never the one holding
// the lock, leaving it held until that connection is eventually recycled).
// Acquire ONE connection and run lock+migration+unlock all through it.
func (s *Store) ensureSchema(ctx context.Context) (retErr error) {
	conn, err := s.db.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire analytics schema migration connection: %w", err)
	}
	defer conn.Release()

	// The pool-wide statement_timeout (New) would otherwise apply to
	// pg_advisory_lock too — under a multi-replica startup storm one replica
	// can legitimately wait longer than that for the lock, so it must be
	// disabled for this connection's session before locking, and explicitly
	// RESTORED before the connection goes back to the pool (a bare `SET`,
	// unlike `SET LOCAL`, persists for the connection's whole session —
	// leaving it disabled would silently defeat the timeout for whichever
	// later query happens to reuse this same pooled connection). The restore
	// error is NOT swallowed: if it fails, ensureSchema fails too — this
	// connection cannot be trusted back into the pool with no server-side
	// time bound, so failing loudly beats returning success blind to that.
	// `queryTimeout` is a compile-time numeric constant (never user input),
	// so inlining it into the SQL text is safe — SET's grammar doesn't accept
	// a bound parameter here the way SELECT/INSERT do.
	if _, err := conn.Exec(ctx, `SET statement_timeout = 0`); err != nil {
		return fmt.Errorf("disable timeout for analytics schema migration: %w", err)
	}
	defer func() {
		if _, err := conn.Exec(ctx, `SET statement_timeout = `+queryTimeout); err != nil && retErr == nil {
			retErr = fmt.Errorf("restore analytics query timeout: %w", err)
		}
	}()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, schemaLockKey); err != nil {
		return fmt.Errorf("lock analytics schema migration: %w", err)
	}
	defer conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, schemaLockKey)

	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin analytics schema migration: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, schema); err != nil {
		return fmt.Errorf("create analytics schema: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit analytics schema migration: %w", err)
	}
	return nil
}

// Summary returns aggregate usage from the derived rollup_day table.
func (s *Store) Summary(q analytics.SummaryQuery) (analytics.Summary, error) {
	out := analytics.Summary{ByTeam: []analytics.TeamRow{}, ByModel: []analytics.ModelRow{}}
	where, args := pgDayWhere(q.SinceDay, q.UntilDay)

	row := s.db.QueryRow(context.Background(), `SELECT COALESCE(SUM(request_count),0),
		COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
		COALESCE(SUM(cache_read),0), COALESCE(SUM(cache_creation),0), COALESCE(SUM(cost_micros),0)
		FROM rollup_day WHERE 1=1`+where, args...)
	if err := row.Scan(&out.Totals.Requests, &out.Totals.InputTokens, &out.Totals.OutputTokens,
		&out.Totals.CacheReadTokens, &out.Totals.CacheCreationTokens, &out.Totals.CostMicros); err != nil {
		return out, fmt.Errorf("query analytics summary totals: %w", err)
	}

	teamRows, err := s.db.Query(context.Background(), `SELECT team, COALESCE(SUM(request_count),0),
		COALESCE(SUM(cost_micros),0) FROM rollup_day WHERE 1=1`+where+`
		GROUP BY team ORDER BY SUM(cost_micros) DESC`, args...)
	if err != nil {
		return out, fmt.Errorf("query analytics summary teams: %w", err)
	}
	for teamRows.Next() {
		var r analytics.TeamRow
		if err := teamRows.Scan(&r.Team, &r.Requests, &r.CostMicros); err != nil {
			teamRows.Close()
			return out, fmt.Errorf("scan analytics summary team: %w", err)
		}
		out.ByTeam = append(out.ByTeam, r)
	}
	if err := teamRows.Err(); err != nil {
		teamRows.Close()
		return out, fmt.Errorf("iterate analytics summary teams: %w", err)
	}
	teamRows.Close()

	modelRows, err := s.db.Query(context.Background(), `SELECT model, COALESCE(SUM(request_count),0),
		COALESCE(SUM(cost_micros),0) FROM rollup_day WHERE 1=1`+where+`
		GROUP BY model ORDER BY SUM(cost_micros) DESC`, args...)
	if err != nil {
		return out, fmt.Errorf("query analytics summary models: %w", err)
	}
	defer modelRows.Close()
	for modelRows.Next() {
		var r analytics.ModelRow
		if err := modelRows.Scan(&r.Model, &r.Requests, &r.CostMicros); err != nil {
			return out, fmt.Errorf("scan analytics summary model: %w", err)
		}
		out.ByModel = append(out.ByModel, r)
	}
	if err := modelRows.Err(); err != nil {
		return out, fmt.Errorf("iterate analytics summary models: %w", err)
	}
	return out, nil
}

// TimeSeries returns daily usage points from the derived rollup_day table.
func (s *Store) TimeSeries(q analytics.TimeSeriesQuery) ([]analytics.DayPoint, error) {
	days := q.Days
	if days <= 0 {
		days = 30
	}
	if days > 366 {
		days = 366
	}
	rows, err := s.db.Query(context.Background(), `SELECT day, COALESCE(SUM(request_count),0),
		COALESCE(SUM(cost_micros),0), COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0)
		FROM rollup_day
		WHERE day >= to_char((current_date - ($1::int * interval '1 day')), 'YYYY-MM-DD')
		GROUP BY day ORDER BY day DESC LIMIT $2`, days, days)
	if err != nil {
		return nil, fmt.Errorf("query analytics time series: %w", err)
	}
	defer rows.Close()
	out := []analytics.DayPoint{}
	for rows.Next() {
		var p analytics.DayPoint
		if err := rows.Scan(&p.Day, &p.Requests, &p.CostMicros, &p.InputTokens, &p.OutputTokens); err != nil {
			return nil, fmt.Errorf("scan analytics time series point: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate analytics time series: %w", err)
	}
	return out, nil
}

// Health reports Mode B lease and ingestion freshness.
func (s *Store) Health() (analytics.Health, error) {
	h := analytics.Health{Mode: "B"}
	ctx := context.Background()

	// is_leader is computed IN SQL (expires_at > now()) rather than comparing
	// the DB's TIMESTAMPTZ against the app's time.Now() — PR review: comparing
	// across the two clocks is exactly the caller-clock-vs-DB-clock mismatch
	// ADR-015 §3 round-2 already fixed for lease acquire/renew; doing it here
	// too keeps every liveness decision on the database's single clock.
	var holder string
	var isLive bool
	err := s.db.QueryRow(ctx, `SELECT holder, epoch, expires_at > now() FROM lease WHERE id=$1`, leaseID).
		Scan(&holder, &h.LeaseEpoch, &isLive)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return h, fmt.Errorf("query analytics lease health: %w", err)
	}
	if err == nil {
		h.IsLeader = holder != "" && holder == s.instanceID && isLive
	}

	var last *time.Time
	err = s.db.QueryRow(ctx, `SELECT COUNT(*), max(updated_at),
		COALESCE(EXTRACT(EPOCH FROM (now() - max(updated_at)))::bigint, 0)
		FROM checkpoints`).Scan(&h.SegmentsTracked, &last, &h.LagSeconds)
	if err != nil {
		return h, fmt.Errorf("query analytics checkpoint health: %w", err)
	}
	if last != nil {
		h.LastIngestTS = last.UTC().Format(time.RFC3339Nano)
	}
	return h, nil
}

// Rebuild truncates derived analytics state and bumps the lease epoch when a
// lease row exists, invalidating any tick that captured a pre-rebuild epoch.
func (s *Store) Rebuild(ctx context.Context) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin analytics rebuild: %w", err)
	}
	defer tx.Rollback(ctx)

	var epoch int64
	err = tx.QueryRow(ctx, `SELECT epoch FROM lease WHERE id=$1 FOR UPDATE`, leaseID).Scan(&epoch)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("lock lease for analytics rebuild: %w", err)
	}
	if err == nil {
		if _, err := tx.Exec(ctx, `UPDATE lease SET epoch=epoch+1 WHERE id=$1`, leaseID); err != nil {
			return fmt.Errorf("bump analytics rebuild epoch: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `TRUNCATE events, rollup_day, checkpoints`); err != nil {
		return fmt.Errorf("truncate analytics rebuild tables: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit analytics rebuild: %w", err)
	}
	return nil
}

func pgDayWhere(since, until string) (string, []any) {
	var b strings.Builder
	args := []any{}
	if since != "" {
		args = append(args, since)
		fmt.Fprintf(&b, " AND day >= $%d", len(args))
	}
	if until != "" {
		args = append(args, until)
		fmt.Fprintf(&b, " AND day <= $%d", len(args))
	}
	return b.String(), args
}

func (s *Store) ingestBatch(ctx context.Context, holder string, epoch int64, records []audit.Record, checkpointUpdates map[string]int64) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin analytics ingest batch: %w", err)
	}
	defer tx.Rollback(ctx)

	if err := fenceIngest(ctx, tx, holder, epoch); err != nil {
		return err
	}

	touched := map[rollupKey]struct{}{}
	for _, r := range records {
		if !analytics.Billable(r) {
			continue
		}
		key := rollupKey{day: analytics.DayOf(r.TS), team: r.Principal.Team, model: analytics.ModelOf(r)}
		if err := upsertEvent(ctx, tx, r, key); err != nil {
			return fmt.Errorf("upsert analytics event %q: %w", r.ID, err)
		}
		touched[key] = struct{}{}
	}
	for key := range touched {
		if err := recomputeRollup(ctx, tx, key); err != nil {
			return fmt.Errorf("recompute analytics rollup: %w", err)
		}
	}
	for segment, offset := range checkpointUpdates {
		if _, err := tx.Exec(ctx, `INSERT INTO checkpoints(segment, byte_offset, updated_at)
			VALUES($1, $2, now())
			ON CONFLICT(segment) DO UPDATE
			SET byte_offset=excluded.byte_offset, updated_at=now()`, segment, offset); err != nil {
			return fmt.Errorf("upsert analytics checkpoint %q: %w", segment, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit analytics ingest batch: %w", err)
	}
	return nil
}

// fenceIngest requires an EXISTING lease row matching holder+epoch —
// tryAcquireLease is the only place a lease row is ever created (using the
// real configured TTL); fenceIngest bootstrapping its own row on ErrNoRows
// (as an earlier version did, with a hardcoded TTL that ignored config) was
// dead in the real aggregator flow (tick always calls tryAcquireLease first)
// and just risked drifting from tryAcquireLease's own TTL handling — removed
// rather than fixed, since there was nothing this path legitimately needed
// to do beyond what tryAcquireLease already does.
func fenceIngest(ctx context.Context, tx pgx.Tx, holder string, epoch int64) error {
	var got int64
	err := tx.QueryRow(ctx, `SELECT epoch FROM lease WHERE id=$1 AND holder=$2 FOR UPDATE`, leaseID, holder).Scan(&got)
	if errors.Is(err, pgx.ErrNoRows) {
		return errFenced
	}
	if err != nil {
		return fmt.Errorf("fence analytics ingest batch: %w", err)
	}
	if got != epoch {
		return errFenced
	}
	return nil
}

type rollupKey struct {
	day   string
	team  string
	model string
}

func upsertEvent(ctx context.Context, tx pgx.Tx, r audit.Record, key rollupKey) error {
	var in, out, cacheRead, cacheCreate int64
	if r.Usage != nil {
		in = r.Usage.InputTokens
		out = r.Usage.OutputTokens
		cacheRead = r.Usage.CacheReadInputTokens
		cacheCreate = r.Usage.CacheCreationInputTokens
	}
	status := 0
	if r.Outcome != nil {
		status = r.Outcome.Status
	}
	_, err := tx.Exec(ctx, `INSERT INTO events(id,day,team,model,provider,status,input_tokens,output_tokens,cache_read,cache_creation,cost_micros)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		ON CONFLICT(id) DO UPDATE SET
			day=excluded.day, team=excluded.team, model=excluded.model, provider=excluded.provider,
			status=excluded.status, input_tokens=excluded.input_tokens, output_tokens=excluded.output_tokens,
			cache_read=excluded.cache_read, cache_creation=excluded.cache_creation, cost_micros=excluded.cost_micros`,
		r.ID, key.day, key.team, key.model, r.Request.Provider, status, in, out, cacheRead, cacheCreate, r.Cost.AmountUSDMicros)
	return err
}

func recomputeRollup(ctx context.Context, tx pgx.Tx, key rollupKey) error {
	_, err := tx.Exec(ctx, `INSERT INTO rollup_day(day, team, model, input_tokens, output_tokens, cache_read, cache_creation, cost_micros, request_count)
		SELECT day, team, model, COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
			COALESCE(SUM(cache_read),0), COALESCE(SUM(cache_creation),0),
			COALESCE(SUM(cost_micros),0), COUNT(*)
		FROM events
		WHERE day=$1 AND team=$2 AND model=$3
		GROUP BY day, team, model
		ON CONFLICT(day, team, model) DO UPDATE SET
			input_tokens=excluded.input_tokens,
			output_tokens=excluded.output_tokens,
			cache_read=excluded.cache_read,
			cache_creation=excluded.cache_creation,
			cost_micros=excluded.cost_micros,
			request_count=excluded.request_count`, key.day, key.team, key.model)
	return err
}
