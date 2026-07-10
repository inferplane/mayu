package anthropicapi

import (
	"encoding/json"
	"net/http"
	"sort"

	"github.com/inferplane/inferplane/internal/principal"
	"github.com/inferplane/inferplane/internal/router"
	"github.com/inferplane/inferplane/pkg/schema"
)

type ModelsHandler struct{ r *router.Router }

func NewModelsHandler(r *router.Router) *ModelsHandler { return &ModelsHandler{r: r} }

// ServeHTTP returns the configured models in Anthropic's GET /v1/models shape.
// M2 returns all configured models; M3 filters by the virtual key's allow-list
// (design doc §3.1).
func (h *ModelsHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	names := h.r.AllModels()
	// M3: filter by the virtual key's allow-list. Absent principal (e.g. M2
	// callers / tests without auth) returns the full, unfiltered list.
	if p, ok := principal.From(req.Context()); ok {
		filtered := names[:0:0]
		for _, n := range names {
			if h.r.Allows(p, n) {
				filtered = append(filtered, n)
			}
		}
		names = filtered
	}
	sort.Strings(names) // deterministic order
	data := make([]schema.ModelInfo, 0, len(names))
	for _, n := range names {
		data = append(data, schema.ModelInfo{Type: "model", ID: n, DisplayName: n})
	}
	resp := map[string]any{"data": data, "has_more": false}
	if len(data) > 0 {
		resp["first_id"] = data[0].ID
		resp["last_id"] = data[len(data)-1].ID
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
