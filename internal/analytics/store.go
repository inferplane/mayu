package analytics

import "context"

// Health reports Mode A/B freshness for /admin/analytics/health.
type Health struct {
	Mode            string `json:"mode"`             // "A" | "B"
	IsLeader        bool   `json:"is_leader"`        // Mode A: always true. Mode B: this replica's lease state.
	LeaseEpoch      int64  `json:"lease_epoch"`      // Mode A: 0.
	LagSeconds      int64  `json:"lag_seconds"`      // Mode A: always 0.
	LastIngestTS    string `json:"last_ingest_ts"`   // RFC3339Nano, "" if never ingested.
	SegmentsTracked int    `json:"segments_tracked"` // Mode A: 0.
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
