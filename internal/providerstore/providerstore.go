// Package providerstore is the optional DB-authoritative store for the gateway's
// reloadable topology — providers and model routes (ADR-008, Stage 2 of ADR-005).
//
// It exists so an operator can register/repoint providers and edit routes from
// the admin plane without a restart, reusing the ADR-006 hot-reload mechanism.
// The store is OPT-IN: when no provider_store is configured, the gateway keeps
// its file-authoritative behavior and UI writes return 405 (ADR-005).
//
// SECRET-SAFETY INVARIANT (structural): a provider row holds only its secret
// REFERENCE — the env var name or file path it authenticates with — and never a
// secret value. There is no column, and no struct field, capable of holding a
// key. This mirrors configapi.ProviderView: the absence of the field is the
// defense. The schema uses only portable TEXT/INTEGER types so the same DDL maps
// onto Postgres for the v0.2 HA path (the keystore pattern).
package providerstore

import (
	"context"
	"errors"
)

// ErrNotFound is returned by Get/Delete when the named provider or model route
// does not exist.
var ErrNotFound = errors.New("providerstore: not found")

// ProviderRow is a provider registration. It carries the auth REFERENCE only —
// APIKeyRefEnv (env var name) or APIKeyRefFile (file path), plus the bedrock IAM
// AuthMode/AuthProfile — and has NO field that can hold a secret value.
type ProviderRow struct {
	Name          string
	Type          string
	BaseURL       string
	Region        string
	AuthMode      string
	AuthProfile   string
	APIKeyRefEnv  string
	APIKeyRefFile string
}

// Target is one entry in a model's ordered fallback chain. It mirrors
// config.Target but is defined locally so the core store (sqlite.go / models.go)
// needs no config import; only overlay.go (the file↔DB translation) imports
// config, and it converts between providerstore.Target and config.Target.
type Target struct {
	Provider string
	Model    string
	API      string
}

// Store is the persistence interface for the topology. The SQLite implementation
// ships; Postgres is the HA path (the DDL is portable).
type Store interface {
	UpsertProvider(ctx context.Context, p ProviderRow) error
	GetProvider(ctx context.Context, name string) (ProviderRow, error)
	ListProviders(ctx context.Context) ([]ProviderRow, error)
	DeleteProvider(ctx context.Context, name string) error

	// SetModel replaces a model's ordered target chain (replace-all, in a txn).
	SetModel(ctx context.Context, name string, targets []Target) error
	// ListModels returns every model name → its ordered targets.
	ListModels(ctx context.Context) (map[string][]Target, error)
	// DeleteModel removes a model route (ErrNotFound if absent).
	DeleteModel(ctx context.Context, name string) error

	// Seeded reports whether the one-time file→DB seed has run (durable marker).
	Seeded(ctx context.Context) (bool, error)
	// Seed imports the file topology (providers + models) AND marks the store
	// seeded, in ONE transaction — but only if not already seeded. It returns
	// true if it seeded, false if the store was already seeded (no-op). The
	// marker, not a row count, gates this, so deleting every provider never
	// resurrects the file topology (ADR-008 round-2 CRITICAL).
	Seed(ctx context.Context, providers []ProviderRow, models map[string][]Target) (bool, error)

	Close() error
}
