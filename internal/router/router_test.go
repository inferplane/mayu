package router

import (
	"sync"
	"testing"

	"github.com/inferplane/inferplane/internal/config"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/live"
	"github.com/inferplane/inferplane/internal/metrics"
	"github.com/inferplane/inferplane/providers"
	"github.com/inferplane/inferplane/providers/testing/mockprovider"
)

// newTestRouter wires a Router over a live.Holder built directly from the given
// providers/models (no registry needed). Identity = provider name (unique per
// provider) so the breaker keys deterministically in tests.
func newTestRouter(provs map[string]providers.Provider, models map[string]config.ModelConfig) (*Router, *live.Holder) {
	ids := make(map[string]string, len(provs))
	for name := range provs {
		ids[name] = name
	}
	h := &live.Holder{}
	h.Swap(live.NewState(provs, models, nil, ids))
	return New(h), h
}

func TestResolveModel(t *testing.T) {
	provs := map[string]providers.Provider{"anthropic-direct": mockprovider.New("claude-sonnet-4-6")}
	models := map[string]config.ModelConfig{
		"claude-sonnet-4-6": {Targets: []config.Target{{Provider: "anthropic-direct", Model: "claude-sonnet-4-6"}}},
	}
	r, _ := newTestRouter(provs, models)
	p, upstream, err := r.Resolve("claude-sonnet-4-6")
	if err != nil {
		t.Fatal(err)
	}
	if p.Name() != "mock" || upstream != "claude-sonnet-4-6" {
		t.Fatalf("resolve wrong: %s %s", p.Name(), upstream)
	}
	if _, _, err := r.Resolve("unknown-model"); err == nil {
		t.Fatal("expected error for unknown model")
	}
}

func TestResolveChainSkipsOpenBreaker(t *testing.T) {
	provs := map[string]providers.Provider{
		"a": mockprovider.New("m"), "b": mockprovider.New("m"),
	}
	models := map[string]config.ModelConfig{
		"m": {Targets: []config.Target{{Provider: "a", Model: "m1"}, {Provider: "b", Model: "m2"}}},
	}
	r, _ := newTestRouter(provs, models)
	// trip provider "a" breaker (5 failures)
	for i := 0; i < 5; i++ {
		r.RecordResult("a", "a", false)
	}
	chain, _, err := r.ResolveChain("m")
	if err != nil {
		t.Fatal(err)
	}
	// "a" open → chain should start with "b"
	if len(chain) == 0 || chain[0].ProviderName != "b" {
		t.Fatalf("open breaker not skipped: %+v", chain)
	}
}

func TestRecordResultSetsCircuitStateMetric(t *testing.T) {
	provs := map[string]providers.Provider{"p": mockprovider.New("m")}
	models := map[string]config.ModelConfig{"m": {Targets: []config.Target{{Provider: "p", Model: "m"}}}}
	r, _ := newTestRouter(provs, models)
	m := metrics.New()
	r.SetMetrics(m)

	// 5 consecutive failures (default threshold) → breaker opens → gauge = 2.
	for i := 0; i < 5; i++ {
		r.RecordResult("p", "p", false)
	}
	if got := circuitState(t, m); got != 2 {
		t.Fatalf("circuit_state after opening = %v, want 2 (open)", got)
	}
	// A success closes the breaker → gauge = 0.
	r.RecordResult("p", "p", true)
	if got := circuitState(t, m); got != 0 {
		t.Fatalf("circuit_state after success = %v, want 0 (closed)", got)
	}
}

// circuitState reads the single inferplane_circuit_state series value from the
// registry exposition (provider "p"), avoiding any test-only export.
func circuitState(t *testing.T, m *metrics.Metrics) float64 {
	t.Helper()
	mfs, err := m.Registry().Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "inferplane_circuit_state" {
			continue
		}
		for _, mc := range mf.GetMetric() {
			return mc.GetGauge().GetValue()
		}
	}
	t.Fatal("inferplane_circuit_state not found in exposition")
	return -1
}

