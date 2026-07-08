package adminapi

import (
	"encoding/json"
	"net/http"

	"github.com/inferplane/inferplane/internal/alert"
)

// AlertsHandler serves GET /admin/alerts/recent (D5b, ADR-017): the in-memory
// ring of recent budget-alert webhook fires. Mounted full-admin-only in
// server.go (requireAdmin) — a fire carries cross-team spend figures, the same
// posture as the analytics summary endpoints. recent is nil-safe (a Notifier
// with no configured webhook still may be reached with recent==nil).
func AlertsHandler(recent func() []alert.Fire) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var fires []alert.Fire
		if recent != nil {
			fires = recent()
		}
		if fires == nil {
			fires = []alert.Fire{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"fires": fires})
	})
}
