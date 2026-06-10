// Package router resolves a requested model name to a provider + upstream
// model id. M2 uses the first configured target only; priority fallback and
// circuit breaking arrive in M5 (design doc §4.5).
package router

import (
	"fmt"

	"github.com/inferplane/inferplane/internal/config"
	"github.com/inferplane/inferplane/providers"
)

type Router struct {
	provs  map[string]providers.Provider
	models map[string]config.ModelConfig
}

func New(provs map[string]providers.Provider, models map[string]config.ModelConfig) *Router {
	return &Router{provs: provs, models: models}
}

// Resolve returns the provider and upstream model id for a requested model.
func (r *Router) Resolve(model string) (providers.Provider, string, error) {
	mc, ok := r.models[model]
	if !ok || len(mc.Targets) == 0 {
		return nil, "", fmt.Errorf("router: no route for model %q", model)
	}
	t := mc.Targets[0] // M2: first target only
	p, ok := r.provs[t.Provider]
	if !ok {
		return nil, "", fmt.Errorf("router: model %q points at unknown provider %q", model, t.Provider)
	}
	return p, t.Model, nil
}

// ResolveProvider is like Resolve but also returns the CONFIG provider name
// (the key under `providers:` in config, e.g. "anthropic-direct"), which is the
// first element of the pricing table key. The provider's own Name() reports its
// TYPE ("anthropic"/"bedrock"), not the config name, so callers that key
// pricing must use this config name to stay consistent with Bundled() and
// config overrides.
func (r *Router) ResolveProvider(model string) (prov providers.Provider, providerName, upstream string, err error) {
	mc, ok := r.models[model]
	if !ok || len(mc.Targets) == 0 {
		return nil, "", "", fmt.Errorf("router: no route for model %q", model)
	}
	t := mc.Targets[0] // M2: first target only
	p, ok := r.provs[t.Provider]
	if !ok {
		return nil, "", "", fmt.Errorf("router: model %q points at unknown provider %q", model, t.Provider)
	}
	return p, t.Provider, t.Model, nil
}

// AllModels returns every configured model name (for /v1/models in M2; M3
// filters by the virtual key's allow-list).
func (r *Router) AllModels() []string {
	out := make([]string, 0, len(r.models))
	for name := range r.models {
		out = append(out, name)
	}
	return out
}
