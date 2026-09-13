package usageapi

import (
	"encoding/json"
	"net/http"

	"github.com/inferplane/inferplane/internal/governance"
	"github.com/inferplane/inferplane/internal/principal"
)

type Handler struct {
	gov *governance.Governor
}

func NewHandler(gov *governance.Governor) *Handler {
	return &Handler{gov: gov}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	p, ok := principal.From(r.Context())
	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "no principal"})
		return
	}
	kp := governance.KeyPolicy{
		RatePerMin:           p.RPM,
		TokensPerMinute:      p.TPM,
		BudgetMicrosPerMonth: p.BudgetUSDMicros,
		BudgetMicrosPerDay:   p.BudgetUSDMicrosPerDay,
	}
	if h.gov == nil {
		_ = json.NewEncoder(w).Encode(governance.UsageStatus{Team: p.Team})
		return
	}
	if h.gov.HasSharedAuthority() {
		limits, err := h.gov.SharedAuthority().SharedUsage(r.Context(), governance.Subject{Team: p.Team, KeyID: p.KeyID, User: p.Owner})
		if err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "shared usage unavailable"})
			return
		}
		_ = json.NewEncoder(w).Encode(governance.UsageStatus{Team: p.Team, EnforcementMode: "shared", SharedLimits: limits})
		return
	}
	_ = json.NewEncoder(w).Encode(h.gov.UsageOf(governance.Subject{Team: p.Team, KeyID: p.KeyID, User: p.Owner}, kp))
}
