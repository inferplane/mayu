# Console Phase 1a — Analytics Index (Mode A) + Query API + Real Usage View

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans to implement task-by-task. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Derive a local SQLite analytics index from the tamper-evident audit log, expose it through token-gated `GET /admin/analytics/*` read endpoints, and make the console's Usage section show real spend/usage instead of an affordance — directly attacking the customer's #1 pain ("invisible cost").

**Architecture:** The analytics index is **just another `audit.Sink`** (the writer already fans out to sinks; its single goroutine serializes calls → single-writer, no concurrency). Each `request_completed` record with a settled `Cost` is upserted (idempotent by record ULID via `INSERT OR IGNORE`) into a local SQLite `events` table. Queries aggregate with SQL `GROUP BY`. At boot the index replays existing file-sink JSONL (idempotent) so it reflects full history. This is **Mode A** (single-replica, §4.1); Mode B (shared store + aggregator) is deferred to Phase 1b — the idempotent events table is already the foundation Mode B needs. No new audit-writer hook (none exists); the Sink is the documented tap.

**Tech Stack:** Go 1.25, `modernc.org/sqlite` (cgo-free, mirrors `internal/providerstore`), `database/sql`. Vanilla JS for the Usage view (toolchain-free, ADR-002). Tests: Go (`go test -race`) + `adminui_test` asset scans. (Go toolchain for this repo: if `go` is absent, a local install at `~/go-sdk/go/bin` works — `export PATH=~/go-sdk/go/bin:$PATH`.)

## Global Constraints

- **Derived & disposable** (C7/§4): audit is the source of truth; the index is rebuildable and never mutates audit. Index loss never affects the data plane.
- **Idempotent ingestion** (§4.3): keyed by record ULID (`INSERT OR IGNORE`); replay/double-delivery is a no-op.
- **Billable filter** (matches `cmd/inferplane/report.go:52`): ingest only `Event == "request_completed"` AND `Cost != nil`.
- **Group/model rule** (matches `report.go:69-73`): `model = ModelResolved || ModelRequested`; group keys are columns, never concatenated strings.
- **Integer µUSD only** — never float; sum `Cost.AmountUSDMicros` (`int64`).
- **Data-free browser** (ADR-001): Usage view fetches on demand; no `localStorage`/`sessionStorage`. **Toolchain-free** (ADR-002): vanilla JS.
- **Authz** (§4, review-corrected): `/admin/analytics/*` is **full-admin only** in Phase 1a (team-scoped views wait for team records, dep D3). Mounted behind `AdminAuth` + `requireAdmin` (the same wrapper `POST /admin/providers/test` uses).
- **Bounded queries** (§13): every endpoint caps the window (default 30 days, max 366) and never returns unbounded rows.
- **SQLite pattern**: mirror `internal/providerstore/sqlite.go` — `sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")`, `SetMaxOpenConns(1)`, idempotent `CREATE TABLE IF NOT EXISTS` DDL, TEXT/INTEGER columns, `?` placeholders.
- **Capability** (§4.4): when the index is active the assembly reports `analytics_index: "A"` (was `"off"`); the Usage affordance then hides and the real content shows (`capOn("analytics_index")` already handles the enum, Phase 0a).
- **Default-on when audit has a file sink** (§15 Q2): enabled unless `analytics.disabled` is set; path = `analytics.path` or derived from the first file sink's directory (`<dir>/analytics.db`). No file audit sink → index off (nothing durable to derive from), capability stays `"off"`.
- Commit style: DCO sign-off (`git commit -s`); body ends `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`.

---

## File Structure

