package configapi

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/inferplane/inferplane/providers"
)

// HealthRecord is one provider's most recent periodic health-probe result
// (ADR-014 deferred item: periodic/auto health checks). LastProbedAt is
// RFC3339Nano UTC.
type HealthRecord struct {
	OK           bool   `json:"ok"`
	LatencyMS    int64  `json:"latency_ms"`
	Detail       string `json:"detail"`
	LastProbedAt string `json:"last_probed_at"`
}

// HealthStore is the cardinality-bounded (provider-name-keyed, never
// request-keyed) in-memory status cache a background prober writes to and
// the admin API reads from. Per-instance state, same posture as
// alert.Notifier's fired/recent maps (ADR-013 caveat).
type HealthStore struct {
	mu      sync.Mutex
	records map[string]HealthRecord
}

func NewHealthStore() *HealthStore {
	return &HealthStore{records: map[string]HealthRecord{}}
}

// Set records the given provider's latest probe result.
func (s *HealthStore) Set(name string, r providers.HealthResult, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[name] = HealthRecord{
		OK:           r.OK,
		LatencyMS:    r.LatencyMS,
		Detail:       r.Detail,
		LastProbedAt: at.UTC().Format(time.RFC3339Nano),
	}
}

// Snapshot returns a copy of the current status map -- callers may not
// observe or mutate the live map.
func (s *HealthStore) Snapshot() map[string]HealthRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]HealthRecord, len(s.records))
	for k, v := range s.records {
		out[k] = v
	}
	return out
}

// HealthHandler serves GET /admin/providers/health (ADR-014 deferred item):
// the periodic background prober's current per-provider status snapshot.
// snapshot is a closure (mirrors adminapi.AlertsHandler(recent func() []alert.Fire)'s
// exact shape), nil-safe -- a nil snapshot func (feature not configured) still
// serves an empty providers map, never an error.
func HealthHandler(snapshot func() map[string]HealthRecord) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var m map[string]HealthRecord
		if snapshot != nil {
			m = snapshot()
		}
		if m == nil {
			m = map[string]HealthRecord{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"providers": m})
	})
}
