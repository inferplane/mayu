package keystore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Raw rows keep SQLite's encodings and all 64 bits of every limit. They are
// private so neither hashes nor the row data used in revisions become API
// fields. Import compares these exact persisted records, including tombstones.
type postgresKey struct {
	KeyID                 string `json:"key_id"`
	Hash                  string `json:"key_hash"`
	Team                  string `json:"team"`
	Models                string `json:"allowed_models"`
	CreatedAt             string `json:"created_at"`
	Revoked               int    `json:"revoked"`
	BudgetUSDMicros       int64  `json:"budget_usd_micros"`
	TPM                   int64  `json:"tpm"`
	RPM                   int64  `json:"rpm"`
	ExpiresAt             string `json:"expires_at"`
	Owner                 string `json:"owner"`
	Metadata              string `json:"metadata"`
	BudgetUSDMicrosPerDay int64  `json:"budget_usd_micros_per_day"`
}

func (k postgresKey) values() []any {
	return []any{k.KeyID, k.Hash, k.Team, k.Models, k.CreatedAt, k.Revoked,
		k.BudgetUSDMicros, k.TPM, k.RPM, k.ExpiresAt, k.Owner, k.Metadata, k.BudgetUSDMicrosPerDay}
}

func (k *postgresKey) destinations() []any {
	return []any{&k.KeyID, &k.Hash, &k.Team, &k.Models, &k.CreatedAt, &k.Revoked,
		&k.BudgetUSDMicros, &k.TPM, &k.RPM, &k.ExpiresAt, &k.Owner, &k.Metadata, &k.BudgetUSDMicrosPerDay}
}

func newPostgresKey(hash, id, team string, models []string, opts KeyOptions, now time.Time) (postgresKey, error) {
	metadata, err := encodeMetadata(opts.Metadata)
	if err != nil {
		return postgresKey{}, postgresError("encode metadata", err)
	}
	return postgresKey{
		KeyID: id, Hash: hash, Team: team, Models: joinModels(models),
		CreatedAt: now.UTC().Format(time.RFC3339Nano), BudgetUSDMicros: opts.BudgetUSDMicros,
		TPM: opts.TPM, RPM: opts.RPM, ExpiresAt: encodeExpiry(opts.ExpiresAt),
		Owner: opts.Owner, Metadata: metadata, BudgetUSDMicrosPerDay: opts.BudgetUSDMicrosPerDay,
	}, nil
}

type postgresTeam struct {
	Name                  string `json:"name"`
	Models                string `json:"allowed_models"`
	RPM                   int64  `json:"rpm"`
	TPM                   int64  `json:"tpm"`
	TokensPerDay          int64  `json:"tokens_per_day"`
	QuotaOnExceeded       string `json:"quota_on_exceeded"`
	BudgetUSDMicros       int64  `json:"budget_usd_micros"`
	BudgetOnExceeded      string `json:"budget_on_exceeded"`
	BudgetUSDMicrosPerDay int64  `json:"budget_usd_micros_per_day"`
	GuardrailID           string `json:"guardrail_id"`
	GuardrailVersion      string `json:"guardrail_version"`
	Regions               string `json:"allowed_regions"`
	CreatedAt             string `json:"created_at"`
	UpdatedAt             string `json:"updated_at"`
}

func (t postgresTeam) values() []any {
	return []any{t.Name, t.Models, t.RPM, t.TPM, t.TokensPerDay, t.QuotaOnExceeded,
		t.BudgetUSDMicros, t.BudgetOnExceeded, t.BudgetUSDMicrosPerDay, t.GuardrailID,
		t.GuardrailVersion, t.Regions, t.CreatedAt, t.UpdatedAt}
}

func (t *postgresTeam) destinations() []any {
	return []any{&t.Name, &t.Models, &t.RPM, &t.TPM, &t.TokensPerDay, &t.QuotaOnExceeded,
		&t.BudgetUSDMicros, &t.BudgetOnExceeded, &t.BudgetUSDMicrosPerDay, &t.GuardrailID,
		&t.GuardrailVersion, &t.Regions, &t.CreatedAt, &t.UpdatedAt}
}