// F6 code-gate follow-up: an allow-list ENTRY that names an alias (not just a
// REQUEST for an alias) must still grant access, canonicalized — an operator
// who configures allowed_models with an alias shouldn't get a permanently
// locked-out key. Still exact-match once canonicalized: no broadening.
func TestAllowsResolvesAliasInAllowList(t *testing.T) {
	provs := map[string]providers.Provider{"a": mockprovider.New("claude-sonnet-4-6")}
	models := map[string]config.ModelConfig{
		"claude-sonnet-4-6": {
			Aliases: []string{"apac.anthropic.claude-sonnet-4-6"},
			Targets: []config.Target{{Provider: "a", Model: "claude-sonnet-4-6"}},
		},
	}
	r, _ := newTestRouter(provs, models)

	withAlias := keystore.Principal{AllowedModels: []string{"apac.anthropic.claude-sonnet-4-6"}}
	if !r.Allows(withAlias, "claude-sonnet-4-6") {
		t.Fatal("an allow-list entry naming an alias must grant access to its canonical model")
	}

	withCanonical := keystore.Principal{AllowedModels: []string{"claude-sonnet-4-6"}}
	if !r.Allows(withCanonical, "claude-sonnet-4-6") {
		t.Fatal("canonical allow-list entry must still work (no regression)")
	}

	withOther := keystore.Principal{AllowedModels: []string{"some-other-model"}}
	if r.Allows(withOther, "claude-sonnet-4-6") {
		t.Fatal("an unrelated allow-list entry must not grant access")
	}

	wildcard := keystore.Principal{AllowedModels: []string{"*"}}
	if !r.Allows(wildcard, "claude-sonnet-4-6") {
		t.Fatal("wildcard must still allow")
	}
}

func TestResolveUnknownProvider(t *testing.T) {
	// model maps to a provider key that isn't in the providers map (config drift).
	provs := map[string]providers.Provider{} // empty
	models := map[string]config.ModelConfig{
		"m": {Targets: []config.Target{{Provider: "ghost", Model: "m"}}},
	}
	r, _ := newTestRouter(provs, models)
	if _, _, err := r.Resolve("m"); err == nil {
		t.Fatal("expected error when target points at unknown provider")
	}
}

// --- hot-reload support (plan 2026-06-13 task 2) ---

func TestResolveChainReturnsSnapshot(t *testing.T) {
	provs := map[string]providers.Provider{"a": mockprovider.New("m")}
	models := map[string]config.ModelConfig{"m": {Targets: []config.Target{{Provider: "a", Model: "m1"}}}}
	r, h := newTestRouter(provs, models)
	chain, st, err := r.ResolveChain("m")
	if err != nil || len(chain) != 1 {
		t.Fatalf("resolve: %v %+v", err, chain)
	}
	if st != h.Load() {
		t.Fatal("ResolveChain must return the snapshot it Loaded")
	}
	// A swap AFTER resolve must not change the snapshot this call returned.
	h.Swap(live.NewState(map[string]providers.Provider{}, map[string]config.ModelConfig{}, nil, nil))
	if _, ok := st.Route("m"); !ok {
		t.Fatal("returned snapshot changed under a later swap (not isolated)")
	}
}

func TestSwapChangesResolution(t *testing.T) {
	a := mockprovider.New("m")
	b := mockprovider.New("m")
	r, h := newTestRouter(
		map[string]providers.Provider{"a": a},
		map[string]config.ModelConfig{"m": {Targets: []config.Target{{Provider: "a", Model: "m1"}}}},
	)
	if _, _, err := r.Resolve("m"); err != nil {
		t.Fatal(err)
	}
	// Swap to a generation routing m → b only.
	h.Swap(live.NewState(
		map[string]providers.Provider{"b": b},
		map[string]config.ModelConfig{"m": {Targets: []config.Target{{Provider: "b", Model: "m2"}}}},
		nil, map[string]string{"b": "b"},
	))
	_, name, _, err := r.ResolveProvider("m")
	if err != nil || name != "b" {
		t.Fatalf("after swap resolve = %q %v, want b", name, err)
	}
	// A model removed by a swap resolves to an error.
	h.Swap(live.NewState(map[string]providers.Provider{}, map[string]config.ModelConfig{}, nil, nil))
	if _, _, err := r.Resolve("m"); err == nil {
		t.Fatal("removed model must error")
	}
}

func TestBreakerKeyedByIdentity(t *testing.T) {
	// Provider "a" with identity X, breaker tripped; swap "a" to identity Y
	// (re-pointed endpoint) → fresh breaker, allowed again.
	a := mockprovider.New("m")
	h := &live.Holder{}
	h.Swap(live.NewState(
		map[string]providers.Provider{"a": a},
		map[string]config.ModelConfig{"m": {Targets: []config.Target{{Provider: "a", Model: "m1"}}}},
		nil, map[string]string{"a": "anthropic\x00https://x"},
	))
	r := New(h)
	for i := 0; i < 5; i++ {
		r.RecordResult("a", "anthropic\x00https://x", false) // trip identity X
	}
	chain, _, _ := r.ResolveChain("m")
	if len(chain) != 1 { // all-open → returns all anyway, but breaker is open
		t.Fatalf("chain: %+v", chain)
	}
	// Re-point "a" to a new identity Y.
	h.Swap(live.NewState(
		map[string]providers.Provider{"a": a},
		map[string]config.ModelConfig{"m": {Targets: []config.Target{{Provider: "a", Model: "m1"}}}},
		nil, map[string]string{"a": "anthropic\x00https://y"},
	))
	// New identity has a fresh (closed) breaker — allowed.
	if !r.brk.Allow("anthropic\x00https://y") {
		t.Fatal("re-pointed provider must get a fresh breaker, not inherit stale-open state")
	}
}

