// Package router resolves a requested model name to a provider + upstream
// model id, with priority fallback and a per-provider circuit breaker
// (design doc §4.5). The topology (providers + routes) is read from a
// live.Holder so it can be hot-reloaded (ADR-006): ResolveChain takes one
// snapshot per call and returns it, so the caller bills the same generation.
// The breaker is keyed by provider IDENTITY (type+base_url) and persists
// across reloads for unchanged providers; RetainBreakers prunes the rest.
package router

import (
	"fmt"
	"time"

	"github.com/inferplane/inferplane/internal/live"
	"github.com/inferplane/inferplane/internal/metrics"
	"github.com/inferplane/inferplane/providers"
)

type Router struct {
	live    *live.Holder
	brk     *breaker
	metrics *metrics.Metrics // nil-safe: no-op when nil
}

func New(holder *live.Holder) *Router {
	// 5 consecutive failures → open, 1s base backoff (doubling, capped 30s).
	return &Router{live: holder, brk: newBreaker(5, time.Second)}
}

// SetMetrics attaches the Prometheus metrics sink. The circuit-state gauge is
// updated on every RecordResult. Pass nil (or never call) to disable.
func (r *Router) SetMetrics(m *metrics.Metrics) { r.metrics = m }

// ChainTarget is one resolved fallback target: the provider instance, its
// CONFIG provider name (pricing/metric key), the breaker Identity (type+base_url,
// captured from the generation this was resolved on so RecordResult records
// against the SAME generation — never a re-Loaded one), and the upstream model.
type ChainTarget struct {
	Provider     providers.Provider
	ProviderName string
	Identity     string
	Upstream     string
}

// ResolveChain returns every configured target for a model in priority order,
// skipping providers whose circuit breaker is open. If ALL breakers are open
// it returns the full chain anyway (better to try than hard-fail). Targets
// pointing at an unknown provider are silently skipped.
func (r *Router) ResolveChain(model string) ([]ChainTarget, *live.State, error) {
	st := r.live.Load() // one snapshot for this whole call — no mixed generations
	mc, ok := st.Route(model)
	if !ok || len(mc.Targets) == 0 {
		return nil, st, fmt.Errorf("router: no route for model %q", model)
	}
	var allowed, all []ChainTarget
	for _, t := range mc.Targets {
		p, ok := st.Provider(t.Provider)
		if !ok {
			continue // config drift: target points at unknown provider
		}
		id, _ := st.Identity(t.Provider)
		ct := ChainTarget{Provider: p, ProviderName: t.Provider, Identity: id, Upstream: t.Model}
		all = append(all, ct)
		if r.brk.Allow(id) {
			allowed = append(allowed, ct)
		}
	}
	if len(all) == 0 {
		return nil, st, fmt.Errorf("router: model %q points at unknown provider(s)", model)
	}
	if len(allowed) == 0 {
		return all, st, nil // all breakers open → try anyway
	}
	return allowed, st, nil
}

// RecordResult feeds a per-provider call outcome to the circuit breaker, keyed
// by the breaker IDENTITY captured when the request resolved (passed via the
// ChainTarget, NOT re-Loaded here) so the outcome is always recorded against
// the generation the call actually ran on. The metric label is the config
// provider name (cardinality-bounded). A stale identity whose provider was
// pruned by a concurrent reload is never consulted by ResolveChain (which only
// checks current-generation identities) and is reaped by the next reload's
// RetainBreakers, so recording against it is harmless.
func (r *Router) RecordResult(providerName, identity string, ok bool) {
	if ok {
		r.brk.RecordSuccess(identity)
	} else {
		r.brk.RecordFailure(identity)
	}
	r.metrics.SetCircuitState(providerName, r.brk.State(identity))
}

// RetainBreakers drops breaker entries whose identity is absent from the given
// generation (config name → identity), so a removed (or re-pointed) provider
// leaves no stale circuit state. Called by the reloader after a Swap.
func (r *Router) RetainBreakers(identities map[string]string) {
	keep := make(map[string]bool, len(identities))
	for _, id := range identities {
		keep[id] = true
	}
	r.brk.Retain(keep)
}

// Resolve returns the provider and upstream model id for a requested model.
func (r *Router) Resolve(model string) (providers.Provider, string, error) {
	st := r.live.Load()
	mc, ok := st.Route(model)
	if !ok || len(mc.Targets) == 0 {
		return nil, "", fmt.Errorf("router: no route for model %q", model)
	}
	t := mc.Targets[0]
	p, ok := st.Provider(t.Provider)
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
	st := r.live.Load()
	mc, ok := st.Route(model)
	if !ok || len(mc.Targets) == 0 {
		return nil, "", "", fmt.Errorf("router: no route for model %q", model)
	}
	t := mc.Targets[0]
	p, ok := st.Provider(t.Provider)
	if !ok {
		return nil, "", "", fmt.Errorf("router: model %q points at unknown provider %q", model, t.Provider)
	}
	return p, t.Provider, t.Model, nil
}

// AllModels returns every configured model name (for /v1/models; the ingress
// filters by the virtual key's allow-list).
func (r *Router) AllModels() []string {
	return r.live.Load().ModelNames()
}
