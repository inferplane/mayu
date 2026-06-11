// Package router resolves a requested model name to a provider + upstream
// model id. M2 uses the first configured target only; priority fallback and
// circuit breaking arrive in M5 (design doc §4.5).
package router

import (
	"fmt"
	"time"

	"github.com/inferplane/inferplane/internal/config"
	"github.com/inferplane/inferplane/internal/metrics"
	"github.com/inferplane/inferplane/providers"
)

type Router struct {
	provs   map[string]providers.Provider
	models  map[string]config.ModelConfig
	brk     *breaker
	metrics *metrics.Metrics // nil-safe: no-op when nil
}

func New(provs map[string]providers.Provider, models map[string]config.ModelConfig) *Router {
	// 5 consecutive failures → open, 1s base backoff (doubling, capped 30s).
	return &Router{provs: provs, models: models, brk: newBreaker(5, time.Second)}
}

// SetMetrics attaches the Prometheus metrics sink. The circuit-state gauge is
// updated on every RecordResult. Pass nil (or never call) to disable.
func (r *Router) SetMetrics(m *metrics.Metrics) { r.metrics = m }

// ChainTarget is one resolved fallback target: the provider instance, its
// CONFIG provider name (pricing/breaker key), and the upstream model id.
type ChainTarget struct {
	Provider     providers.Provider
	ProviderName string
	Upstream     string
}

// ResolveChain returns every configured target for a model in priority order,
// skipping providers whose circuit breaker is open. If ALL breakers are open
// it returns the full chain anyway (better to try than hard-fail). Targets
// pointing at an unknown provider are silently skipped.
func (r *Router) ResolveChain(model string) ([]ChainTarget, error) {
	mc, ok := r.models[model]
	if !ok || len(mc.Targets) == 0 {
		return nil, fmt.Errorf("router: no route for model %q", model)
	}
	var allowed, all []ChainTarget
	for _, t := range mc.Targets {
		p, ok := r.provs[t.Provider]
		if !ok {
			continue // config drift: target points at unknown provider
		}
		ct := ChainTarget{Provider: p, ProviderName: t.Provider, Upstream: t.Model}
		all = append(all, ct)
		if r.brk.Allow(t.Provider) {
			allowed = append(allowed, ct)
		}
	}
	if len(all) == 0 {
		return nil, fmt.Errorf("router: model %q points at unknown provider(s)", model)
	}
	if len(allowed) == 0 {
		return all, nil // all breakers open → try anyway
	}
	return allowed, nil
}

// RecordResult feeds a per-provider call outcome to the circuit breaker and
// reflects the resulting circuit state into the metrics gauge.
func (r *Router) RecordResult(providerName string, ok bool) {
	if ok {
		r.brk.RecordSuccess(providerName)
	} else {
		r.brk.RecordFailure(providerName)
	}
	r.metrics.SetCircuitState(providerName, r.brk.State(providerName))
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
