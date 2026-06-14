package configapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/inferplane/inferplane/internal/config"
)

func exportSnapshot() ExportDoc {
	return ExportDocFrom(
		map[string]config.ProviderConfig{
			"anthropic-prod": {
				Type:      "anthropic",
				BaseURL:   "https://api.anthropic.com",
				APIKeyRef: &config.SecretRef{Env: "ANTHROPIC_KEY"},
				APIKey:    "sk-RESOLVED-SECRET-must-not-export", // resolved value — must NOT be serialized
			},
		},
		map[string]config.ModelConfig{
			"claude": {Targets: []config.Target{{Provider: "anthropic-prod", Model: "claude-sonnet-4-6"}}},
		},
	)
}

func TestExportSecretFree(t *testing.T) {
	h := ExportHandler(exportSnapshot)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/admin/config/export", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET export = %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "sk-RESOLVED-SECRET-must-not-export") {
		t.Fatalf("export leaked the resolved secret value: %s", body)
	}
	// The REF is exported (refs are non-secret operational data, ADR-005).
	if !strings.Contains(body, "ANTHROPIC_KEY") {
		t.Fatalf("export should carry the ref name: %s", body)
	}
}

func TestExportReParsesAsConfig(t *testing.T) {
	h := ExportHandler(exportSnapshot)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/admin/config/export", nil))

	var cfg config.Config
	if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
		t.Fatalf("export does not re-parse as config: %v", err)
	}
	p, ok := cfg.Providers["anthropic-prod"]
	if !ok || p.Type != "anthropic" || p.APIKeyRef == nil || p.APIKeyRef.Env != "ANTHROPIC_KEY" {
		t.Fatalf("round-trip provider wrong: %+v", p)
	}
	if cfg.Models["claude"].Targets[0].Provider != "anthropic-prod" {
		t.Fatalf("round-trip model wrong: %+v", cfg.Models)
	}
}

func TestExportRejectsWrite(t *testing.T) {
	h := ExportHandler(exportSnapshot)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("PUT", "/admin/config/export", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT export = %d, want 405", rec.Code)
	}
}
