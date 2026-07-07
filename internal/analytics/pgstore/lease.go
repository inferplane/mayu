package pgstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const leaseID = "mode_b_aggregator"

var errFenced = errors.New("pgstore: lease fenced")

func tryAcquireLease(ctx context.Context, db *pgxpool.Pool, holder string, ttl time.Duration) (int64, bool, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return 0, false, fmt.Errorf("begin lease transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	ttlSeconds := int64(ttl.Seconds())

	var epoch int64
	var currentHolder string
	err = tx.QueryRow(ctx, `SELECT epoch, holder FROM lease WHERE id=$1 FOR UPDATE`, leaseID).Scan(&epoch, &currentHolder)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, `INSERT INTO lease(id, holder, epoch, expires_at)
			VALUES($1, $2, 1, now() + ($3 * interval '1 second'))
			ON CONFLICT (id) DO NOTHING
			RETURNING epoch`, leaseID, holder, ttlSeconds).Scan(&epoch)
		if err == nil {
			if err := tx.Commit(ctx); err != nil {
				return 0, false, fmt.Errorf("commit new lease: %w", err)
			}
			return epoch, true, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return 0, false, fmt.Errorf("insert lease: %w", err)
		}
		if err := tx.QueryRow(ctx, `SELECT epoch, holder FROM lease WHERE id=$1 FOR UPDATE`, leaseID).Scan(&epoch, &currentHolder); err != nil {
			return 0, false, fmt.Errorf("lock concurrent lease: %w", err)
		}
	} else if err != nil {
		return 0, false, fmt.Errorf("lock lease: %w", err)
	}

	err = tx.QueryRow(ctx, `UPDATE lease
		SET holder=$1,
		    epoch = CASE WHEN holder=$1 THEN epoch ELSE epoch+1 END,
		    expires_at = now() + ($2 * interval '1 second')
		WHERE id=$3 AND (expires_at < now() OR holder=$1)
		RETURNING epoch`, holder, ttlSeconds, leaseID).Scan(&epoch)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := tx.Commit(ctx); err != nil {
			return 0, false, fmt.Errorf("commit unchanged lease: %w", err)
		}
		return epoch, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("update lease: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, false, fmt.Errorf("commit lease: %w", err)
	}
	return epoch, true, nil
}
