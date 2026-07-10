package live

import (
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"sync"
	"testing"

	"github.com/inferplane/inferplane/internal/config"

	// Provider factories register via init() — blank-imported here (as
	// cmd/inferplane/main.go does) so BuildState's providers.New can resolve
	// real provider types in this unit test.
	_ "github.com/inferplane/inferplane/providers/anthropic"
	_ "github.com/inferplane/inferplane/providers/bedrock"
	_ "github.com/inferplane/inferplane/providers/openaicompat"
)

func sampleConfig() *config.Config {
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"anthropic-direct": {Type: "anthropic", BaseURL: "https://api.anthropic.com", APIKey: "sk-x"},
		},
		Models: map[string]config.ModelConfig{
			"claude": {Targets: []config.Target{{Provider: "anthropic-direct", Model: "claude-sonnet-4-6"}}},
		},
	}
	cfg.Pricing.OnMissing = "allow"
	return cfg
}

func TestBuildStateValidatesRoutes(t *testing.T) {
	// A model target naming a missing provider must fail the build (no State).
	cfg := sampleConfig()
	cfg.Models["bad"] = config.ModelConfig{Targets: []config.Target{{Provider: "ghost", Model: "m"}}}
	if _, _, err := BuildState(cfg); err == nil {
		t.Fatal("BuildState must reject a model target → missing provider")
	}

	// The clean config builds.
	st, ids, err := BuildState(sampleConfig())
	if err != nil {
		t.Fatalf("BuildState: %v", err)
	}
	if st == nil || len(st.Providers()) != 1 || st.Pricing() == nil {
		t.Fatalf("incomplete state: %+v", st)
	}
	if ids["anthropic-direct"] == "" {
		t.Fatalf("identity not computed: %v", ids)
	}
}

// TestState_Region (D7, ADR-020): Region reads the provider's config label,
// any provider type — not just bedrock — and returns "" for an unlabeled or
// unknown provider (the fail-closed default for a restricted team).
func TestState_Region(t *testing.T) {
	cfg := sampleConfig()
	pc := cfg.Providers["anthropic-direct"]
	pc.Region = "eu"
	cfg.Providers["anthropic-direct"] = pc
	st, _, err := BuildState(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Region("anthropic-direct"); got != "eu" {
		t.Fatalf("Region(anthropic-direct) = %q, want %q", got, "eu")
	}
	if got := st.Region("unknown"); got != "" {
		t.Fatalf("Region(unknown) = %q, want \"\"", got)
	}
}

func TestNewStateDeepCopies(t *testing.T) {
	cfg := sampleConfig()
	st, _, err := BuildState(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Mutating the caller's source maps after build must not affect the state.
	cfg.Models["claude"].Targets[0].Model = "MUTATED"
	cfg.Models["injected"] = config.ModelConfig{}
	if m, ok := st.Models()["claude"]; !ok || m.Targets[0].Model != "claude-sonnet-4-6" {
		t.Fatalf("state models not frozen: %+v", st.Models())
	}
	if _, ok := st.Models()["injected"]; ok {
		t.Fatal("state models map shares backing with caller")
	}
	// Mutating the accessor's returned value must not affect internals either.
	got := st.Models()
	got["claude"] = config.ModelConfig{Targets: []config.Target{{Provider: "evil"}}}
	if st.Models()["claude"].Targets[0].Provider != "anthropic-direct" {
		t.Fatal("accessor leaks mutable internal map")
	}
}

func TestHolderLoadSwap(t *testing.T) {
	h := &Holder{}
	s1, _, _ := BuildState(sampleConfig())
	h.Swap(s1)
	if h.Load() != s1 {
		t.Fatal("Load did not return the swapped state")
	}
	s2, _, _ := BuildState(sampleConfig())
	h.Swap(s2)
	if h.Load() != s2 {
		t.Fatal("Load did not return the latest swap")
	}
}

func TestHolderRaceFree(t *testing.T) {
	h := &Holder{}
	s, _, _ := BuildState(sampleConfig())
	h.Swap(s)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_ = h.Load().Providers()
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 200; j++ {
			ns, _, _ := BuildState(sampleConfig())
			h.Swap(ns)
		}
	}()
	wg.Wait()
}

// TestLiveImportsAreLeafSafe makes the topology-only boundary STRUCTURAL: this
// package must never import the stateful constructors (governance/keystore/
// audit) or the server packages — reload cannot then accidentally rebuild
// safety-critical state (P2 r2).
func TestLiveImportsAreLeafSafe(t *testing.T) {
	fset := token.NewFileSet()
	// Guard the PRODUCTION boundary only — test scaffolding (this file's
	// blank-imports of provider packages) is not part of the package's
	// shipped dependency surface.
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	banned := []string{
		"internal/governance", "internal/keystore", "internal/audit",
		"internal/server", "internal/limiter", "internal/budget",
	}
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			for _, imp := range f.Imports {
				p := strings.Trim(imp.Path.Value, `"`)
				for _, b := range banned {
					if strings.Contains(p, b) {
						t.Errorf("internal/live must not import %q (leaf/topology-only boundary)", p)
					}
				}
			}
		}
	}
}

// Task 2: BuildState builds an alias→canonical map; State.Canonical resolves an
// alias to its canonical model name and is the identity for anything else.
func TestBuildStateAliases(t *testing.T) {
	cfg := sampleConfig()
	mc := cfg.Models["claude"]
	mc.Aliases = []string{"apac.anthropic.claude-sonnet-4-6"}
	cfg.Models["claude"] = mc
	st, _, err := BuildState(cfg)
	if err != nil {
		t.Fatalf("BuildState with alias: %v", err)
	}
	if got := st.Canonical("apac.anthropic.claude-sonnet-4-6"); got != "claude" {
		t.Fatalf("alias must resolve to canonical: got %q want %q", got, "claude")
	}
	if got := st.Canonical("claude"); got != "claude" {
		t.Fatalf("canonical name must be identity: got %q", got)
	}
	if got := st.Canonical("unknown-xyz"); got != "unknown-xyz" {
		t.Fatalf("unknown name must be identity: got %q", got)
	}
	// Route still only accepts canonical names.
	if _, ok := st.Route("apac.anthropic.claude-sonnet-4-6"); ok {
		t.Fatal("Route must not resolve an alias directly")
	}
	if _, ok := st.Route("claude"); !ok {
		t.Fatal("Route must resolve the canonical name")
	}
}
