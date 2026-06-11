package openaiapi

import (
	"encoding/json"
	"net/http"
	"sort"

	"github.com/inferplane/inferplane/internal/principal"
	"github.com/inferplane/inferplane/internal/router"
)

type ModelsHandler struct{ r *router.Router }

func NewModelsHandler(r *router.Router) *ModelsHandler { return &ModelsHandler{r: r} }

// ServeHTTP returns the configured models in OpenAI's GET /v1/models shape:
// {"object":"list","data":[{"id","object":"model","owned_by":"inferplane"}]}.
// Filtered by the virtual key's allow-list when a principal is present (§3.1);
// an absent principal returns the full, unfiltered list (tests without auth).
func (h *ModelsHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	names := h.r.AllModels()
	if p, ok := principal.From(req.Context()); ok {
		filtered := names[:0:0]
		for _, n := range names {
			if p.Allows(n) {
				filtered = append(filtered, n)
			}
		}
		names = filtered
	}
	sort.Strings(names) // deterministic order
	data := make([]map[string]any, 0, len(names))
	for _, n := range names {
		data = append(data, map[string]any{
			"id":       n,
			"object":   "model",
			"owned_by": "inferplane",
		})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"object": "list",
		"data":   data,
	})
}
