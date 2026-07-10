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
	}
	if h.gov == nil {
		_ = json.NewEncoder(w).Encode(governance.UsageStatus{Team: p.Team})
		return
	}
	_ = json.NewEncoder(w).Encode(h.gov.UsageOf(p.Team, p.KeyID, kp))
}
