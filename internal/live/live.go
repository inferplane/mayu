// Package live holds the gateway's reloadable topology as a single immutable
// generation (providers, model routes, pricing table) behind one atomic
// pointer, so a hot reload publishes the whole generation in one Swap and a
// reader never observes a mixed generation (ADR-006).
//
// It is also the TOPOLOGY-ONLY builder boundary: it imports only config,
// providers, and pricing — never the stateful constructors (governance,
// keystore, audit) or the server packages — so a reload cannot rebuild or
// reset safety-critical state. An import-guard test enforces this structurally.
package live

import (
	"encoding/json"
	"fmt"
	"sync/atomic"

	"github.com/inferplane/inferplane/internal/config"
	"github.com/inferplane/inferplane/internal/pricing"
	"github.com/inferplane/inferplane/providers"
)

// Holder publishes the current topology generation behind an atomic pointer:
// lock-free Load for readers, single-store Swap for the reloader. Every reader
// of a reloadable thing (router topology, pricing) goes through one Load.
type Holder struct {
	p atomic.Pointer[State]
}

// Load returns the current generation (nil before the first Swap).
func (h *Holder) Load() *State { return h.p.Load() }

// Swap atomically publishes a new generation; all consumers flip together.
func (h *Holder) Swap(s *State) { h.p.Store(s) }

// State is one immutable topology generation. All fields are unexported and
// frozen at construction (NewState/BuildState deep-copy mutable inputs);
// accessors return copies so a published State can never be mutated.
type State struct {
	providers  map[string]providers.Provider
	models     map[string]config.ModelConfig
	pricing    *pricing.Table
	identities map[string]string // config provider name → identity (type+base_url)
	// providerConfigs is the source ProviderConfig per name, kept so the
	// assembly layer can derive the secret-free /admin/config view from the
	// live generation (set by BuildState; nil for NewState-only test states).
	providerConfigs map[string]config.ProviderConfig
}

// Providers returns the provider instances by config name. The map is a copy;
// the provider VALUES are shared by reference (providers are concurrency-safe
// and identity-stable within a generation).
func (s *State) Providers() map[string]providers.Provider {
	out := make(map[string]providers.Provider, len(s.providers))
	for k, v := range s.providers {
		out[k] = v
	}
	return out
}

// Models returns a deep copy of the model routes (the Targets slices are
// copied too, so callers cannot mutate the frozen generation).
func (s *State) Models() map[string]config.ModelConfig {
	out := make(map[string]config.ModelConfig, len(s.models))
	for k, v := range s.models {
		mc := config.ModelConfig{Targets: append([]config.Target(nil), v.Targets...)}
		out[k] = mc
	}
	return out
}

// Pricing returns the generation's pricing table (immutable).
func (s *State) Pricing() *pricing.Table { return s.pricing }

// Route returns a copy of the model's config (the Targets slice is copied so a
// caller can never mutate the published generation — the immutability invariant
// holds through every accessor). The copy is a tiny slice (1–3 targets),
// negligible against the upstream call.
func (s *State) Route(model string) (config.ModelConfig, bool) {
	mc, ok := s.models[model]
	if !ok {
		return config.ModelConfig{}, false
	}
	return config.ModelConfig{Targets: append([]config.Target(nil), mc.Targets...)}, true
}

// Provider returns the built provider for a config name (read-only).
func (s *State) Provider(name string) (providers.Provider, bool) {
	p, ok := s.providers[name]
	return p, ok
}

// ModelNames returns every configured model name (order unspecified).
func (s *State) ModelNames() []string {
	out := make([]string, 0, len(s.models))
	for name := range s.models {
		out = append(out, name)
	}
	return out
}

// Identities returns a copy of the config-name → identity (type+base_url) map.
func (s *State) Identities() map[string]string {
	out := make(map[string]string, len(s.identities))
	for k, v := range s.identities {
		out[k] = v
	}
	return out
}

// Identity returns the identity string for a config provider name, if present.
func (s *State) Identity(name string) (string, bool) {
	id, ok := s.identities[name]
	return id, ok
}

// Region returns the configured region label for a provider (D7, ADR-020), or
// "" if unlabeled. Any provider type may carry a region label, not just
// bedrock — it is a generic topology attribute.
func (s *State) Region(name string) string {
	return s.providerConfigs[name].Region
}

// ProviderConfigs returns a copy of the source provider configs, for deriving
// the secret-free admin view (live never imports the view package). The
// returned configs still carry the resolved APIKey — the view layer drops it.
func (s *State) ProviderConfigs() map[string]config.ProviderConfig {
	out := make(map[string]config.ProviderConfig, len(s.providerConfigs))
	for k, v := range s.providerConfigs {
		out[k] = v
	}
	return out
}

