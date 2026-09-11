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
	"log"
	"sort"
	"strconv"
	"strings"
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
	aliases    map[string]string
	pricing    *pricing.Table
	identities map[string]string // config provider name → identity (type+base_url)
	// providerConfigs is the source ProviderConfig per name, kept so the
	// assembly layer can derive the secret-free /admin/config view from the
	// live generation (set by BuildState; nil for NewState-only test states).
	providerConfigs map[string]config.ProviderConfig
	// fallbacks is the operator-declared model_fallbacks map (requested →
	// served), set by BuildState. Nil for NewState-only test states — FallbackFor
	// still works (falls through to the family heuristic, or "").
	fallbacks map[string]string
	// fallbackFamily enables the same-family default heuristic in FallbackFor
	// (config.Config.ModelFallbackFamily; default true). Set by BuildState.
	fallbackFamily bool
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
		mc := config.ModelConfig{
			Aliases:       append([]string(nil), v.Aliases...),
			Targets:       append([]config.Target(nil), v.Targets...),
			ContextWindow: v.ContextWindow,
		}
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
	return config.ModelConfig{
		Aliases:       append([]string(nil), mc.Aliases...),
		Targets:       append([]config.Target(nil), mc.Targets...),
		ContextWindow: mc.ContextWindow,
	}, true
}

// Canonical resolves a configured model alias to its canonical model name.
// Unknown names, including canonical names themselves, are returned unchanged.
func (s *State) Canonical(name string) string {
	if canonical, ok := s.aliases[name]; ok {
		return canonical
	}
	return name
}

// FallbackFor returns the model to serve in place of an unrouted `model`, or
// "" if there is none. An explicit config entry (model_fallbacks) wins; then,
// if enabled, the same-family default: among configured models sharing
// model's family (see familyOf), the highest version strictly below model's.
// Callers only consult this after Route has already failed, so `model` is
// never itself a routed name here.
func (s *State) FallbackFor(model string) string {
	if served, ok := s.fallbacks[model]; ok {
		return served
	}
	if !s.fallbackFamily {
		return ""
	}
	family, version, ok := familyOf(model)
	if !ok {
		return ""
	}
	best := ""
	var bestVersion []int
	for name := range s.models {
		f, v, ok := familyOf(name)
		if !ok || f != family || compareVersions(v, version) >= 0 {
			continue
		}
		if best == "" || compareVersions(v, bestVersion) > 0 {
			best, bestVersion = name, v
		}
	}
	return best
}

// familyOf splits a model name into its family and numeric version, treating
// the trailing run of all-numeric "-"-separated segments as the version
// ("claude-opus-4-8" -> "claude-opus", [4, 8]). A name with no numeric tail
// ("claude-sonnet-4-6-bedrock") has no version and ok is false — it is never a
// family-fallback candidate; an operator wanting it reached that way lists it
// explicitly in model_fallbacks.
func familyOf(name string) (family string, version []int, ok bool) {
	parts := strings.Split(name, "-")
	i := len(parts)
	for i > 0 {
		if _, err := strconv.Atoi(parts[i-1]); err != nil {
			break
		}
		i--
	}
	if i == len(parts) { // no numeric tail at all
		return "", nil, false
	}
	version = make([]int, len(parts)-i)
	for j, p := range parts[i:] {
		n, _ := strconv.Atoi(p) // already validated above
		version[j] = n
	}
	return strings.Join(parts[:i], "-"), version, true
}

