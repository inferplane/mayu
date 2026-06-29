package analytics

import (
	"strings"
	"testing"

	"github.com/inferplane/inferplane/internal/audit"
)

func completed(id, ts, team, model string, in, out, cacheCreate, cost int64) audit.Record {
	return audit.Record{
		Event:     "request_completed",
		ID:        id,
		TS:        ts,
		Principal: audit.PrincipalRef{Team: team},
		Request:   audit.RequestRef{ModelResolved: model},
		Usage:     &audit.UsageRef{InputTokens: in, OutputTokens: out, CacheCreationInputTokens: cacheCreate},
		Cost:      &audit.CostRef{AmountUSDMicros: cost},
	}
}

func TestIngest_idempotentAndAggregates(t *testing.T) {
	ix, err := OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()

	r1 := completed("01A", "2026-06-29T10:00:00Z", "alpha", "claude-sonnet-4-6", 100, 50, 10, 1_000)
	r2 := completed("01B", "2026-06-29T11:00:00Z", "alpha", "claude-opus-4-8", 200, 80, 0, 5_000)
	for _, r := range []audit.Record{r1, r2, r1 /* dup id → ignored */} {
		if err := ix.Ingest(r); err != nil {
			t.Fatalf("ingest: %v", err)
		}
	}
	// Non-billable records are ignored.
	if err := ix.Ingest(audit.Record{Event: "request_started", ID: "01C", TS: r1.TS}); err != nil {
		t.Fatalf("ingest started: %v", err)
	}
	if err := ix.Ingest(audit.Record{Event: "request_completed", ID: "01D", TS: r1.TS /* Cost nil */}); err != nil {
		t.Fatalf("ingest unsettled: %v", err)
	}

	s, err := ix.Summary(SummaryQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Totals.Requests != 2 {
		t.Fatalf("requests = %d, want 2 (dup + started + unsettled ignored)", s.Totals.Requests)
	}
	if s.Totals.CostMicros != 6_000 {
		t.Fatalf("cost = %d, want 6000", s.Totals.CostMicros)
	}
	if s.Totals.CacheCreationTokens != 10 {
		t.Fatalf("cache_creation = %d, want 10", s.Totals.CacheCreationTokens)
	}
	if len(s.ByModel) != 2 || len(s.ByTeam) != 1 {
		t.Fatalf("by-model=%d by-team=%d, want 2/1", len(s.ByModel), len(s.ByTeam))
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

func TestSummary_dayWindow(t *testing.T) {
	ix, _ := OpenSQLite(":memory:")
	defer ix.Close()
	_ = ix.Ingest(completed("d1", "2026-06-01T00:00:00Z", "t", "m", 1, 1, 0, 100))
	_ = ix.Ingest(completed("d2", "2026-06-29T00:00:00Z", "t", "m", 1, 1, 0, 200))
	s, err := ix.Summary(SummaryQuery{SinceDay: "2026-06-15"})
	if err != nil {
		t.Fatal(err)
	}
	if s.Totals.CostMicros != 200 {
		t.Fatalf("windowed cost = %d, want 200 (only d2)", s.Totals.CostMicros)
	}
}

func TestTimeSeries_clampsAndReturnsNonNil(t *testing.T) {
	ix, _ := OpenSQLite(":memory:")
	defer ix.Close()
	pts, err := ix.TimeSeries(TimeSeriesQuery{Days: 0})
	if err != nil {
		t.Fatalf("timeseries: %v", err)
	}
	if pts == nil {
		t.Fatal("timeseries returned nil slice; want empty non-nil")
	}
}

func TestReplay_dropsUnterminatedTail(t *testing.T) {
	ix, _ := OpenSQLite(":memory:")
	defer ix.Close()
	complete := `{"event":"request_completed","id":"c1","ts":"2026-06-29T00:00:00Z","principal":{"team":"t"},"request":{"model_resolved":"m"},"cost":{"amount_usd_micros":5}}` + "\n"
	partial := `{"event":"request_completed","id":"c2","ts":"2026-06-29T00:00:00Z","cost":{"amount_usd_micros":9}` // no closing brace, no newline (crash-truncated)
	n, err := ix.Replay(strings.NewReader(complete + partial))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("ingested %d, want 1 (the unterminated tail must be dropped)", n)
	}
	s, _ := ix.Summary(SummaryQuery{})
	if s.Totals.CostMicros != 5 {
		t.Fatalf("cost = %d, want 5", s.Totals.CostMicros)
	}
}
