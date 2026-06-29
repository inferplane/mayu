// Package analytics maintains a DERIVED, rebuildable read-model of the audit log
// for the console's Usage analytics (design spec §4). The audit chain is the
// source of truth; this index is disposable — losing it never affects the data
// plane, and Replay() reconstructs it from the audit JSONL. Mode A (single
// local SQLite); Mode B (shared HA store) is a later phase. Ingestion is
// idempotent by record ULID, so replay/double-delivery is a no-op.
package analytics

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"io"

	"github.com/inferplane/inferplane/internal/audit"

	_ "modernc.org/sqlite" // pure-Go driver (CGO_ENABLED=0), registered as "sqlite"
)

// TEXT/INTEGER only, Postgres-portable (mirrors internal/providerstore).
const schema = `
CREATE TABLE IF NOT EXISTS events (
  id             TEXT PRIMARY KEY,
  day            TEXT NOT NULL DEFAULT '',
  team           TEXT NOT NULL DEFAULT '',
  model          TEXT NOT NULL DEFAULT '',
  provider       TEXT NOT NULL DEFAULT '',
  status         INTEGER NOT NULL DEFAULT 0,
  input_tokens   INTEGER NOT NULL DEFAULT 0,
  output_tokens  INTEGER NOT NULL DEFAULT 0,
  cache_read     INTEGER NOT NULL DEFAULT 0,
  cache_creation INTEGER NOT NULL DEFAULT 0,
  cost_micros    INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS events_day ON events(day);
CREATE INDEX IF NOT EXISTS events_team ON events(team);`

type Index struct{ db *sql.DB }

func OpenSQLite(path string) (*Index, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	// Deliberately NO SetMaxOpenConns(1): unlike providerstore, this index is
	// read-concurrent (HTTP query handlers) while a single goroutine (the audit
	// Sink) writes. Capping to one connection would let a slow query block
	// Ingest() on the audit-writer goroutine and stall the fan-out. WAL mode
	// (DSN above) supports concurrent readers + one writer safely.
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Index{db: db}, nil
}

func (ix *Index) Close() error { return ix.db.Close() }

// billable mirrors cmd/inferplane/report.go: only settled completed records.
func billable(r audit.Record) bool { return r.Event == "request_completed" && r.Cost != nil }

func modelOf(r audit.Record) string {
	if r.Request.ModelResolved != "" {
		return r.Request.ModelResolved
	}
	return r.Request.ModelRequested
}

// dayOf extracts the UTC YYYY-MM-DD prefix from an RFC3339(Nano) timestamp.
// The audit TS is already UTC (RFC3339Nano), so a prefix is correct and cheap.
func dayOf(ts string) string {
	if len(ts) >= 10 {
		return ts[:10]
	}
	return ""
}

