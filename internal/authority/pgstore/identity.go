package pgstore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"time"

	"github.com/inferplane/inferplane/internal/policy"
	"github.com/jackc/pgx/v5"
)

func initializeIdentity(ctx context.Context, tx pgx.Tx) error {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return databaseError("initialize authority identity", err)
	}
	_, err := tx.Exec(ctx, `INSERT INTO authority_identity(singleton,namespace)
		VALUES(true,$1) ON CONFLICT(singleton) DO NOTHING`, hex.EncodeToString(raw[:]))
	return databaseError("initialize authority identity", err)
}

func readIdentity(ctx context.Context, tx pgx.Tx) (string, error) {
	var value string
	err := tx.QueryRow(ctx, `SELECT namespace FROM authority_identity WHERE singleton=true`).Scan(&value)
	return value, databaseError("read authority identity", err)
}

// SharedBinding proves that the shared gateway and heartbeat issuer address the
// same authority namespace and policy content, not independent equal-sized
// budgets. It is a background synchronization operation, not a grant request.
func (s *Store) SharedBinding(ctx context.Context) (string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return "", "", databaseError("begin authority binding", err)
	}
	defer rollback(tx)
	id, err := readIdentity(ctx, tx)
	if err != nil {
		return "", "", err
	}
	docs, err := readPolicies(ctx, tx)
	if err != nil {
		return "", "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", "", databaseError("read authority binding", err)
	}
	return id, policy.GenerationOf(docs), nil
}
