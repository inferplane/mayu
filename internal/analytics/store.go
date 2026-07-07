package analytics

import "context"

// Health reports Mode A/B freshness for /admin/analytics/health.
type Health struct {
	Mode            string // "A" | "B"
	IsLeader        bool   // Mode A: always true. Mode B: this replica's lease state.
	LeaseEpoch      int64  // Mode A: 0.
	LagSeconds      int64  // Mode A: always 0.
	LastIngestTS    string // RFC3339Nano, "" if never ingested.
	SegmentsTracked int    // Mode A: 0.
}

// Store is the query surface analyticsapi depends on. Mode A (local SQLite)
// and Mode B (shared Postgres) both implement it.
type Store interface {
	Summary(SummaryQuery) (Summary, error)
	TimeSeries(TimeSeriesQuery) ([]DayPoint, error)
	Health() (Health, error)
}

// Rebuilder is implemented only by stores that support an operator-triggered
// rebuild.
type Rebuilder interface {
	Rebuild(context.Context) error
}