// Ingest upserts one billable record; non-billable records are ignored. PK=ULID
// with INSERT OR IGNORE makes re-ingestion of the same record a no-op.
func (ix *Index) Ingest(r audit.Record) error {
	if !billable(r) {
		return nil
	}
	var in, out, cacheRead, cacheCreate int64
	if r.Usage != nil {
		in, out = r.Usage.InputTokens, r.Usage.OutputTokens
		cacheRead, cacheCreate = r.Usage.CacheReadInputTokens, r.Usage.CacheCreationInputTokens
	}
	status := 0
	if r.Outcome != nil {
		status = r.Outcome.Status
	}
	_, err := ix.db.Exec(
		`INSERT OR IGNORE INTO events(id,day,team,model,provider,status,input_tokens,output_tokens,cache_read,cache_creation,cost_micros)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, dayOf(r.TS), r.Principal.Team, modelOf(r), r.Request.Provider, status,
		in, out, cacheRead, cacheCreate, r.Cost.AmountUSDMicros)
	return err
}

// Replay scans a JSONL audit stream and Ingests every billable line. Malformed
// lines are skipped (best-effort, like report.go). Idempotent via Ingest. The
// returned count is billable lines SEEN (duplicates already in the index are
// counted as seen, not newly inserted) — it is only for a boot log line.
func (ix *Index) Replay(r io.Reader) (int, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	n := 0
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec audit.Record
		if json.Unmarshal(line, &rec) != nil {
			continue
		}
		if !billable(rec) {
			continue
		}
		if err := ix.Ingest(rec); err != nil {
			return n, err
		}
		n++
	}
	return n, sc.Err()
}

// SummaryQuery bounds the aggregate by inclusive UTC day (YYYY-MM-DD); empty =
// unbounded at this layer. The API handler applies the default 30-day / max
// 366-day window before calling this (spec §13).
type SummaryQuery struct{ SinceDay, UntilDay string }
type TimeSeriesQuery struct{ Days int }

type Totals struct {
	Requests            int64 `json:"requests"`
	InputTokens         int64 `json:"input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	CacheReadTokens     int64 `json:"cache_read_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_tokens"`
	CostMicros          int64 `json:"cost_micros"`
}
type TeamRow struct {
	Team       string `json:"team"`
	Requests   int64  `json:"requests"`
	CostMicros int64  `json:"cost_micros"`
}
type ModelRow struct {
	Model      string `json:"model"`
	Requests   int64  `json:"requests"`
	CostMicros int64  `json:"cost_micros"`
}
type Summary struct {
	Totals  Totals     `json:"totals"`
	ByTeam  []TeamRow  `json:"by_team"`
	ByModel []ModelRow `json:"by_model"`
}
type DayPoint struct {
	Day          string `json:"day"`
	Requests     int64  `json:"requests"`
	CostMicros   int64  `json:"cost_micros"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
}

func dayWhere(since, until string) (string, []any) {
	clause, args := "", []any{}
	if since != "" {
		clause += " AND day >= ?"
		args = append(args, since)
	}
	if until != "" {
		clause += " AND day <= ?"
		args = append(args, until)
	}
	return clause, args
}

func (ix *Index) Summary(q SummaryQuery) (Summary, error) {
	s := Summary{ByTeam: []TeamRow{}, ByModel: []ModelRow{}}
	where, args := dayWhere(q.SinceDay, q.UntilDay)

	row := ix.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
		COALESCE(SUM(cache_read),0), COALESCE(SUM(cache_creation),0), COALESCE(SUM(cost_micros),0)
		FROM events WHERE 1=1`+where, args...)
	if err := row.Scan(&s.Totals.Requests, &s.Totals.InputTokens, &s.Totals.OutputTokens,
		&s.Totals.CacheReadTokens, &s.Totals.CacheCreationTokens, &s.Totals.CostMicros); err != nil {
		return s, err
	}

	teamRows, err := ix.db.Query(`SELECT team, COUNT(*), COALESCE(SUM(cost_micros),0) FROM events
		WHERE 1=1`+where+` GROUP BY team ORDER BY SUM(cost_micros) DESC`, args...)
	if err != nil {
		return s, err
	}
	for teamRows.Next() {
		var r TeamRow
		if err := teamRows.Scan(&r.Team, &r.Requests, &r.CostMicros); err != nil {
			teamRows.Close()
			return s, err
		}
		s.ByTeam = append(s.ByTeam, r)
	}
	if err := teamRows.Err(); err != nil {
		teamRows.Close()
		return s, err
	}
	teamRows.Close()

	modelRows, err := ix.db.Query(`SELECT model, COUNT(*), COALESCE(SUM(cost_micros),0) FROM events
		WHERE 1=1`+where+` GROUP BY model ORDER BY SUM(cost_micros) DESC`, args...)
	if err != nil {
		return s, err
	}
	defer modelRows.Close()
	for modelRows.Next() {
		var r ModelRow
		if err := modelRows.Scan(&r.Model, &r.Requests, &r.CostMicros); err != nil {
			return s, err
		}
		s.ByModel = append(s.ByModel, r)
	}
	return s, modelRows.Err()
}

func (ix *Index) TimeSeries(q TimeSeriesQuery) ([]DayPoint, error) {
	days := q.Days
	if days <= 0 {
		days = 30
	}
	if days > 366 {
		days = 366
	}
	rows, err := ix.db.Query(`SELECT day, COUNT(*), COALESCE(SUM(cost_micros),0),
		COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0)
		FROM events GROUP BY day ORDER BY day DESC LIMIT ?`, days)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DayPoint{}
	for rows.Next() {
		var p DayPoint
		if err := rows.Scan(&p.Day, &p.Requests, &p.CostMicros, &p.InputTokens, &p.OutputTokens); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
