// Package analyticsapi serves the console's read-only analytics over the
// derived analytics index (design spec §4 / D1). Handlers are full-admin gated
// at the mux (requireAdmin) and bound every query window (§13). The Querier
// interface keeps the server package off a direct analytics import.
package analyticsapi

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/inferplane/inferplane/internal/analytics"
)

// Querier is satisfied structurally by *analytics.Index and *pgstore.Store.
type Querier interface {
	Summary(analytics.SummaryQuery) (analytics.Summary, error)
	TimeSeries(analytics.TimeSeriesQuery) ([]analytics.DayPoint, error)
	Health() (analytics.Health, error)
	// Recent lists the most recent events for the console's Logs list (D4,
	// ADR-018), newest first, id-keyset paginated.
	Recent(limit int, before string) ([]analytics.Event, error)
}

const dayLayout = "2006-01-02"
const maxWindowDays = 366

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// boundedSummaryQuery applies the §13 window: an empty `since` defaults to 30
// days ago (UTC); `since`/`until` must be YYYY-MM-DD; the span may not exceed
// maxWindowDays. Returns ok=false (caller writes 400) on a violation.
func boundedSummaryQuery(q url.Values) (analytics.SummaryQuery, bool) {
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC) // midnight UTC

	since, until := q.Get("since"), q.Get("until")

	var sinceT time.Time
	if since == "" {
		sinceT = today.AddDate(0, 0, -30)
		since = sinceT.Format(dayLayout)
	} else {
		t, err := time.Parse(dayLayout, since)
		if err != nil {
			return analytics.SummaryQuery{}, false
		}
		sinceT = t
	}
	untilT := today // default upper bound = midnight today (no time-of-day remainder)
	if until != "" {
		t, err := time.Parse(dayLayout, until)
		if err != nil {
			return analytics.SummaryQuery{}, false
		}
		untilT = t
	}
	if untilT.Before(sinceT) { // reversed range
		return analytics.SummaryQuery{}, false
	}
	if untilT.Sub(sinceT) > maxWindowDays*24*time.Hour {
		return analytics.SummaryQuery{}, false
	}
	return analytics.SummaryQuery{SinceDay: since, UntilDay: until}, true
}

func SummaryHandler(q Querier) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		sq, ok := boundedSummaryQuery(r.URL.Query())
		if !ok {
			http.Error(w, "bad date range (use YYYY-MM-DD, max 366 days)", http.StatusBadRequest)
			return
		}
		out, err := q.Summary(sq)
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
		days, _ := strconv.Atoi(r.URL.Query().Get("days")) // 0 on parse error → Index defaults+clamps
		out, err := q.TimeSeries(analytics.TimeSeriesQuery{Days: days})
		if err != nil {
			http.Error(w, "analytics query failed", http.StatusInternalServerError)
			return
		}
		writeJSON(w, out)
	})
}

// HealthHandler serves /admin/analytics/health (ADR-015 §4) — the querier's
// own Health(), same for Mode A and Mode B.
func HealthHandler(q Querier) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h, err := q.Health()
		if err != nil {
			http.Error(w, "analytics health query failed", http.StatusInternalServerError)
			return
		}
		writeJSON(w, h)
	})
}

// RebuildHandler serves POST /admin/analytics/rebuild (ADR-015 §6) —
// operator-triggered recovery. Only a Querier that also implements
// analytics.Rebuilder supports this (Mode B; Mode A's *analytics.Index does
// not) — a runtime type assertion, not an interface requirement on Querier,
// so Mode A needs no no-op Rebuild method.
func RebuildHandler(q Querier) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		rb, ok := q.(analytics.Rebuilder)
		if !ok {
			http.Error(w, "rebuild not supported (analytics mode has no rebuild operation)", http.StatusMethodNotAllowed)
			return
		}
		if err := rb.Rebuild(r.Context()); err != nil {
			http.Error(w, "rebuild failed", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
