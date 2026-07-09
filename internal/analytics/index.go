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
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/inferplane/inferplane/internal/audit"

	_ "modernc.org/sqlite" // pure-Go driver (CGO_ENABLED=0), registered as "sqlite"
)

// TEXT/INTEGER only, Postgres-portable (mirrors internal/providerstore). ts
// and body_ref (D4, ADR-018) are included here for a FRESH database; an
// existing database gets them via the ALTER-if-missing migration below
// (SQLite has no "ADD COLUMN IF NOT EXISTS").
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
  cost_micros    INTEGER NOT NULL DEFAULT 0,
  ts             TEXT NOT NULL DEFAULT '',
  body_ref       TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS events_day ON events(day);
CREATE INDEX IF NOT EXISTS events_team ON events(team);`

// eventsMigrationColumns are added to a pre-existing "events" table that
// predates them (D4, ADR-018's ts/body_ref) — mirrors keystore.sqlite.go's
// ALTER-if-missing pattern for the "keys" table, at 1/10th the size since
// there's no legacy-column-name mapping to worry about here.
var eventsMigrationColumns = []string{
	`ALTER TABLE events ADD COLUMN ts TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE events ADD COLUMN body_ref TEXT NOT NULL DEFAULT ''`,
}

// migrateEventsColumns adds any of eventsMigrationColumns not already
// present, so a pre-D4 database (created before ts/body_ref existed) gets
// them without losing its rows. A fresh database already has them from the
// CREATE TABLE above, so this is a no-op there.
func migrateEventsColumns(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(events)`)
	if err != nil {
		return err
	}
	existing := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		existing[name] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()
	for _, stmt := range eventsMigrationColumns {
		// Each ALTER statement's target column name is token index 5:
		// "ALTER(0) TABLE(1) events(2) ADD(3) COLUMN(4) <name>(5) ...".
		fields := strings.Fields(stmt)
		col := fields[5]
		if existing[col] {
			continue
		}
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("migrate events.%s: %w", col, err)
		}
	}
	return nil
}

type Index struct {
	db *sql.DB

	mu         sync.Mutex
	lastIngest time.Time
}

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
	if err := migrateEventsColumns(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Index{db: db}, nil
}

func (ix *Index) Close() error { return ix.db.Close() }

// Billable mirrors cmd/inferplane/report.go: only settled completed records.
func Billable(r audit.Record) bool { return r.Event == "request_completed" && r.Cost != nil }

// ModelOf returns the resolved model when available, falling back to the
// requested model.
func ModelOf(r audit.Record) string {
	if r.Request.ModelResolved != "" {
		return r.Request.ModelResolved
	}
	return r.Request.ModelRequested
}

// DayOf extracts the UTC YYYY-MM-DD prefix from an RFC3339(Nano) timestamp.
// The audit TS is already UTC (RFC3339Nano), so a prefix is correct and cheap.
func DayOf(ts string) string {
	if len(ts) >= 10 {
		return ts[:10]
	}
	return ""
}

func billable(r audit.Record) bool  { return Billable(r) }
func modelOf(r audit.Record) string { return ModelOf(r) }
func dayOf(ts string) string        { return DayOf(ts) }