func newPostgresTeam(t TeamRecord, now time.Time) postgresTeam {
	stamp := now.UTC().Format(time.RFC3339Nano)
	return postgresTeam{
		Name: t.Name, Models: joinModels(t.AllowedModels), RPM: t.RPM, TPM: t.TPM,
		TokensPerDay: t.TokensPerDay, QuotaOnExceeded: t.QuotaOnExceeded,
		BudgetUSDMicros: t.BudgetUSDMicros, BudgetOnExceeded: t.BudgetOnExceeded,
		BudgetUSDMicrosPerDay: t.BudgetUSDMicrosPerDay, GuardrailID: t.GuardrailID,
		GuardrailVersion: t.GuardrailVersion, Regions: joinModels(t.AllowedRegions),
		CreatedAt: stamp, UpdatedAt: stamp,
	}
}

func (t postgresTeam) record() TeamRecord {
	return TeamRecord{
		Name: t.Name, AllowedModels: splitModels(t.Models), RPM: t.RPM, TPM: t.TPM,
		TokensPerDay: t.TokensPerDay, QuotaOnExceeded: t.QuotaOnExceeded,
		BudgetUSDMicros: t.BudgetUSDMicros, BudgetOnExceeded: t.BudgetOnExceeded,
		BudgetUSDMicrosPerDay: t.BudgetUSDMicrosPerDay, GuardrailID: t.GuardrailID,
		GuardrailVersion: t.GuardrailVersion, AllowedRegions: splitModels(t.Regions),
		CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
	}
}

// A single joined statement captures both rows, including absence of a team.
// Database time is returned separately and is never part of the digest.
const postgresSnapshotQuery = `SELECT to_jsonb(k), to_jsonb(t), clock_timestamp()
FROM keys k LEFT JOIN teams t ON t.name=k.team `

func scanPostgresSnapshot(row interface{ Scan(...any) error }) (Principal, time.Time, error) {
	var keyJSON, teamJSON []byte
	var now time.Time
	if err := row.Scan(&keyJSON, &teamJSON, &now); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Principal{}, time.Time{}, ErrKeyNotFound
		}
		return Principal{}, time.Time{}, postgresError("read snapshot", err)
	}
	var k postgresKey
	var team *postgresTeam
	if err := json.Unmarshal(keyJSON, &k); err != nil {
		return Principal{}, time.Time{}, postgresError("decode key", err)
	}
	if len(teamJSON) != 0 {
		if err := json.Unmarshal(teamJSON, &team); err != nil {
			return Principal{}, time.Time{}, postgresError("decode team", err)
		}
	}
	expiry, err := decodeExpiry(k.ExpiresAt)
	if err != nil {
		return Principal{}, time.Time{}, postgresError("decode expiry", err)
	}
	p := Principal{
		KeyID: k.KeyID, Team: k.Team, AllowedModels: splitModels(k.Models),
		KeyOptions: KeyOptions{
			BudgetUSDMicros: k.BudgetUSDMicros, BudgetUSDMicrosPerDay: k.BudgetUSDMicrosPerDay,
			TPM: k.TPM, RPM: k.RPM, ExpiresAt: expiry, Owner: k.Owner, Metadata: decodeMetadata(k.Metadata),
		},
		TeamSnapshotLoaded: true,
	}
	if team != nil {
		record := team.record()
		p.TeamSnapshot = &record
	}
	encoded, err := json.Marshal(struct {
		Key  postgresKey
		Team *postgresTeam
	}{k, team})
	if err != nil {
		return Principal{}, time.Time{}, postgresError("encode snapshot", err)
	}
	digest := sha256.Sum256(encoded)
	p.SharedRevision = hex.EncodeToString(digest[:])
	return p, now, nil
}

func checkPostgresExpiry(p Principal, now time.Time) error {
	if now.IsZero() {
		return ErrStoreUnavailable
	}
	if p.ExpiresAt != nil && !now.Before(*p.ExpiresAt) {
		return ErrKeyExpired
	}
	return nil
}

// ReadPostgresPrincipal consumes, but never commits or rolls back, the caller's
// transaction. The caller must lock keys and teams before taking a fresh
// clock_timestamp() and supplying it here. All readers use the same digest.
func ReadPostgresPrincipal(ctx context.Context, tx pgx.Tx, keyID string, now time.Time) (Principal, error) {
	ctx, cancel := context.WithTimeout(ctx, postgresTimeout)
	defer cancel()
	if tx == nil || now.IsZero() {
		return Principal{}, ErrStoreUnavailable
	}
	p, _, err := scanPostgresSnapshot(tx.QueryRow(ctx,
		postgresSnapshotQuery+`WHERE k.key_id=$1 AND k.revoked=0`, keyID))
	if err != nil {
		return Principal{}, err
	}
	if err := checkPostgresExpiry(p, now); err != nil {
		return Principal{}, err
	}
	return p, nil
}