// compareVersions compares two version-number slices lexicographically,
// treating a missing trailing component as 0 (so [4] < [4, 8]).
func compareVersions(a, b []int) int {
	for i := 0; i < len(a) || i < len(b); i++ {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
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
	a := make(map[string]string)
	for k, v := range models {
		m[k] = config.ModelConfig{
			Aliases:       append([]string(nil), v.Aliases...),
			Targets:       append([]config.Target(nil), v.Targets...),
			ContextWindow: v.ContextWindow,
		}
		for _, alias := range v.Aliases {
			a[alias] = k
		}
	}
	ids := make(map[string]string, len(identities))
	for k, v := range identities {
		ids[k] = v
	}
	return &State{providers: p, models: m, aliases: a, pricing: price, identities: ids}
}

// NewStateWithFallbacks is NewState plus model_fallbacks/the family heuristic
// flag, for tests that build a State directly (e.g. with mock providers,
// bypassing BuildState's real provider factory) but still need FallbackFor to
// behave as it would under BuildState.
func NewStateWithFallbacks(provs map[string]providers.Provider, models map[string]config.ModelConfig, price *pricing.Table, identities map[string]string, fallbacks map[string]string, family bool) *State {
	st := NewState(provs, models, price, identities)
	st.fallbacks = make(map[string]string, len(fallbacks))
	for k, v := range fallbacks {
		st.fallbacks[k] = v
	}
	st.fallbackFamily = family
	return st
}

// identityOf is the breaker/topology identity of a provider: a re-added or
// re-pointed provider (different type or base_url) gets a distinct identity, so
// stale circuit-breaker state never leaks to it.
func identityOf(name string, pc config.ProviderConfig) string {
	return pc.Type + "\x00" + pc.BaseURL
}

// Deps carries the optional cross-cutting dependencies provider construction
// needs but the topology builder must not OWN — the gateway builds them and
// live only passes them through, so this package's narrow import graph (config,
// providers, pricing) is unchanged. The zero value injects nothing and is
// byte-identical to the pre-ADR-040 behavior.
type Deps struct {
	// Credentials supplies rotating upstream credentials to a provider whose
	// auth mode opts in (bedrock's auth.mode "broker", ADR-040). nil is not a
	// silent downgrade: a broker-mode provider fails construction, which is
	// exactly the fail-closed posture the ADR requires (invariant #1).
	Credentials providers.CredentialSource
}

// BuildState constructs an immutable topology generation from config with no
// injected dependencies. It is BuildStateWith(cfg, Deps{}) — kept as the
// signature every existing caller and test uses.
func BuildState(cfg *config.Config) (*State, map[string]string, error) {
	return BuildStateWith(cfg, Deps{})
}

// BuildStateWith constructs an immutable topology generation from config: it
// builds every provider, builds the pricing table, validates that every model
// target references a provider that exists, and computes provider identities.
// It returns an error WITHOUT a State if anything fails, so callers (initial
// boot and reload alike) can fail safely. It touches no stateful component.
func BuildStateWith(cfg *config.Config, deps Deps) (*State, map[string]string, error) {
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
		// Credentials rides along for every provider, the same way HTTPClient
		// would: the field is documented as "nil ⇒ unchanged", and only
		// providers/bedrock reads it today (and only in auth.mode "broker").
		p, err := providers.New(providers.Config{Type: pc.Type, BaseURL: pc.BaseURL, APIKey: pc.APIKey, Settings: settings, Credentials: deps.Credentials})
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
	if err := validatePricingCoverage(cfg, tbl); err != nil {
		return nil, nil, err
	}
	st := NewState(provs, cfg.Models, tbl, identities)
	// Keep the source configs for the secret-free admin view (copy so the
	// published State is independent of the caller's cfg).
	st.providerConfigs = make(map[string]config.ProviderConfig, len(cfg.Providers))
	for k, v := range cfg.Providers {
		st.providerConfigs[k] = v
	}
	st.fallbacks = make(map[string]string, len(cfg.ModelFallbacks))
	for k, v := range cfg.ModelFallbacks {
		st.fallbacks[k] = v
	}
	st.fallbackFamily = cfg.FallbackFamilyEnabled()
	return st, identities, nil
}

// UnpricedTargets returns every (provider, upstream-model) pair a configured
// model routes to that has no rate in the table, sorted for stable output.
// Exported so `mayu pricing check` reports exactly what boot validation
// would reject — one predicate, no drift.
func UnpricedTargets(cfg *config.Config, tbl *pricing.Table) []string {
	seen := map[string]bool{}
	var out []string
	for _, mc := range cfg.Models {
		for _, t := range mc.Targets {
			if tbl.HasRate(t.Provider, t.Model) {
				continue
			}
			pair := t.Provider + "/" + t.Model
			if !seen[pair] {
				seen[pair] = true
				out = append(out, pair)
			}
		}
	}
	sort.Strings(out)
	return out
}

// validatePricingCoverage is the money guard for unpriced routes (ADR-030): a
// configured route with no rate silently billed 0 µUSD, with nothing but a
// boolean in the audit record to show for it.
//
// Strictness follows the operator's own `pricing.on_missing` declaration rather
// than overriding it:
//
//   - `block` — refuse to boot. The operator said unpriced traffic must never
//     be served, so serving it (even at 0) violates that. Runtime enforcement
//     in the governance pre-check covers the routes this can't see: models
//     added through UI-write, and hot-reloaded generations.
//   - `allow` (default) — log the unpriced routes loudly and continue. This is
//     a legitimate posture: a self-hosted vLLM deployment may genuinely have no
//     meaningful per-token price. Silence was the bug, not permissiveness.
//
// Either way `mayu pricing check` reports the same list for CI, so a
// newly-added model surfaces at deploy time instead of in next month's
// chargeback.
//
// The overrides cross-check is unconditional: a key naming a provider or model
// that does not exist is a typo, never a policy, and at runtime it is
// indistinguishable from a missing rate — it made that provider free forever.
func validatePricingCoverage(cfg *config.Config, tbl *pricing.Table) error {
	for provider, models := range cfg.Pricing.Overrides {
		if _, ok := cfg.Providers[provider]; !ok {
			return fmt.Errorf("live: pricing.overrides names unknown provider %q (a typo here silently prices that provider's traffic at 0)", provider)
		}
		for model := range models {
			if !routesTo(cfg, provider, model) {
				return fmt.Errorf("live: pricing.overrides[%q] names model %q, which no configured model routes to (check the upstream id, including any global./us./apac. prefix)", provider, model)
			}
		}
	}
	unpriced := UnpricedTargets(cfg, tbl)
	if len(unpriced) == 0 {
		return nil
	}
	if tbl.OnMissing() == pricing.OnMissingBlock {
		return fmt.Errorf("live: pricing.on_missing is \"block\" but %d configured route(s) have no rate: %s — declare them under pricing.overrides (input_per_mtok + output_per_mtok; cache rates derive automatically) or set on_missing to \"allow\"",
			len(unpriced), strings.Join(unpriced, ", "))
	}
	log.Printf("inferplane: WARNING %d configured route(s) have no pricing rate and will be billed 0 uUSD: %s — declare them under pricing.overrides, or set pricing.on_missing to \"block\" to refuse them",
		len(unpriced), strings.Join(unpriced, ", "))
	return nil
}

// routesTo reports whether any configured model routes to this exact
// (provider, upstream) pair. Matching is exact on purpose: an override keyed
// to a region-prefixed id is a deliberate per-prefix rate and must correspond
// to a real target, while the unprefixed base id is matched via the same
// prefix-stripping rule the rate lookup uses.
func routesTo(cfg *config.Config, provider, model string) bool {
	for _, mc := range cfg.Models {
		for _, t := range mc.Targets {
			if t.Provider != provider {
				continue
			}
			if t.Model == model || strings.HasSuffix(t.Model, "."+model) {
				return true
			}
		}
	}
	return false
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
				// Free must ride along or every free:true override silently
				// becomes an Unpriced() row here — the exact inversion of the
				// bug this field exists to fix.
				Free: rc.Free,
			}
		}
	}
	return pricing.FromConfigVersioned(cfg.Pricing.OnMissing, cfg.Pricing.Version, overrides)
}

// PricingTableFor builds the rate table this config would run with, without
// constructing providers. Exported so `mayu pricing check` reports against
// exactly the table BuildState validates — including the derived cache rates and
// the Bedrock region-prefix fallback — rather than reimplementing the assembly.
func PricingTableFor(cfg *config.Config) *pricing.Table { return pricingFromConfig(cfg) }
