// Package keystore stores virtual API keys (SHA-256 hashed) and the team /
// model-allow-list metadata behind them. Store is the swappable backend
// interface; M3 ships SQLite, Postgres is the HA path (v0.2). Only the key
// HASH is persisted — the plaintext is shown once at Create and never stored.
package keystore

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"strings"
	"time"
)

// KeyOptions are the optional per-key governance fields (spec §8 D2). Zero
// value of each field means "unlimited"/"never" — a key created via the plain
// Create() has none of these set. Budget/TPM/RPM are enforced in the request
// hot path (internal/governance.Governor, layered on top of team policy).
type KeyOptions struct {
	BudgetUSDMicros int64             // 0 = unlimited; integer microUSD, never float
	TPM             int64             // 0 = unlimited
	RPM             int64             // 0 = unlimited
	ExpiresAt       *time.Time        // nil = never; enforced in Resolve
	Owner           string            // opaque identifier, optional — never use as a metric label (unbounded cardinality; CLAUDE.md forbids raw-input metric labels)
	Metadata        map[string]string // optional key/value tags — same caution: never use as a metric label
}

// Principal is the resolved identity behind a virtual key (M3: service-account
// + team; user/OIDC is M5). It rides in the request context after KeyAuth.
type Principal struct {
	KeyID         string // "ik_" + 12-char prefix of the key id; logged, never the secret
	Team          string
	AllowedModels []string // "*" allows all; else explicit allow-list (§5.1 policy)
	KeyOptions
}

// Allows reports whether this principal may use the given model.
func (p Principal) Allows(model string) bool {
	for _, m := range p.AllowedModels {
		if m == "*" || m == model {
			return true
		}
	}
	return false
}

type Store interface {
	Create(ctx context.Context, team string, allowedModels []string) (plaintext string, p Principal, err error)
	// CreateWithOptions is Create plus the optional governance fields (§8 D2).
	CreateWithOptions(ctx context.Context, team string, allowedModels []string, opts KeyOptions) (plaintext string, p Principal, err error)
	Resolve(ctx context.Context, plaintext string) (Principal, error)
	Revoke(ctx context.Context, keyID string) error
	List(ctx context.Context) ([]Principal, error)
	Close() error
}

// TeamRecord is a team's governance policy + defaults, stored as a first-class
// keystore row (D3, ADR-016). Zero value of a numeric field means "unlimited",
// same convention as KeyOptions. Budget is integer microUSD, never float.
type TeamRecord struct {
	Name             string
	AllowedModels    []string // default allow-list for keys created under this team; not itself hot-path enforced (ADR-016)
	RPM              int64
	TPM              int64
	TokensPerDay     int64
	QuotaOnExceeded  string // "" | "block" | "warn"
	BudgetUSDMicros  int64
	BudgetOnExceeded string // "" | "block" | "warn"
	// GuardrailID/GuardrailVersion override the provider's default Bedrock
	// Guardrail for this team (D6, ADR-019) — a different guardrail, never
	// "none" (no per-team opt-out; the anti-bypass fix cannot be disabled by
	// the team it protects). Empty ID = no override, provider default applies.
	GuardrailID      string
	GuardrailVersion string
	// AllowedRegions restricts this team's traffic to providers labeled with
	// one of these regions (D7, ADR-020). Empty = unrestricted (current
	// behavior). Fail-closed: an unlabeled provider is treated as NOT in any
	// allowed region, so a restricted team can never reach it.
	AllowedRegions []string
	CreatedAt      string
	UpdatedAt      string
}

// TeamStore is a separate interface (not folded into Store) so the existing
// fake Store implementations in internal/server's tests keep compiling
// unchanged — only backends that want to support D3 team records need it.
type TeamStore interface {
	UpsertTeam(ctx context.Context, t TeamRecord) error
	GetTeam(ctx context.Context, name string) (TeamRecord, bool, error)
	ListTeams(ctx context.Context) ([]TeamRecord, error)
	DeleteTeam(ctx context.Context, name string) error
}

// KeyEnsurer upserts a virtual key from a caller-supplied plaintext (ADR-023
// declarative bootstrap), as opposed to Create/CreateWithOptions which generate
// a new random plaintext. Revocation and created_at are never touched.
type KeyEnsurer interface {
	EnsureKey(ctx context.Context, plaintext, team string, allowedModels []string, opts KeyOptions) (Principal, error)
}

var b32 = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// generateKey returns a high-entropy virtual key ("ik_" + 32 random bytes in
// lowercase base32) and its SHA-256 hash (hex). The key id is a 12-char prefix
// of the hash — safe to log, not reversible to the secret.
func generateKey() (plaintext, hashHex, keyID string, err error) {
	var raw [32]byte
	if _, err = rand.Read(raw[:]); err != nil {
		return "", "", "", err
	}
	plaintext = "ik_" + b32.EncodeToString(raw[:])
	sum := sha256.Sum256([]byte(plaintext))
	hashHex = hex.EncodeToString(sum[:])
	keyID = "ik_" + hashHex[:12]
	return plaintext, hashHex, keyID, nil
}

// hashKey returns the SHA-256 hex of a presented plaintext key for lookup.
func hashKey(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

func joinModels(models []string) string { return strings.Join(models, ",") }
func splitModels(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339Nano) }