- `internal/analytics/index.go` — **Create**. `Index` (SQLite): `OpenSQLite(path)`, `Ingest(rec audit.Record)`, `Replay(r io.Reader)`, `Summary(SummaryQuery)`, `TimeSeries(TimeSeriesQuery)`, `Close()`.
- `internal/analytics/index_test.go` — **Create**. Ingest idempotency, billable filter, aggregation, bounded windows.
- `internal/analytics/sink.go` — **Create**. `Sink` implementing `audit.Sink` (`Write`/`Name`/`Required`/`Close`), wrapping `*Index`. Best-effort (`Required()==false`).
- `internal/analytics/sink_test.go` — **Create**. A `request_completed` line ingests; a `request_started` line is ignored; malformed line is dropped (never errors the chain).
- `internal/server/analyticsapi/analyticsapi.go` — **Create**. `SummaryHandler` + `TimeSeriesHandler` over a small `Querier` interface (so the server package doesn't import `analytics` directly — leaf discipline).
- `internal/server/analyticsapi/analyticsapi_test.go` — **Create**. Handler shape, GET-only, bounded params, JSON.
- `internal/server/server.go` — **Modify** `AdminMux` (add a `Querier` param before the variadic; mount the two routes behind `AdminAuth`+`requireAdmin`).
- `internal/config/config.go` — **Modify** add `Analytics AnalyticsConfig { Path string; Disabled bool }` to `Config`.
- `cmd/inferplane/gateway.go` — **Modify** open the index, replay file sinks, add the analytics sink to `buildSinks` output, pass the `Querier` to `AdminMux`, flip the capability to `"A"`.
- `internal/server/adminui/static/index.html` — **Modify** add the Usage content block (tables), shown when the capability is on.
- `internal/server/adminui/static/app.js` — **Modify** `refreshUsageView()` to fetch `/admin/analytics/*` and render tables; call it from `showView("usage")`.
- `internal/server/adminui/adminui_test.go` — **Modify** assert the Usage view fetches `/admin/analytics/`.

**Task order (fail-first TDD):** 1 index core → 2 sink → 3 query API → 4 assembly wiring + config → 5 Usage view.

---

### Task 1: `internal/analytics` index core (SQLite, ingest + queries)

**Files:** Create `internal/analytics/index.go`, `internal/analytics/index_test.go`.

**Interfaces — Produces:**
- `type Index struct { db *sql.DB }`
- `func OpenSQLite(path string) (*Index, error)`
- `func (ix *Index) Ingest(rec audit.Record) error` — idempotent (PK=ULID); ignores non-billable records (returns nil).
- `func (ix *Index) Replay(r io.Reader) (n int, err error)` — scans JSONL, `Ingest`s each billable line; returns count ingested.
- `func (ix *Index) Summary(q SummaryQuery) (Summary, error)` and `func (ix *Index) TimeSeries(q TimeSeriesQuery) ([]DayPoint, error)`.
- `type SummaryQuery struct { SinceDay, UntilDay string }` (inclusive `YYYY-MM-DD`, UTC; empty = no bound).
- `type TimeSeriesQuery struct { Days int }` (clamped 1..366; default 30).
- `type Summary struct { Totals Totals; ByTeam []TeamRow; ByModel []ModelRow }`; `type Totals struct { Requests int64; InputTokens, OutputTokens, CacheReadTokens, CostMicros int64 }`; `type TeamRow struct { Team string; Requests, CostMicros int64 }`; `type ModelRow struct { Model string; Requests, CostMicros int64 }`; `type DayPoint struct { Day string; Requests, CostMicros, InputTokens, OutputTokens int64 }`.

- [ ] **Step 1: Write the failing test**

```go
// internal/analytics/index_test.go
package analytics

import (
	"strings"
	"testing"

	"github.com/inferplane/inferplane/internal/audit"
)

func completed(id, ts, team, model string, in, out, cost int64) audit.Record {
	return audit.Record{
		Event:   "request_completed",
		ID:      id,
		TS:      ts,
		Request: audit.RequestRef{ModelResolved: model},
		Usage:   &audit.UsageRef{InputTokens: in, OutputTokens: out},
		Cost:    &audit.CostRef{AmountUSDMicros: cost},
	}
	// Team lives on Principal.
}

func TestIngest_idempotentAndAggregates(t *testing.T) {
	ix, err := OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()

	r1 := completed("01A", "2026-06-29T10:00:00Z", "alpha", "claude-sonnet-4-6", 100, 50, 1_000)
	r1.Principal = audit.PrincipalRef{Team: "alpha"}
	r2 := completed("01B", "2026-06-29T11:00:00Z", "alpha", "claude-opus-4-8", 200, 80, 5_000)
	r2.Principal = audit.PrincipalRef{Team: "alpha"}
	for _, r := range []audit.Record{r1, r2, r1 /* dup id → ignored */} {
		if err := ix.Ingest(r); err != nil {
			t.Fatalf("ingest: %v", err)
		}
	}
	// A non-billable record must be ignored.
	if err := ix.Ingest(audit.Record{Event: "request_started", ID: "01C", TS: r1.TS}); err != nil {
		t.Fatalf("ingest started: %v", err)
	}

	s, err := ix.Summary(SummaryQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Totals.Requests != 2 {
		t.Fatalf("requests = %d, want 2 (dup ignored, started ignored)", s.Totals.Requests)
	}
	if s.Totals.CostMicros != 6_000 {
		t.Fatalf("cost = %d, want 6000", s.Totals.CostMicros)
	}
	if len(s.ByModel) != 2 {
		t.Fatalf("by-model rows = %d, want 2", len(s.ByModel))
	}
}

func TestReplay_isIdempotent(t *testing.T) {
	ix, _ := OpenSQLite(":memory:")
	defer ix.Close()
	line := `{"event":"request_completed","id":"01Z","ts":"2026-06-29T10:00:00Z","principal":{"team":"alpha"},"request":{"model_resolved":"m1"},"usage":{"input_tokens":10,"output_tokens":5},"cost":{"amount_usd_micros":42}}`
	for i := 0; i < 2; i++ { // replay twice → still one row
		if _, err := ix.Replay(strings.NewReader(line + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	s, _ := ix.Summary(SummaryQuery{})
	if s.Totals.Requests != 1 || s.Totals.CostMicros != 42 {
		t.Fatalf("got requests=%d cost=%d, want 1/42", s.Totals.Requests, s.Totals.CostMicros)
	}
}

func TestTimeSeries_clampsDays(t *testing.T) {
	ix, _ := OpenSQLite(":memory:")
	defer ix.Close()
	if pts, err := ix.TimeSeries(TimeSeriesQuery{Days: 0}); err != nil || pts == nil {
		// default applied; empty index returns an empty (non-nil) slice
		if err != nil {
			t.Fatalf("timeseries: %v", err)
		}
	}
}
```

- [ ] **Step 2: Run test, expect FAIL** — `export PATH=~/go-sdk/go/bin:$PATH; go test ./internal/analytics/ -run TestIngest -v` → `undefined: OpenSQLite`.

- [ ] **Step 3: Implement `internal/analytics/index.go`**

```go
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
  id            TEXT PRIMARY KEY,
  day           TEXT NOT NULL DEFAULT '',
  team          TEXT NOT NULL DEFAULT '',
  model         TEXT NOT NULL DEFAULT '',
  provider      TEXT NOT NULL DEFAULT '',
  status        INTEGER NOT NULL DEFAULT 0,
  input_tokens  INTEGER NOT NULL DEFAULT 0,
  output_tokens INTEGER NOT NULL DEFAULT 0,
  cache_read    INTEGER NOT NULL DEFAULT 0,
  cost_micros   INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS events_day ON events(day);
CREATE INDEX IF NOT EXISTS events_team ON events(team);`

type Index struct{ db *sql.DB }

func OpenSQLite(path string) (*Index, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
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
	var cacheRead, in, out int64
	if r.Usage != nil {
		in, out, cacheRead = r.Usage.InputTokens, r.Usage.OutputTokens, r.Usage.CacheReadInputTokens
	}
	status := 0
	if r.Outcome != nil {
		status = r.Outcome.Status
	}
	_, err := ix.db.Exec(
		`INSERT OR IGNORE INTO events(id,day,team,model,provider,status,input_tokens,output_tokens,cache_read,cost_micros)
		 VALUES(?,?,?,?,?,?,?,?,?,?)`,
		r.ID, dayOf(r.TS), r.Principal.Team, modelOf(r), r.Request.Provider, status,
		in, out, cacheRead, r.Cost.AmountUSDMicros)
	return err
}

// Replay scans a JSONL audit stream and Ingests every billable line. Malformed
// lines are skipped (best-effort, like report.go). Idempotent via Ingest.
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

type SummaryQuery struct{ SinceDay, UntilDay string } // inclusive YYYY-MM-DD; empty = unbounded
type TimeSeriesQuery struct{ Days int }

type Totals struct {
	Requests        int64 `json:"requests"`
	InputTokens     int64 `json:"input_tokens"`
	OutputTokens    int64 `json:"output_tokens"`
	CacheReadTokens int64 `json:"cache_read_tokens"`
	CostMicros      int64 `json:"cost_micros"`
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
	Totals  Totals    `json:"totals"`
	ByTeam  []TeamRow `json:"by_team"`
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
	var s Summary
	s.ByTeam, s.ByModel = []TeamRow{}, []ModelRow{}
	where, args := dayWhere(q.SinceDay, q.UntilDay)

	row := ix.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
		COALESCE(SUM(cache_read),0), COALESCE(SUM(cost_micros),0) FROM events WHERE 1=1`+where, args...)
	if err := row.Scan(&s.Totals.Requests, &s.Totals.InputTokens, &s.Totals.OutputTokens,
		&s.Totals.CacheReadTokens, &s.Totals.CostMicros); err != nil {
		return s, err
	}

	teamRows, err := ix.db.Query(`SELECT team, COUNT(*), COALESCE(SUM(cost_micros),0) FROM events
		WHERE 1=1`+where+` GROUP BY team ORDER BY SUM(cost_micros) DESC`, args...)
	if err != nil {
		return s, err
	}
	defer teamRows.Close()
	for teamRows.Next() {
		var r TeamRow
		if err := teamRows.Scan(&r.Team, &r.Requests, &r.CostMicros); err != nil {
			return s, err
		}
		s.ByTeam = append(s.ByTeam, r)
	}

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
	return s, teamRows.Err()
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
```

- [ ] **Step 4: Run, expect PASS** — `go test ./internal/analytics/ -v`. Then `go vet ./internal/analytics/` + `gofmt -w internal/analytics/`.

- [ ] **Step 5: Commit**

```bash
git add internal/analytics/index.go internal/analytics/index_test.go
git commit -s -m "feat(analytics): derived SQLite index over audit (Mode A) — ingest + queries

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 2: `audit.Sink` adapter for the index

**Files:** Create `internal/analytics/sink.go`, `internal/analytics/sink_test.go`.

**Interfaces — Produces:** `func NewSink(ix *Index) audit.Sink`. The sink's `Write([]byte) error` unmarshals the canonical line and calls `ix.Ingest`; `Name() string` returns `"analytics"`; `Required() bool` returns `false` (best-effort — must never block or fail the chain); `Close() error` is a no-op (the Index is closed by the assembly that owns it).

> Confirm the `audit.Sink` interface shape first: `internal/audit/sink.go:11-16` — `Write([]byte) error`, `Name() string`, `Required() bool`, `Close() error`. Match it exactly.

- [ ] **Step 1: Failing test**

```go
// internal/analytics/sink_test.go
package analytics

import "testing"

func TestSink_ingestsCompletedIgnoresRest(t *testing.T) {
	ix, _ := OpenSQLite(":memory:")
	defer ix.Close()
	s := NewSink(ix)
	if s.Required() {
		t.Fatal("analytics sink must be best-effort (Required()==false)")
	}
	if err := s.Write([]byte(`{"event":"request_completed","id":"x1","ts":"2026-06-29T00:00:00Z","principal":{"team":"t"},"request":{"model_resolved":"m"},"cost":{"amount_usd_micros":7}}`)); err != nil {
		t.Fatalf("write completed: %v", err)
	}
	if err := s.Write([]byte(`{"event":"request_started","id":"x2","ts":"2026-06-29T00:00:00Z"}`)); err != nil {
		t.Fatalf("write started: %v", err)
	}
	if err := s.Write([]byte(`not json`)); err != nil {
		t.Fatalf("malformed line must not error the chain: %v", err)
	}
	s2, _ := ix.Summary(SummaryQuery{})
	if s2.Totals.Requests != 1 || s2.Totals.CostMicros != 7 {
		t.Fatalf("got %d/%d, want 1/7", s2.Totals.Requests, s2.Totals.CostMicros)
	}
}
```

- [ ] **Step 2: Run, expect FAIL** (`undefined: NewSink`).

- [ ] **Step 3: Implement `internal/analytics/sink.go`**

```go
package analytics

import (
	"encoding/json"

	"github.com/inferplane/inferplane/internal/audit"
)

type sink struct{ ix *Index }

// NewSink adapts the index to the audit.Sink fan-out. It is best-effort
// (Required()==false): a parse error or ingest error is swallowed so the
// tamper-evident chain and the data plane are never blocked by the derived
// index (§4 isolation invariant).
func NewSink(ix *Index) audit.Sink { return &sink{ix: ix} }

func (s *sink) Write(line []byte) error {
	var rec audit.Record
	if json.Unmarshal(line, &rec) != nil {
		return nil // malformed → skip, never error the chain
	}
	_ = s.ix.Ingest(rec) // best-effort; derived index must not break the writer
	return nil
}

func (s *sink) Name() string   { return "analytics" }
func (s *sink) Required() bool { return false }
func (s *sink) Close() error   { return nil } // Index lifecycle owned by the assembly
```

- [ ] **Step 4: Run, expect PASS** + vet + fmt.

- [ ] **Step 5: Commit**

```bash
git add internal/analytics/sink.go internal/analytics/sink_test.go
git commit -s -m "feat(analytics): best-effort audit.Sink adapter feeding the index

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 3: Query API handlers + mount (full-admin)

**Files:** Create `internal/server/analyticsapi/analyticsapi.go`, `_test.go`; Modify `internal/server/server.go`.

**Interfaces — Produces:**
- In `analyticsapi`: `type Querier interface { Summary(analytics.SummaryQuery) (analytics.Summary, error); TimeSeries(analytics.TimeSeriesQuery) ([]analytics.DayPoint, error) }` and `func SummaryHandler(q Querier) http.Handler`, `func TimeSeriesHandler(q Querier) http.Handler`. `*analytics.Index` satisfies `Querier` structurally.
- `AdminMux` gains `analyticsQ analyticsapi.Querier` immediately before the variadic; nil → routes omitted.

- [ ] **Step 1: Failing test** (`analyticsapi_test.go`): a fake `Querier` returns a known `Summary`; `SummaryHandler` GET → 200 JSON containing `"cost_micros"`; POST → 405; `TimeSeriesHandler` clamps `?days=9999` (assert the fake received `Days<=366` — or assert 200 + JSON array). Also add to `server_test.go`: `AdminMux(..., fakeQ)` then `GET /admin/analytics/summary` with `Bearer tok` → 200 (full-admin); and **without admin (no token) → 401**.

```go
// internal/server/analyticsapi/analyticsapi_test.go
package analyticsapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/inferplane/inferplane/internal/analytics"
)

type fakeQ struct{ gotDays int }

func (f *fakeQ) Summary(analytics.SummaryQuery) (analytics.Summary, error) {
	return analytics.Summary{Totals: analytics.Totals{Requests: 3, CostMicros: 1234}}, nil
}
func (f *fakeQ) TimeSeries(q analytics.TimeSeriesQuery) ([]analytics.DayPoint, error) {
	f.gotDays = q.Days
	return []analytics.DayPoint{{Day: "2026-06-29", CostMicros: 1234}}, nil
}

func TestSummaryHandler_GET(t *testing.T) {
	rec := httptest.NewRecorder()
	SummaryHandler(&fakeQ{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/analytics/summary", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"cost_micros"`) {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
}

func TestSummaryHandler_rejectsPOST(t *testing.T) {
	rec := httptest.NewRecorder()
	SummaryHandler(&fakeQ{}).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/analytics/summary", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status %d, want 405", rec.Code)
	}
}
```

- [ ] **Step 2: Run, expect FAIL.**

- [ ] **Step 3: Implement `analyticsapi.go`** — parse `?since`/`?until` (YYYY-MM-DD passthrough to `SummaryQuery`) and `?days` (atoi, the Index clamps); GET-only (405 otherwise); `json.NewEncoder(w).Encode(result)`; `Content-Type: application/json`. On a query error, `http.Error(w, "analytics query failed", 500)` (no internal detail leaked).

```go
// internal/server/analyticsapi/analyticsapi.go
package analyticsapi

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/inferplane/inferplane/internal/analytics"
)