// NewState freezes the given topology into an immutable State, deep-copying the
// maps and nested slices. Callers may mutate their inputs afterward without
// affecting the published State.
func NewState(provs map[string]providers.Provider, models map[string]config.ModelConfig, price *pricing.Table, identities map[string]string) *State {
	p := make(map[string]providers.Provider, len(provs))
	for k, v := range provs {
		p[k] = v
	}
	m := make(map[string]config.ModelConfig, len(models))
	for k, v := range models {
		m[k] = config.ModelConfig{Targets: append([]config.Target(nil), v.Targets...)}
	}
	ids := make(map[string]string, len(identities))
	for k, v := range identities {
		ids[k] = v
	}
	return &State{providers: p, models: m, pricing: price, identities: ids}
}

// identityOf is the breaker/topology identity of a provider: a re-added or
// re-pointed provider (different type or base_url) gets a distinct identity, so
// stale circuit-breaker state never leaks to it.
func identityOf(name string, pc config.ProviderConfig) string {
	return pc.Type + "\x00" + pc.BaseURL
}

// BuildState constructs an immutable topology generation from config: it builds
// every provider, builds the pricing table, validates that every model target
// references a provider that exists, and computes provider identities. It
// returns an error WITHOUT a State if anything fails, so callers (initial boot
// and reload alike) can fail safely. It touches no stateful component.
func BuildState(cfg *config.Config) (*State, map[string]string, error) {
	// model_api[providerName] = {upstreamModelID: api} so the bedrock factory
	// can override invoke/converse routing per upstream model.
	modelAPIByProvider := map[string]map[string]string{}
	for _, mc := range cfg.Models {
		for _, t := range mc.Targets {
			if t.API != "" {
				if modelAPIByProvider[t.Provider] == nil {
					modelAPIByProvider[t.Provider] = map[string]string{}
				}
				modelAPIByProvider[t.Provider][t.Model] = t.API
			}
		}
	}

	provs := make(map[string]providers.Provider, len(cfg.Providers))
	identities := make(map[string]string, len(cfg.Providers))
	for name, pc := range cfg.Providers {
		var settings map[string]string
		if pc.Type == "anthropic" && pc.AuthHeader != "" {
			settings = map[string]string{"auth_header": pc.AuthHeader}
		}
		if pc.Type == "bedrock" {
			settings = map[string]string{
				"region":            pc.Region,
				"auth_mode":         pc.Auth.Mode,
				"profile":           pc.Auth.Profile,
				"guardrail_id":      pc.GuardrailID,
				"guardrail_version": pc.GuardrailVersion,
			}
			if m := modelAPIByProvider[name]; len(m) > 0 {
				b, _ := json.Marshal(m)
				settings["model_api"] = string(b)
			}
		}
		p, err := providers.New(providers.Config{Type: pc.Type, BaseURL: pc.BaseURL, APIKey: pc.APIKey, Settings: settings})
		if err != nil {
			return nil, nil, fmt.Errorf("live: provider %q: %w", name, err)
		}
		provs[name] = p
		identities[name] = identityOf(name, pc)
	}

	// Validate every model target references a provider that exists — a route
	// to a missing provider must never be published.
	for model, mc := range cfg.Models {
		for _, t := range mc.Targets {
			if _, ok := provs[t.Provider]; !ok {
				return nil, nil, fmt.Errorf("live: model %q targets unknown provider %q", model, t.Provider)
			}
		}
	}

	tbl := pricingFromConfig(cfg)
	st := NewState(provs, cfg.Models, tbl, identities)
	// Keep the source configs for the secret-free admin view (copy so the
	// published State is independent of the caller's cfg).
	st.providerConfigs = make(map[string]config.ProviderConfig, len(cfg.Providers))
	for k, v := range cfg.Providers {
		st.providerConfigs[k] = v
	}
	return st, identities, nil
}

// pricingFromConfig mirrors the gateway's pricing assembly (kept here so the
// topology-only builder owns the full generation).
func pricingFromConfig(cfg *config.Config) *pricing.Table {
	overrides := map[string]map[string]pricing.ConfigRate{}
	for provider, models := range cfg.Pricing.Overrides {
		overrides[provider] = map[string]pricing.ConfigRate{}
		for model, rc := range models {
			overrides[provider][model] = pricing.ConfigRate{
				InputPerMTok:        rc.InputPerMTok,
				OutputPerMTok:       rc.OutputPerMTok,
				CacheReadPerMTok:    rc.CacheReadPerMTok,
				CacheWrite5mPerMTok: rc.CacheWrite5mPerMTok,
				CacheWrite1hPerMTok: rc.CacheWrite1hPerMTok,
			}
		}
	}
	return pricing.FromConfig(cfg.Pricing.OnMissing, overrides)
}
