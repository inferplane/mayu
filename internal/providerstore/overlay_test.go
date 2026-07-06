package providerstore

import (
	"context"
	"testing"

	"github.com/inferplane/inferplane/internal/config"
)

func fileCfg() *config.Config {
	c := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"file-prov": {Type: "anthropic", BaseURL: "https://file", APIKeyRef: &config.SecretRef{Env: "FILE_KEY"}},
		},
		Models: map[string]config.ModelConfig{
			"file-model": {Targets: []config.Target{{Provider: "file-prov", Model: "x"}}},
		},
		Teams:   map[string]config.TeamConfig{"demo": {AllowedModels: []string{"*"}}},
		Pricing: config.PricingConfig{OnMissing: "block"},
	}
	c.Server.Listen = ":8080"
	return c
}

func TestOverlayReplacesTopologyKeepsRest(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	_ = s.UpsertProvider(ctx, ProviderRow{Name: "db-prov", Type: "anthropic", BaseURL: "https://db", APIKeyRefEnv: "DB_KEY"})
	_ = s.SetModel(ctx, "db-model", []Target{{Provider: "db-prov", Model: "y", API: "converse"}})

	eff, err := Overlay(fileCfg(), s)
	if err != nil {
		t.Fatalf("Overlay: %v", err)
	}
	// Topology comes from the DB.
	if _, ok := eff.Providers["db-prov"]; !ok {
		t.Fatalf("DB provider missing from overlay: %+v", eff.Providers)
	}
	if _, ok := eff.Providers["file-prov"]; ok {
		t.Fatal("file provider must be replaced, not merged")
	}
	if eff.Providers["db-prov"].APIKeyRef == nil || eff.Providers["db-prov"].APIKeyRef.Env != "DB_KEY" {
		t.Fatalf("db provider ref not mapped: %+v", eff.Providers["db-prov"].APIKeyRef)
	}
	mt := eff.Models["db-model"].Targets
	if len(mt) != 1 || mt[0].Provider != "db-prov" || mt[0].API != "converse" {
		t.Fatalf("db model not mapped: %+v", mt)
	}
	if _, ok := eff.Models["file-model"]; ok {
		t.Fatal("file model must be replaced")
	}
	// Non-topology fields stay from the file.
	if eff.Server.Listen != ":8080" || eff.Pricing.OnMissing != "block" {
		t.Fatalf("non-topology config not preserved: %+v / %+v", eff.Server, eff.Pricing)
	}
	if _, ok := eff.Teams["demo"]; !ok {
		t.Fatal("teams not preserved from file")
	}
}

// TestOverlayNoSecretMaterial: Overlay maps refs only; it never resolves, so the
// returned config carries APIKeyRef but an EMPTY APIKey (resolution is the
// caller's job). No secret value is produced by Overlay.
func TestOverlayNoSecretMaterial(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	_ = s.UpsertProvider(ctx, ProviderRow{Name: "p", Type: "anthropic", APIKeyRefEnv: "K"})
	eff, err := Overlay(fileCfg(), s)
	if err != nil {
		t.Fatal(err)
	}
	if got := eff.Providers["p"].APIKey; got != "" {
		t.Fatalf("Overlay must not resolve secrets; APIKey=%q", got)
	}
}

// TestOverlayIgnoredFileProvidersNotResolved (composes with G1): Overlay drops
// file providers entirely, so a file provider with an unset ref never causes a
// resolution error in the overlay path.
func TestOverlayIgnoredFileProvidersNotResolved(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	_ = s.UpsertProvider(ctx, ProviderRow{Name: "db-prov", Type: "anthropic"})
	// fileCfg()'s file-prov references FILE_KEY (never set) — must not matter.
	if _, err := Overlay(fileCfg(), s); err != nil {
		t.Fatalf("Overlay must not resolve/inspect ignored file provider refs: %v", err)
	}
}