type Querier interface {
	Summary(analytics.SummaryQuery) (analytics.Summary, error)
	TimeSeries(analytics.TimeSeriesQuery) ([]analytics.DayPoint, error)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func SummaryHandler(q Querier) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		out, err := q.Summary(analytics.SummaryQuery{
			SinceDay: r.URL.Query().Get("since"),
			UntilDay: r.URL.Query().Get("until"),
		})
		if err != nil {
			http.Error(w, "analytics query failed", http.StatusInternalServerError)
			return
		}
		writeJSON(w, out)
	})
}

func TimeSeriesHandler(q Querier) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		days, _ := strconv.Atoi(r.URL.Query().Get("days")) // 0 on parse error → Index defaults
		out, err := q.TimeSeries(analytics.TimeSeriesQuery{Days: days})
		if err != nil {
			http.Error(w, "analytics query failed", http.StatusInternalServerError)
			return
		}
		writeJSON(w, out)
	})
}
```

- [ ] **Step 4: Mount in `AdminMux`** — add param `analyticsQ analyticsapi.Querier` before `probeAllowedHosts ...string`; mount full-admin-gated (reuse `requireAdmin`, like the probe route). Update existing test callers (add a trailing `nil` before the variadic, as in Phase 0a Task 2).

```go
	// Analytics read API (spec §4 / D1). Full-admin only in Phase 1a (team-scoped
	// views await team records, D3). nil → omitted (index disabled).
	if analyticsQ != nil {
		mux.Handle("GET /admin/analytics/summary", AdminAuth(adminTokens, verifier, mapping, denied,
			requireAdmin(analyticsapi.SummaryHandler(analyticsQ), emit)))
		mux.Handle("GET /admin/analytics/timeseries", AdminAuth(adminTokens, verifier, mapping, denied,
			requireAdmin(analyticsapi.TimeSeriesHandler(analyticsQ), emit)))
	}
