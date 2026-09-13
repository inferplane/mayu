package pgstore

import (
	"context"
	"errors"
	"math/big"
	"time"

	"github.com/jackc/pgx/v5"
)

// Refill retires oldest debt first. Keeping only still-live segments makes an
// old cancellation incapable of crediting a newer attempt, while two overlapping
// cancellations can return all unspent debt exactly. Counter row locks serialize
// every segment writer; no per-process clock or counter participates.
func refillSharedRate(ctx context.Context, tx pgx.Tx, id accountKey, c *sharedCounter, now time.Time, rate int64) error {
	before := new(big.Int).Set(&c.debt)
	if err := c.refill(now, rate); err != nil {
		return err
	}
	retired := new(big.Int).Sub(before, &c.debt)
	if retired.Sign() == 0 {
		return nil
	}
	var amount string
	err := tx.QueryRow(ctx, `WITH ordered AS (
		SELECT segment_id,remaining,
		  COALESCE(SUM(remaining) OVER (ORDER BY segment_id
		    ROWS BETWEEN UNBOUNDED PRECEDING AND 1 PRECEDING),0) AS prior
		FROM shared_rate_segments WHERE counter_key=$1 AND window_id=$2
	), changed AS (
		UPDATE shared_rate_segments s
		SET remaining=GREATEST(0,o.remaining-GREATEST(0,$3::numeric-o.prior))
		FROM ordered o WHERE s.segment_id=o.segment_id AND o.prior<$3::numeric
		RETURNING o.remaining-s.remaining AS retired
	) SELECT COALESCE(SUM(retired),0)::text FROM changed`, id.key, id.window, retired.String()).Scan(&amount)
	if err != nil {
		return err
	}
	n, ok := new(big.Int).SetString(amount, 10)
	if !ok || n.Cmp(retired) != 0 {
		return errors.New("inconsistent shared rate segments")
	}
	_, err = tx.Exec(ctx, `DELETE FROM shared_rate_segments WHERE counter_key=$1 AND window_id=$2 AND remaining=0`, id.key, id.window)
	return err
}

func returnSharedRate(ctx context.Context, tx pgx.Tx, hash string, b sharedBooking, c *sharedCounter, refund int64, known bool) error {
	var value string
	err := tx.QueryRow(ctx, `SELECT remaining::text FROM shared_rate_segments
		WHERE permit_hash=$1 AND counter_key=$2 AND window_id=$3`, hash, b.id.key, b.id.window).Scan(&value)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // all of this booking's debt has already refilled
	}
	if err != nil {
		return err
	}
	remaining, ok := new(big.Int).SetString(value, 10)
	if !ok || remaining.Sign() < 0 {
		return errors.New("invalid shared rate segment")
	}
	release := sharedScaled(refund)
	if release.Cmp(remaining) > 0 {
		release.Set(remaining)
	}
	if release.Cmp(&c.debt) > 0 {
		return errors.New("inconsistent shared rate refund")
	}
	c.debt.Sub(&c.debt, release)
	remaining.Sub(remaining, release)
	if remaining.Sign() == 0 {
		_, err = tx.Exec(ctx, `DELETE FROM shared_rate_segments WHERE permit_hash=$1 AND counter_key=$2 AND window_id=$3`,
			hash, b.id.key, b.id.window)
	} else {
		_, err = tx.Exec(ctx, `UPDATE shared_rate_segments SET remaining=$4::numeric,uncertain=$5
			WHERE permit_hash=$1 AND counter_key=$2 AND window_id=$3`, hash, b.id.key, b.id.window, remaining.String(), !known)
	}
	return err
}

func sharedRateReserved(ctx context.Context, tx pgx.Tx, id accountKey) (int64, error) {
	var value string
	err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(remaining),0)::text FROM shared_rate_segments
		WHERE counter_key=$1 AND window_id=$2 AND uncertain`, id.key, id.window).Scan(&value)
	if err != nil {
		return 0, err
	}
	remaining, ok := new(big.Int).SetString(value, 10)
	if !ok {
		return 0, errors.New("invalid shared rate reservation")
	}
	return sharedCeil(remaining)
}
