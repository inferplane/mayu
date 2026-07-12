// Package configapi serves a read-only, secret-free view of the gateway's
// provider/model topology on the admin plane (ADR-005). Operators see WHICH
// providers are wired, their endpoints, their auth MODE, and the model
// routing/fallback order — but never a secret value. Registration itself
// stays in config (policy-as-code, ADR-003); UI-write is a deferred stage 2.
package configapi

import (
	"encoding/json"
	"net/http"
	"sort"

	"github.com/inferplane/inferplane/internal/config"
)

// ProviderView is the safe projection of a provider. It has NO field capable
// of holding a secret value — Auth carries only the ref NAME (env var / file
// path) or IAM mode, which are operationally essential and not sensitive.
type ProviderView struct {
	Name             string `json:"name"`
	Type             string `json:"type"`
	BaseURL          string `json:"base_url,omitempty"`
	Region           string `json:"region,omitempty"`            // bedrock region (non-secret) — lets the console prefill an edit
	GuardrailID      string `json:"guardrail_id,omitempty"`      // bedrock Guardrail ID (non-secret) — lets the console prefill an edit
	GuardrailVersion string `json:"guardrail_version,omitempty"` // bedrock Guardrail version (non-secret) — lets the console prefill an edit
	Auth             string `json:"auth"`
}

type TargetView struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	API      string `json:"api,omitempty"`
}

// ModelView lists a model's targets in priority order (target[0] is primary,
// the rest are the fallback chain).
type ModelView struct {
	Name    string       `json:"name"`
	Targets []TargetView `json:"targets"`
}

type View struct {
	Providers []ProviderView `json:"providers"`
	Models    []ModelView    `json:"models"`
	// Writable is true when a provider store is configured, so the console can
	// offer register/edit/delete forms (ADR-008); false → file-authoritative,
	// writes 405, the console shows the config-block guide only. It is NOT
	// secret-bearing — purely a capability hint set by the assembly layer.
	Writable bool `json:"writable"`
}

// ViewFrom builds the secret-free view from the loaded config. The auth string
// is derived ONLY from APIKeyRef (ref name) and Auth.Mode — never from the
// resolved APIKey field, which is the secret value.
func ViewFrom(providers map[string]config.ProviderConfig, models map[string]config.ModelConfig) View {
	v := View{Providers: make([]ProviderView, 0, len(providers)), Models: make([]ModelView, 0, len(models))}
	for name, p := range providers {
		v.Providers = append(v.Providers, ProviderView{
			Name:             name,
			Type:             p.Type,
			BaseURL:          p.BaseURL,
			Region:           p.Region,
			GuardrailID:      p.GuardrailID,
			GuardrailVersion: p.GuardrailVersion,
			Auth:             authString(p),
		})
	}
	sort.Slice(v.Providers, func(i, j int) bool { return v.Providers[i].Name < v.Providers[j].Name })

	for name, mc := range models {
		mv := ModelView{Name: name, Targets: make([]TargetView, 0, len(mc.Targets))}
		for _, t := range mc.Targets {
			mv.Targets = append(mv.Targets, TargetView{Provider: t.Provider, Model: t.Model, API: t.API})
		}
		v.Models = append(v.Models, mv)
	}
	sort.Slice(v.Models, func(i, j int) bool { return v.Models[i].Name < v.Models[j].Name })
	return v
}

// authString describes how the gateway authenticates to the provider, using
// only non-secret material: the env var name, the file path, or the IAM mode.
func authString(p config.ProviderConfig) string {
	if p.Type == "bedrock" {
		mode := p.Auth.Mode
		if mode == "" {
			mode = "default"
		}
		return "IAM · " + mode
	}
	label := "api key"
	if p.AuthHeader == "bearer" {
		label = "bearer"
	}
	if p.APIKeyRef != nil {
		switch {
		case p.APIKeyRef.Env != "":
			return label + " · env:" + p.APIKeyRef.Env
		case p.APIKeyRef.File != "":
			return label + " · file:" + p.APIKeyRef.File
		}
	}
	return "none (keyless)"
}

// Handler serves the view as JSON on GET. Writes return 405 — registration is
// config-driven (stage 2 UI-write is a separate ADR). The handler is mounted
// behind AdminAuth, so it is already authenticated when it runs. It takes a
// view PROVIDER (not a fixed value) so a config hot-reload (ADR-006) is
// reflected: each request derives the view from the current generation.
func Handler(view func() View) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"read-only"}`, http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(view())
	})
}