```

> `requireAdmin(h, emit)` is the existing wrapper (`server.go`, used by the probe). The capabilities param added in Phase 0a stays before this one; keep the variadic last. Update all existing `AdminMux(...)` test callers to pass a trailing `nil` for `analyticsQ` (grep `AdminMux(` in `*_test.go`).

- [ ] **Step 5: Run** `go test ./internal/server/analyticsapi/ ./internal/server/ -v`, expect PASS. vet + fmt.

- [ ] **Step 6: Commit**

```bash
git add internal/server/analyticsapi/ internal/server/server.go internal/server/*_test.go
git commit -s -m "feat(analyticsapi): full-admin /admin/analytics/{summary,timeseries} + mount

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 4: Config + assembly wiring (open index, replay, sink, capability "A")

**Files:** Modify `internal/config/config.go`, `cmd/inferplane/gateway.go`.

**Interfaces — Produces:** `config.AnalyticsConfig { Path string \`json:"path"\`; Disabled bool \`json:"disabled"\` }` on `Config` as `Analytics AnalyticsConfig \`json:"analytics"\``. The gateway opens the index when enabled and threads it as both an audit sink and the `AdminMux` `Querier`.

- [ ] **Step 1: Add the config struct field** to `Config` (`internal/config/config.go`), with the JSON tag `analytics`. No validation needed (path is a local file path, not a secret).

- [ ] **Step 2: Resolution + wiring in `gateway.go`.** CRITICAL ORDERING: the analytics sink must be appended to `sinks` **after `sinks, _ := buildSinks(raw.Audit.Sinks)` and BEFORE `audit.NewWriter(inst, raw.Audit.Buffer.Path, sinks)`** (~gateway.go:83-89). At that point the later `auditFileSinks` var (computed ~line 185) does NOT exist yet, so compute the file-sink paths inline from `raw.Audit.Sinks`. Insert this block immediately after `buildSinks` and before `NewWriter`:

```go
	// Analytics index (Mode A, spec §4): default-on when a file audit sink exists,
	// unless analytics.disabled. Path = analytics.path or <first file sink dir>/analytics.db.
	// Opened here so its Sink can join `sinks` BEFORE NewWriter fans out to them.
	var analyticsIdx *analytics.Index
	if !raw.Analytics.Disabled {
		var fileSinkPaths []string
		for _, sk := range raw.Audit.Sinks {
			if sk.Type == "file" {
				fileSinkPaths = append(fileSinkPaths, sk.Path)
			}
		}
		apath := raw.Analytics.Path
		if apath == "" && len(fileSinkPaths) > 0 {
			apath = filepath.Join(filepath.Dir(fileSinkPaths[0]), "analytics.db")
		}
		if apath != "" {
			ix, err := analytics.OpenSQLite(apath)
			if err != nil {
				fmt.Fprintln(os.Stderr, "inferplane: analytics index disabled (open failed):", err)
			} else {
				analyticsIdx = ix
				// Replay existing audit history so the index reflects the full log
				// (idempotent). Best-effort: a replay error never blocks boot.
				for _, p := range fileSinkPaths {
					if f, e := os.Open(p); e == nil {
						if _, e := analyticsIdx.Replay(f); e != nil {
							fmt.Fprintln(os.Stderr, "inferplane: analytics replay warning:", e)
						}
						f.Close()
					}
				}
				sinks = append(sinks, analytics.NewSink(analyticsIdx)) // live ingestion
			}
		}
	}
```

> `analyticsIdx` is declared here (early), so it is in scope for the capability closure + `AdminMux` call later (~line 260). Add imports `path/filepath`, `os` (likely already imported), `github.com/inferplane/inferplane/internal/analytics`. Store it on the gateway struct (`g.analyticsIdx = analyticsIdx` where `g` is built) and **close it in the gateway's shutdown/`closeAll` path** (the assembly owns the Index; the sink's `Close()` is a no-op). On the early-boot error paths that call `closeAll(...)` before `g` exists, close `analyticsIdx` too if non-nil.

- [ ] **Step 3: Flip the capability + pass the Querier.** Update the `capabilities` closure (Phase 0a) and the `AdminMux` call:

```go
	capabilities := func() configapi.Capabilities {
		ai := "off"
		if analyticsIdx != nil {
			ai = "A"
		}
		return configapi.Capabilities{
			AnalyticsIndex: ai,
			ProviderStore:  writer != nil,
			Guardrails:     masking != nil,
		}
	}
	var analyticsQ analyticsapi.Querier
	if analyticsIdx != nil {
		analyticsQ = analyticsIdx
	}
	g.adminSrv = &http.Server{Handler: server.AdminMux(store, cfg.Server.AdminAuth.Tokens, oidcVerifier(cfg), oidcMapping(cfg), liveView(holder, pstore != nil), auditFileSinks, aud, m, writer, liveExport(holder), capabilities, analyticsQ, cfg.Probe.AllowedHosts...)}
```

> Add import `github.com/inferplane/inferplane/internal/server/analyticsapi`. `analyticsQ` goes after `capabilities`, before the variadic — match the Task 3 signature order.

- [ ] **Step 4: Verify** — `go build ./...`; `go test ./internal/config/ ./cmd/... ./internal/server/ -race`; `go vet ./...`; `gofmt -l .`.

- [ ] **Step 5: Smoke** (optional): add `"analytics": {}` is unnecessary — with a file audit sink the index auto-enables. Run `go run ./cmd/inferplane serve --config examples/config.json` only if the example has a file audit sink; otherwise skip.

- [ ] **Step 6: Commit**

```bash
git add internal/config/config.go cmd/inferplane/gateway.go
git commit -s -m "feat(gateway): wire analytics index (default-on w/ file audit sink) + capability A

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 5: Real Usage view (tables from the query API)

**Files:** Modify `internal/server/adminui/static/index.html`, `app.js`, `adminui_test.go`.

**Interfaces — Produces:** a Usage content block (`#usage-content`) with totals + by-team + by-model tables, populated by `refreshUsageView()` fetching `/admin/analytics/summary`. Shown when `capOn("analytics_index")`; the affordance card shows otherwise (Phase 0a wiring already toggles `.affordance`).

- [ ] **Step 1: Failing asset test** (append to `adminui_test.go`):

```go
func TestAdminUI_usageFetchesAnalytics(t *testing.T) {
	_, js := get(t, "/app.js")
	if !strings.Contains(js, "/admin/analytics/summary") {
		t.Error("app.js Usage view does not fetch /admin/analytics/summary")
	}
	if !strings.Contains(js, "refreshUsageView") {
		t.Error("app.js missing refreshUsageView()")
	}
	_, html := get(t, "/index.html")
	if !strings.Contains(html, `id="usage-content"`) {
		t.Error("index.html missing #usage-content block")
	}
}
```

- [ ] **Step 2: Run, expect FAIL.**

- [ ] **Step 3: Add the Usage content block** in `#view-usage` (after the affordance card) in `index.html`:

```html
        <div id="usage-content" hidden>
          <div class="card">
            <div class="microlabel">totals · last 30 days</div>
            <table class="data" id="usage-totals"><tbody></tbody></table>
          </div>
          <div class="twocol">
            <div class="card">
              <div class="microlabel">spend by team</div>
              <table class="data" id="usage-by-team"><thead><tr><th>team</th><th class="num">req</th><th class="num">USD</th></tr></thead><tbody></tbody></table>
            </div>
            <div class="card">
              <div class="microlabel">spend by model</div>
              <table class="data" id="usage-by-model"><thead><tr><th>model</th><th class="num">req</th><th class="num">USD</th></tr></thead><tbody></tbody></table>
            </div>
          </div>
        </div>
```

- [ ] **Step 4: Add `refreshUsageView()`** in `app.js` and call it from `showView`. Reuse the existing `td()` helper and a µUSD→USD formatter (mirror the server's integer formatting — divide by 1e6 with 2-4 dp; a simple `(micros/1e6).toFixed(4)` is acceptable in the UI display layer):

```js
async function refreshUsageView() {
  const content = $("usage-content");
  if (!capOn("analytics_index")) { content.hidden = true; return; }
  content.hidden = false;
  const s = await api("GET", "/admin/analytics/summary", null, true);
  if (!s || s === DISABLED) { content.hidden = true; return; }
  const usd = (m) => "$" + (Number(m) / 1e6).toFixed(4);
  const totals = $("usage-totals").querySelector("tbody");
  totals.replaceChildren();
  const tline = (k, v) => { const tr = document.createElement("tr"); tr.append(td(k), td(v)); totals.append(tr); };
  tline("requests", String(s.totals.requests));
  tline("input tokens", String(s.totals.input_tokens));
  tline("output tokens", String(s.totals.output_tokens));
  tline("spend", usd(s.totals.cost_micros));
  const fill = (id, rows, nameKey) => {
    const tb = $(id).querySelector("tbody"); tb.replaceChildren();
    (rows || []).forEach((r) => { const tr = document.createElement("tr");
      tr.append(td(r[nameKey] || "—"), td(String(r.requests)), td(usd(r.cost_micros))); tb.append(tr); });
  };
  fill("usage-by-team", s.by_team, "team");
  fill("usage-by-model", s.by_model, "model");
}
```

Wire it into `showView` next to the other per-view refreshers:

```js
  if (name === "usage") refreshUsageView();
```

> `refreshUsageView` calls `api(..., true)` (optional) so a disabled index degrades to a hidden content block, not an error (§9.1). Keep `td()` usage textContent-only (no innerHTML) — data never becomes markup (existing invariant).

- [ ] **Step 5: Run** `go test ./internal/server/adminui/ -v`, expect PASS. Confirm `grep -nE "localStorage|sessionStorage" app.js` → clean.

- [ ] **Step 6: Commit**

```bash
git add internal/server/adminui/static/index.html internal/server/adminui/static/app.js internal/server/adminui/adminui_test.go
git commit -s -m "feat(adminui): real Usage view — totals + by-team/model from analytics API

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Final verification

- [ ] `gofmt -l .` → clean · `go vet ./...` → clean
- [ ] `go test ./... -race` → PASS (incl. new `analytics`, `analyticsapi`, updated `server`/`adminui`)
- [ ] `CGO_ENABLED=0 go build -trimpath -o bin/inferplane ./cmd/inferplane` → builds
- [ ] `bash tests/run-all.sh` → pass
- [ ] Smoke: serve with a file audit sink, issue a key, send a request, open `/admin/ui/` → Usage shows totals + the request's spend; `/admin/capabilities` reports `analytics_index:"A"`.

## Scope boundary

**In scope (Phase 1a):** Mode A local index as an audit Sink; idempotent ingest + boot replay; full-admin summary + timeseries query API; capability flip to "A"; Usage view tables.

**Out of scope:** Mode B shared store + single-writer aggregator/fencing (Phase 1b, §4.6); Logs viewer + request-row API + body store (Phase 3); team-scoped analytics authz (needs team records, D3); charts/sparklines (Phase 0b — Usage uses tables for now); CSV export endpoint (§6.2, follow-up); drift/health endpoint `/admin/analytics/health` (Phase 1b with Mode B).
