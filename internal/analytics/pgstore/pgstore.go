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
);`

// Store implements analytics.Store against a shared Postgres database.
type Store struct {
	db         *pgxpool.Pool
	instanceID string
}

var _ analytics.Store = (*Store)(nil)
var _ analytics.Rebuilder = (*Store)(nil)

// New opens a Mode B Postgres store and ensures its schema exists.
func New(ctx context.Context, dsn string) (*Store, error) {
	db, err := pgxpool.New(ctx, dsn)
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

func (s *Store) ensureSchema(ctx context.Context) error {
	if _, err := s.db.Exec(ctx, `SELECT pg_advisory_lock($1)`, schemaLockKey); err != nil {
		return fmt.Errorf("lock analytics schema migration: %w", err)
	}
	defer s.db.Exec(ctx, `SELECT pg_advisory_unlock($1)`, schemaLockKey)

	tx, err := s.db.Begin(ctx)
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
		COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0), COALESCE(SUM(cost_micros),0)
		FROM rollup_day WHERE 1=1`+where, args...)
	if err := row.Scan(&out.Totals.Requests, &out.Totals.InputTokens, &out.Totals.OutputTokens, &out.Totals.CostMicros); err != nil {
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

	var expiresAt time.Time
	err := s.db.QueryRow(ctx, `SELECT holder, epoch, expires_at FROM lease WHERE id=$1`, leaseID).
		Scan(new(string), &h.LeaseEpoch, &expiresAt)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return h, fmt.Errorf("query analytics lease health: %w", err)
	}
	if err == nil && !expiresAt.IsZero() {
		var holder string
		if err := s.db.QueryRow(ctx, `SELECT holder FROM lease WHERE id=$1`, leaseID).Scan(&holder); err != nil {
			return h, fmt.Errorf("query analytics lease holder: %w", err)
		}
		h.IsLeader = holder != "" && holder == s.instanceID && time.Now().Before(expiresAt)
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

func fenceIngest(ctx context.Context, tx pgx.Tx, holder string, epoch int64) error {
	var got int64
	err := tx.QueryRow(ctx, `SELECT epoch FROM lease WHERE id=$1 AND holder=$2 FOR UPDATE`, leaseID, holder).Scan(&got)
	if errors.Is(err, pgx.ErrNoRows) {
		tag, insertErr := tx.Exec(ctx, `INSERT INTO lease(id, holder, epoch, expires_at)
			VALUES($1, $2, $3, now() + (15 * interval '1 second'))
			ON CONFLICT (id) DO NOTHING`, leaseID, holder, epoch)
		if insertErr != nil {
			return fmt.Errorf("bootstrap analytics lease: %w", insertErr)
		}
		if tag.RowsAffected() == 1 {
			return nil
		}
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
	_, err := tx.Exec(ctx, `INSERT INTO rollup_day(day, team, model, input_tokens, output_tokens, cost_micros, request_count)
		SELECT day, team, model, COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
			COALESCE(SUM(cost_micros),0), COUNT(*)
		FROM events
		WHERE day=$1 AND team=$2 AND model=$3
		GROUP BY day, team, model
		ON CONFLICT(day, team, model) DO UPDATE SET
			input_tokens=excluded.input_tokens,
			output_tokens=excluded.output_tokens,
			cost_micros=excluded.cost_micros,
			request_count=excluded.request_count`, key.day, key.team, key.model)
	return err
}