// execer is satisfied by both *sql.DB and *sql.Tx, so Ingest can run either on
// the live DB (one autocommit per record) or batched inside a Replay tx.
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func ingest(e execer, r audit.Record) error {
	if !Billable(r) {
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
	var bodyRef string
	if r.BodyRef != nil {
		bodyRef = *r.BodyRef
	}
	_, err := e.Exec(
		`INSERT OR IGNORE INTO events(id,day,team,model,provider,status,input_tokens,output_tokens,cache_read,cache_creation,cost_micros,ts,body_ref)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, DayOf(r.TS), r.Principal.Team, ModelOf(r), r.Request.Provider, status,
		in, out, cacheRead, cacheCreate, r.Cost.AmountUSDMicros, r.TS, bodyRef)
	return err
}

// Ingest upserts one billable record; non-billable records are ignored. PK=ULID
// with INSERT OR IGNORE makes re-ingestion of the same record a no-op.
func (ix *Index) Ingest(r audit.Record) error {
	if err := ingest(ix.db, r); err != nil {
		return err
	}
	if Billable(r) {
		ix.markIngest(time.Now().UTC())
	}
	return nil
}

func (ix *Index) markIngest(t time.Time) {
	ix.mu.Lock()
	ix.lastIngest = t
	ix.mu.Unlock()
}

// Health reports Mode A freshness for /admin/analytics/health.
func (ix *Index) Health() (Health, error) {
	ix.mu.Lock()
	lastIngest := ix.lastIngest
	ix.mu.Unlock()

	h := Health{
		Mode:            "A",
		IsLeader:        true,
		LeaseEpoch:      0,
		LagSeconds:      0,
		SegmentsTracked: 0,
	}
	if !lastIngest.IsZero() {
		h.LastIngestTS = lastIngest.Format(time.RFC3339Nano)
	}
	return h, nil
}

// Replay scans a JSONL audit stream and ingests every billable, newline-
// terminated line inside ONE transaction (so a large boot replay does not pay
// an fsync per row). It reads only complete lines (ReadString), dropping an
// unterminated final remainder exactly as cmd/inferplane/report.go does — so a
// crash-truncated last record is never half-ingested. Malformed lines are
// skipped. Idempotent. The returned count is billable lines SEEN (duplicates
// already present count as seen) — only for a boot log line.
func (ix *Index) Replay(r io.Reader) (int, error) {
	tx, err := ix.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() // no-op after a successful Commit

	br := bufio.NewReader(r)
	n := 0
	for {
		line, readErr := br.ReadString('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' { // complete line only
			var rec audit.Record
			if json.Unmarshal([]byte(line), &rec) == nil && Billable(rec) {
				if e := ingest(tx, rec); e != nil {
					return n, e
				}
				n++
			}
		}
		if readErr != nil { // io.EOF (drops any unterminated trailing remainder)
			break
		}
	}
	if err := tx.Commit(); err != nil {
		return n, err
	}
	if n > 0 {
		ix.markIngest(time.Now().UTC())
	}
	return n, nil
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

// Event is one row from Recent — the console's Logs list (D4, ADR-018).
// BodyRef is "" when the request wasn't captured (log_bodies off, an error
// response, or a streaming response — request-only capture there).
type Event struct {
	ID           string `json:"id"`
	TS           string `json:"ts"`
	Team         string `json:"team"`
	Model        string `json:"model"`
	Provider     string `json:"provider"`
	Status       int    `json:"status"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	CostMicros   int64  `json:"cost_micros"`
	BodyRef      string `json:"body_ref,omitempty"`
}

// recentEventsLimit bounds a single Recent page — the console paginates via
// `before` (id keyset) rather than requesting unbounded history in one call.
const recentEventsLimit = 200

// Recent returns the most recent events, newest first (ULIDs are
// lexicographically time-ordered, so an id DESC sort is correct). limit is
// clamped to (0, recentEventsLimit]; before, when set, keyset-paginates
// strictly older than that event's ID (id-based, not offset-based, so a
// concurrent insert can't shift or duplicate a page).
func (ix *Index) Recent(limit int, before string) ([]Event, error) {
	if limit <= 0 || limit > recentEventsLimit {
		limit = recentEventsLimit
	}
	query := `SELECT id, ts, team, model, provider, status, input_tokens, output_tokens, cost_micros, body_ref FROM events`
	args := []any{}
	if before != "" {
		query += ` WHERE id < ?`
		args = append(args, before)
	}
	query += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := ix.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.TS, &e.Team, &e.Model, &e.Provider, &e.Status,
			&e.InputTokens, &e.OutputTokens, &e.CostMicros, &e.BodyRef); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
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
	// Bound the SCAN by day (uses the events_day index), not just the result
	// LIMIT — otherwise SQLite would group the entire history before limiting.
	// date('now', ...) is UTC and returns 'YYYY-MM-DD', matching the day column.
	rows, err := ix.db.Query(`SELECT day, COUNT(*), COALESCE(SUM(cost_micros),0),
		COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0)
		FROM events WHERE day >= date('now', ?) GROUP BY day ORDER BY day DESC LIMIT ?`,
		fmt.Sprintf("-%d days", days), days)
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
