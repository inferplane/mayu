package keystore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"

	"github.com/jackc/pgx/v5"
)

// Seed records the ORIGINAL declaration fingerprint in the same transaction as
// its first insert/attachment. Identical restarts never reconcile over admin
// edits or recreate deleted identities. A changed declaration fails closed,
// even if it happens to match a subsequent admin edit. The first attachment to
// a preexisting row requires matching semantic fields (excluding time/ID and
// revocation, which bootstrap never owns).
func (s *PostgresStore) Seed(ctx context.Context, teams []TeamRecord, keys []SeedKey) error {
	ctx, cancel := context.WithTimeout(ctx, postgresTimeout)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return postgresError("begin seed", err)
	}
	defer rollbackPostgres(tx)
	if err := lockPostgresIdentity(ctx, tx); err != nil {
		return err
	}
	now, err := postgresNow(ctx, tx)
	if err != nil {
		return err
	}
	for _, team := range teams {
		record := newPostgresTeam(team, now)
		fingerprint, err := teamSeedFingerprint(record)
		if err != nil {
			return err
		}
		known, err := knownSeedFingerprint(ctx, tx, "team", team.Name, fingerprint)
		if err != nil {
			return err
		}
		if known {
			continue
		}
		if _, err := insertPostgresTeam(ctx, tx, record, true); err != nil {
			return err
		}
		if err := saveSeedFingerprint(ctx, tx, "team", team.Name, fingerprint); err != nil {
			return err
		}
	}
	for _, key := range keys {
		hash := hashKey(key.Plaintext)
		k, err := newPostgresKey(hash, "ik_"+hash[:12], key.Team, key.AllowedModels, key.Options, now)
		if err != nil {
			return err
		}
		fingerprint, err := keySeedFingerprint(k)
		if err != nil {
			return err
		}
		known, err := knownSeedFingerprint(ctx, tx, "key", hash, fingerprint)
		if err != nil {
			return err
		}
		if known {
			continue
		}
		if _, err := insertPostgresKey(ctx, tx, k, true); err != nil {
			return err
		}
		if err := saveSeedFingerprint(ctx, tx, "key", hash, fingerprint); err != nil {
			return err
		}
	}
	return postgresError("commit seed", tx.Commit(ctx))
}

func knownSeedFingerprint(ctx context.Context, tx pgx.Tx, kind, identity, fingerprint string) (bool, error) {
	var stored string
	err := tx.QueryRow(ctx, `SELECT fingerprint FROM keystore_seed_fingerprints WHERE kind=$1 AND identity=$2`,
		kind, identity).Scan(&stored)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, postgresError("read original seed fingerprint", err)
	}
	if stored != fingerprint {
		return false, ErrSnapshotChanged
	}
	return true, nil
}

func saveSeedFingerprint(ctx context.Context, tx pgx.Tx, kind, identity, fingerprint string) error {
	_, err := tx.Exec(ctx, `INSERT INTO keystore_seed_fingerprints(kind,identity,fingerprint) VALUES($1,$2,$3)`,
		kind, identity, fingerprint)
	return postgresError("save original seed fingerprint", err)
}

func seedDigest(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", postgresError("encode seed fingerprint", err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func canonicalSeedList(encoded string) string {
	list := splitModels(encoded)
	slices.Sort(list)
	return joinModels(slices.Compact(list))
}

func keySeedFingerprint(k postgresKey) (string, error) {
	k.KeyID, k.CreatedAt, k.Revoked = "", "", 0
	k.Models = canonicalSeedList(k.Models)
	expiry, err := decodeExpiry(k.ExpiresAt)
	if err != nil {
		return "", postgresError("decode seed expiry", err)
	}
	k.ExpiresAt = encodeExpiry(expiry)
	var metadata map[string]string
	if k.Metadata != "" {
		if err := json.Unmarshal([]byte(k.Metadata), &metadata); err != nil {
			return "", postgresError("decode seed metadata", err)
		}
	}
	k.Metadata, err = encodeMetadata(metadata)
	if err != nil {
		return "", postgresError("encode seed metadata", err)
	}
	return seedDigest(k)
}

func teamSeedFingerprint(team postgresTeam) (string, error) {
	team.CreatedAt, team.UpdatedAt = "", ""
	team.Models, team.Regions = canonicalSeedList(team.Models), canonicalSeedList(team.Regions)
	return seedDigest(team)
}

// The caller holds both identity tables locked. Import compares exact rows;
// seed compares declared fields, retaining the stored ID/time/revocation.
func insertPostgresKey(ctx context.Context, tx pgx.Tx, k postgresKey, seed bool) (bool, error) {
	tag, err := tx.Exec(ctx, postgresInsertKey+` ON CONFLICT DO NOTHING`, k.values()...)
	if err != nil {
		return false, postgresError("insert immutable key", err)
	}
	if tag.RowsAffected() != 0 {
		return true, nil
	}
	var encoded []byte
	if err := tx.QueryRow(ctx, `SELECT to_jsonb(k) FROM keys k WHERE key_hash=$1`, k.Hash).Scan(&encoded); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, ErrSnapshotChanged // a different hash occupies key_id
		}
		return false, postgresError("read key declaration", err)
	}
	var existing postgresKey
	if err := json.Unmarshal(encoded, &existing); err != nil {
		return false, postgresError("decode key declaration", err)
	}
	if seed {
		oldFingerprint, err := keySeedFingerprint(existing)
		if err != nil {
			return false, err
		}
		newFingerprint, err := keySeedFingerprint(k)
		if err != nil {
			return false, err
		}
		if oldFingerprint != newFingerprint {
			return false, ErrSnapshotChanged
		}
		return false, nil
	}
	if existing != k {
		return false, ErrSnapshotChanged
	}
	return false, nil
}

func insertPostgresTeam(ctx context.Context, tx pgx.Tx, team postgresTeam, seed bool) (bool, error) {
	tag, err := tx.Exec(ctx, postgresInsertTeam+` ON CONFLICT DO NOTHING`, team.values()...)
	if err != nil {
		return false, postgresError("insert immutable team", err)
	}
	if tag.RowsAffected() != 0 {
		return true, nil
	}
	var encoded []byte
	if err := tx.QueryRow(ctx, `SELECT to_jsonb(t) FROM teams t WHERE name=$1`, team.Name).Scan(&encoded); err != nil {
		return false, postgresError("read team declaration", err)
	}
	var existing postgresTeam
	if err := json.Unmarshal(encoded, &existing); err != nil {
		return false, postgresError("decode team declaration", err)
	}
	if seed {
		oldFingerprint, err := teamSeedFingerprint(existing)
		if err != nil {
			return false, err
		}
		newFingerprint, err := teamSeedFingerprint(team)
		if err != nil {
			return false, err
		}
		if oldFingerprint != newFingerprint {
			return false, ErrSnapshotChanged
		}
		return false, nil
	}
	if existing != team {
		return false, ErrSnapshotChanged
	}
	return false, nil
}
