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

// Store is the persistence interface for the topology. The SQLite implementation
// ships; Postgres is the HA path (the DDL is portable).
type Store interface {
	UpsertProvider(ctx context.Context, p ProviderRow) error
	GetProvider(ctx context.Context, name string) (ProviderRow, error)
	ListProviders(ctx context.Context) ([]ProviderRow, error)
	DeleteProvider(ctx context.Context, name string) error
	Close() error
}
