package analyticsapi

import (
	"net/http"
	"strconv"
)

// LogsHandler serves GET /admin/logs (D4, ADR-018): the most recent request
// events, newest first, for the console's Logs list. `limit` (optional,
// clamped by Querier.Recent) and `before` (an event ID, for id-keyset
// pagination — "load more") are the only query params.
func LogsHandler(q Querier) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit")) // 0/invalid → Recent's default clamp
		before := r.URL.Query().Get("before")
		events, err := q.Recent(limit, before)
		if err != nil {
			http.Error(w, "analytics logs query failed", http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"events": events})
	})
}
