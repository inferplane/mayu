package providerstore

import (
	"context"
	"fmt"
	"log"

	"github.com/inferplane/inferplane/internal/config"
)

// Overlay returns the EFFECTIVE config: a copy of the raw file config with its
// Providers and Models REPLACED by the DB topology (ADR-008 — the DB is
// authoritative for the reloadable topology; server/teams/audit/pricing stay
// file-sourced). It maps secret REFS only and never resolves them — resolution
// is the caller's job (config.ResolveProviders on the returned config), so
// Overlay touches no secret material and the leaf store never imports the
// resolver's failure modes.
//
// A file that still declares providers while the DB is authoritative is logged
// as a divergence (not silently honored) so operators are never confused about
// what is authoritative.
func Overlay(rawFileCfg *config.Config, store Store) (*config.Config, error) {
	ctx := context.Background()

	if len(rawFileCfg.Providers) > 0 {
		log.Printf("providerstore: DB is authoritative; ignoring %d provider(s) declared in the config file (use GET /admin/config/export to reconcile)", len(rawFileCfg.Providers))
	}

	provs, err := store.ListProviders(ctx)
	if err != nil {
		return nil, err
	}
	models, err := store.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	return OverlayFrom(rawFileCfg, provs, models), nil
}

// OverlayFrom is the pure topology overlay: a copy of rawFileCfg with its
// Providers/Models replaced by the given DB rows/routes (refs only, unresolved).
// It is shared by Overlay (boot/reload reads the store) and the write path
// (which builds a candidate from the current store PLUS the pending mutation, so
// the validated generation is the one published — build-once-swap-once).
func OverlayFrom(rawFileCfg *config.Config, provs []ProviderRow, models map[string][]Target) *config.Config {
	eff := *rawFileCfg // shallow copy; Providers/Models are replaced with fresh maps below
	eff.Providers = make(map[string]config.ProviderConfig, len(provs))
	for _, p := range provs {
		eff.Providers[p.Name] = providerConfigFromRow(p)
	}
	eff.Models = make(map[string]config.ModelConfig, len(models))
	for name, targets := range models {
		eff.Models[name] = config.ModelConfig{Targets: targetsToConfig(targets)}
	}
	return &eff
}

// SeedIfEmpty performs the one-time file→DB import (ADR-008): if the store has
// never been seeded (durable marker), it imports the raw file config's providers
// and models in a single transaction and marks the store seeded. A store that
// was seeded and later emptied is NOT re-seeded — the marker, not a row count,
// gates this.
func SeedIfEmpty(ctx context.Context, store Store, rawFileCfg *config.Config) error {
	provs := make([]ProviderRow, 0, len(rawFileCfg.Providers))
	for name, pc := range rawFileCfg.Providers {
		// Validate the ref SHAPE through the SAME shared guard the UI write path
		// uses, BEFORE any DB insert (P4 CRITICAL): a malformed/secret-shaped file
		// ref must never be persisted/exported/audited via the seed path.
		if err := config.ValidateSecretRef(pc.APIKeyRef); err != nil {
			return fmt.Errorf("providerstore: seed provider %q: %w", name, err)
		}
		provs = append(provs, rowFromProviderConfig(name, pc))
	}
	models := make(map[string][]Target, len(rawFileCfg.Models))
	for name, mc := range rawFileCfg.Models {
		models[name] = targetsFromConfig(mc.Targets)
	}
	did, err := store.Seed(ctx, provs, models)
	if err != nil {
		return err
	}
	if did {
		log.Printf("providerstore: seeded DB from file config (%d provider(s), %d model(s)); DB is now authoritative", len(provs), len(models))
	}
	return nil
}

// providerConfigFromRow maps a DB row to a config.ProviderConfig carrying the
// ref only (APIKey stays empty — caller resolves).
func providerConfigFromRow(p ProviderRow) config.ProviderConfig {
	pc := config.ProviderConfig{Type: p.Type, BaseURL: p.BaseURL, Region: p.Region, AuthHeader: p.AuthHeader, GuardrailID: p.GuardrailID, GuardrailVersion: p.GuardrailVersion}
	pc.Auth.Mode = p.AuthMode
	pc.Auth.Profile = p.AuthProfile
	switch {
	case p.APIKeyRefEnv != "":
		pc.APIKeyRef = &config.SecretRef{Env: p.APIKeyRefEnv}
	case p.APIKeyRefFile != "":
		pc.APIKeyRef = &config.SecretRef{File: p.APIKeyRefFile}
	}
	return pc
}

// rowFromProviderConfig maps a file ProviderConfig to a DB row, persisting the
// REF only — never the resolved APIKey (which is dropped here by construction).
func rowFromProviderConfig(name string, pc config.ProviderConfig) ProviderRow {
	r := ProviderRow{
		Name: name, Type: pc.Type, BaseURL: pc.BaseURL, Region: pc.Region,
		AuthMode: pc.Auth.Mode, AuthProfile: pc.Auth.Profile, AuthHeader: pc.AuthHeader, GuardrailID: pc.GuardrailID, GuardrailVersion: pc.GuardrailVersion,
	}
	if pc.APIKeyRef != nil {
		r.APIKeyRefEnv = pc.APIKeyRef.Env
		r.APIKeyRefFile = pc.APIKeyRef.File
	}
	return r
}

func targetsToConfig(ts []Target) []config.Target {
	out := make([]config.Target, len(ts))
	for i, t := range ts {
		out[i] = config.Target{Provider: t.Provider, Model: t.Model, API: t.API}
	}
	return out
}

func targetsFromConfig(ts []config.Target) []Target {
	out := make([]Target, len(ts))
	for i, t := range ts {
		out[i] = Target{Provider: t.Provider, Model: t.Model, API: t.API}
	}
	return out
}