// TestAuthHeaderRoundTripsThroughRowConversion (PR #13 review, Finding 1): a
// file provider's auth_header must survive rowFromProviderConfig → (DB) →
// providerConfigFromRow, or an OpenRouter-style provider silently regresses to
// x-api-key the moment the provider store seeds/overlays it.
func TestAuthHeaderRoundTripsThroughRowConversion(t *testing.T) {
	pc := config.ProviderConfig{Type: "anthropic", BaseURL: "https://openrouter.ai/api", AuthHeader: "bearer"}
	row := rowFromProviderConfig("openrouter", pc)
	if row.AuthHeader != "bearer" {
		t.Fatalf("rowFromProviderConfig dropped auth_header: %+v", row)
	}
	back := providerConfigFromRow(row)
	if back.AuthHeader != "bearer" {
		t.Fatalf("providerConfigFromRow dropped auth_header: %+v", back)
	}
}

// TestOverlayPreservesAuthHeader is the same guarantee at the seed→overlay
// level (SeedIfEmpty writes the row, Overlay reads it back).
func TestOverlayPreservesAuthHeader(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	cfg := fileCfg()
	cfg.Providers["file-prov"] = config.ProviderConfig{Type: "anthropic", BaseURL: "https://openrouter.ai/api", APIKeyRef: &config.SecretRef{Env: "FILE_KEY"}, AuthHeader: "bearer"}
	if err := SeedIfEmpty(ctx, s, cfg); err != nil {
		t.Fatal(err)
	}
	eff, err := Overlay(cfg, s)
	if err != nil {
		t.Fatal(err)
	}
	if eff.Providers["file-prov"].AuthHeader != "bearer" {
		t.Fatalf("auth_header lost across seed→overlay: %+v", eff.Providers["file-prov"])
	}
}

func TestSeedIfEmptySeedsOnceFromFile(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := SeedIfEmpty(ctx, s, fileCfg()); err != nil {
		t.Fatalf("SeedIfEmpty: %v", err)
	}
	pl, _ := s.ListProviders(ctx)
	ml, _ := s.ListModels(ctx)
	if len(pl) != 1 || pl[0].Name != "file-prov" {
		t.Fatalf("seed did not import file providers: %+v", pl)
	}
	if len(ml) != 1 || len(ml["file-model"]) != 1 {
		t.Fatalf("seed did not import file models: %+v", ml)
	}
	if pl[0].APIKeyRefEnv != "FILE_KEY" {
		t.Fatalf("seed lost the ref: %+v", pl[0])
	}

	// A second SeedIfEmpty with a DIFFERENT file config is a no-op (already seeded).
	other := fileCfg()
	other.Providers = map[string]config.ProviderConfig{"new": {Type: "anthropic"}}
	if err := SeedIfEmpty(ctx, s, other); err != nil {
		t.Fatal(err)
	}
	pl2, _ := s.ListProviders(ctx)
	if len(pl2) != 1 || pl2[0].Name != "file-prov" {
		t.Fatalf("second SeedIfEmpty must be a no-op, got %+v", pl2)
	}
}

// TestSeedRejectsMalformedRef pins the P4 CRITICAL: the file→DB seed path
// validates ref SHAPE before persisting, so a secret-shaped file ref never
// reaches the DB (and the store is not marked seeded).
func TestSeedRejectsMalformedRef(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	bad := &config.Config{
		Providers: map[string]config.ProviderConfig{
			// A pasted secret in the env ref field — not a valid env var name.
			"p": {Type: "anthropic", APIKeyRef: &config.SecretRef{Env: "sk-ant-PASTED-SECRET"}},
		},
	}
	if err := SeedIfEmpty(ctx, s, bad); err == nil {
		t.Fatal("seed must reject a secret-shaped ref")
	}
	if list, _ := s.ListProviders(ctx); len(list) != 0 {
		t.Fatalf("nothing must be persisted on a rejected seed, got %d providers", len(list))
	}
	if ok, _ := s.Seeded(ctx); ok {
		t.Fatal("store must NOT be marked seeded after a rejected seed")
	}
}
