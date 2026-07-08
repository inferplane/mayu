// Capabilities is the secret-free feature/runtime map the console fetches on
// bootstrap (design spec §4.4) to render each section's enabled/disabled state
// without probing endpoints and catching 404/5xx. It carries booleans/enums
// only — never a secret value (§7). Same capability-hint concern as
// View.Writable, hence this package.
package configapi

import (
	"encoding/json"
	"net/http"
)

// Capabilities is the console's bootstrap view of which optional subsystems are
// live. Every field is a non-secret boolean or small enum.
type Capabilities struct {
	// AnalyticsIndex is "A" (local single-replica, Phase 1a), "B" (shared
	// Postgres store + fenced aggregator, Phase 1b, ADR-015), or "off". The
	// caller (cmd/inferplane gateway assembly) decides which per config —
	// this package only carries the value through, it never derives it.
	AnalyticsIndex      string `json:"analytics_index"`
	LogsBodies          bool   `json:"logs_bodies"`
	TeamsRecords        bool   `json:"teams_records"`
	KeyGovernanceFields bool   `json:"key_governance_fields"`
	ProviderStore       bool   `json:"provider_store"`
	RegionPolicy        bool   `json:"region_policy"`
	Guardrails          bool   `json:"guardrails"`
	BudgetAlerts        bool   `json:"budget_alerts"`
}

// CapabilitiesHandler serves the capability map (GET only, JSON). It is mounted
// behind AdminAuth (token-gated, §4.4). get is evaluated per request so a
// hot-reload that changes the topology is reflected without a restart.
func CapabilitiesHandler(get func() Capabilities) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(get())
	})
}
