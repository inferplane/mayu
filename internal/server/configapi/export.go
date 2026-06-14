package configapi

import (
	"encoding/json"
	"net/http"

	"github.com/inferplane/inferplane/internal/config"
)

// ExportDoc is the secret-free, config-shaped projection of the current
// effective topology (ADR-008 §3, the "Git export" half of DB-authoritative-
// with-Git-export). It marshals as a config fragment an operator can commit:
// providers + models with REFS only. The secret-free guarantee is structural —
// config.ProviderConfig.APIKey is tagged `json:"-"`, so a resolved key can never
// be serialized here even though the live snapshot carries it.
type ExportDoc struct {
	Providers map[string]config.ProviderConfig `json:"providers"`
	Models    map[string]config.ModelConfig    `json:"models"`
}

// ExportDocFrom builds the export doc from a topology snapshot (the live
// generation's provider configs + model routes). The APIKey field on each
// ProviderConfig is dropped by its `json:"-"` tag at marshal time.
func ExportDocFrom(providers map[string]config.ProviderConfig, models map[string]config.ModelConfig) ExportDoc {
	return ExportDoc{Providers: providers, Models: models}
}

// ExportHandler serves GET /admin/config/export as a secret-free config
// fragment. It is read-only (writes 405) and safe to mount unconditionally —
// with no provider store it simply exports the file topology (ADR-008 gate C7).
func ExportHandler(snapshot func() ExportDoc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"read-only"}`, http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ") // human-committable to Git
		_ = enc.Encode(snapshot())
	})
}
