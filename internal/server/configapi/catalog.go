package configapi

import (
	_ "embed"
	"encoding/json"
	"net/http"
)

//go:embed models_catalog.json
var modelsCatalogRaw []byte

// modelsCatalog maps provider type → known public model ids, parsed once at
// init from the embedded JSON. It backs the console's typeahead (ADR-014 D3); it
// is advisory only — the UI never blocks a save on catalog membership.
var modelsCatalog = func() map[string][]string {
	m := map[string][]string{}
	_ = json.Unmarshal(modelsCatalogRaw, &m)
	return m
}()

// CatalogHandler serves GET /admin/providers/catalog?type=<t> → {"models":[...]}.
// A missing type is 400; an unknown type returns an empty list (never 500), so
// the typeahead degrades to free-text.
func CatalogHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		typ := r.URL.Query().Get("type")
		if typ == "" {
			writeErr(w, http.StatusBadRequest, "type query parameter is required")
			return
		}
		models := modelsCatalog[typ]
		if models == nil {
			models = []string{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string][]string{"models": models})
	})
}