func TestRetainPrunesAndStaleIdentityNeverRoutes(t *testing.T) {
	a := mockprovider.New("m")
	h := &live.Holder{}
	h.Swap(live.NewState(
		map[string]providers.Provider{"a": a},
		map[string]config.ModelConfig{"m": {Targets: []config.Target{{Provider: "a", Model: "m1"}}}},
		nil, map[string]string{"a": "idA"},
	))
	r := New(h)
	for i := 0; i < 5; i++ {
		r.RecordResult("a", "idA", false) // trip idA
	}
	// Swap to a generation WITHOUT "a", then prune: idA is dropped.
	h.Swap(live.NewState(map[string]providers.Provider{}, map[string]config.ModelConfig{}, nil, map[string]string{}))
	r.RetainBreakers(map[string]string{})
	if r.brk.State("idA") != 0 {
		t.Fatal("RetainBreakers must drop the pruned identity's state")
	}
	// A late in-flight RecordResult against the stale idA may transiently
	// recreate the entry, but ResolveChain only ever consults CURRENT-generation
	// identities — so the stale entry can never affect routing.
	r.RecordResult("a", "idA", false)
	// Bring "a" back under a NEW identity; routing uses the fresh (closed) breaker.
	h.Swap(live.NewState(
		map[string]providers.Provider{"a": a},
		map[string]config.ModelConfig{"m": {Targets: []config.Target{{Provider: "a", Model: "m1"}}}},
		nil, map[string]string{"a": "idA2"},
	))
	r.RetainBreakers(map[string]string{"a": "idA2"})
	chain, _, err := r.ResolveChain("m")
	if err != nil || len(chain) != 1 || chain[0].Identity != "idA2" {
		t.Fatalf("re-added provider must route on its fresh identity: %+v %v", chain, err)
	}
	if !r.brk.Allow("idA2") {
		t.Fatal("re-added provider's fresh breaker must be closed")
	}
}

func TestBreakerOpsRaceFree(t *testing.T) {
	a := mockprovider.New("m")
	h := &live.Holder{}
	mk := func(id string) *live.State {
		return live.NewState(
			map[string]providers.Provider{"a": a},
			map[string]config.ModelConfig{"m": {Targets: []config.Target{{Provider: "a", Model: "m1"}}}},
			nil, map[string]string{"a": id},
		)
	}
	h.Swap(mk("idA"))
	r := New(h)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 300; j++ {
				r.RecordResult("a", "idA", j%2 == 0)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 300; j++ {
			r.RetainBreakers(map[string]string{"a": "idA"})
		}
	}()
	wg.Wait()
}

// TestResolveChainRegionNilProviderConfigs (D7, ADR-020): a live.State built
// via NewState (no BuildState, as every existing test in this file does)
// carries a nil providerConfigs map — ChainTarget.Region must read as ""
// rather than panic on the nil-map lookup.
func TestResolveChainRegionNilProviderConfigs(t *testing.T) {
	provs := map[string]providers.Provider{"a": mockprovider.New("m")}
	models := map[string]config.ModelConfig{"m": {Targets: []config.Target{{Provider: "a", Model: "m1"}}}}
	r, _ := newTestRouter(provs, models)
	chain, _, err := r.ResolveChain("m")
	if err != nil || len(chain) != 1 {
		t.Fatalf("resolve: %v %+v", err, chain)
	}
	if chain[0].Region != "" {
		t.Fatalf("Region = %q, want \"\" (unlabeled)", chain[0].Region)
	}
}

func TestFilterRegions(t *testing.T) {
	chain := []ChainTarget{
		{ProviderName: "us", Region: "us"},
		{ProviderName: "eu", Region: "eu"},
		{ProviderName: "unlabeled", Region: ""},
	}
	// No policy → unrestricted, unchanged (and not even copied).
	if got := FilterRegions(chain, nil); len(got) != 3 {
		t.Fatalf("nil allowed: got %d targets, want 3 unchanged", len(got))
	}
	// Restricted to "eu" → only the eu target survives; the unlabeled target
	// is fail-closed dropped even though it was never explicitly excluded.
	got := FilterRegions(chain, []string{"eu"})
	if len(got) != 1 || got[0].ProviderName != "eu" {
		t.Fatalf("eu-restricted: got %+v, want only eu", got)
	}
	// Restricted to a region nothing matches → empty chain (caller 403s).
	if got := FilterRegions(chain, []string{"apac"}); len(got) != 0 {
		t.Fatalf("apac-restricted: got %+v, want empty", got)
	}
}
