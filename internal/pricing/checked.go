package pricing

import (
	"errors"
	"math/big"
)

var ErrInvalidCost = errors.New("pricing: negative or overflowing cost")

// CostUSDMicrosChecked preserves per-class round-half-even semantics while
// checking the result before narrowing to int64. Durable settlement must never
// mistake a wrapped cost for proof that a reservation can be refunded.
func (t *Table) CostUSDMicrosChecked(provider, model string, u Usage) (int64, error) {
	if t == nil {
		return 0, ErrAuthorityBound
	}
	r, ok := t.rates[Key{provider, model}]
	if !ok {
		r, ok = t.rates[Key{provider, normalizeModel(model)}]
	}
	if !ok {
		return 0, ErrAuthorityBound
	}
	total := new(big.Int)
	denom := big.NewInt(1_000_000)
	for _, item := range []struct{ count, rate int64 }{
		{u.Input, r.InputPerMTok}, {u.Output, r.OutputPerMTok},
		{u.CacheRead, r.CacheReadPerMTok}, {u.CacheWrite5m, r.CacheWrite5mPerMTok},
		{u.CacheWrite1h, r.CacheWrite1hPerMTok},
	} {
		if item.count < 0 || item.rate < 0 {
			return 0, ErrInvalidCost
		}
		n := new(big.Int).Mul(big.NewInt(item.count), big.NewInt(item.rate))
		q, rem := new(big.Int), new(big.Int)
		q.QuoRem(n, denom, rem)
		cmp := new(big.Int).Lsh(rem, 1).Cmp(denom)
		if cmp > 0 || (cmp == 0 && q.Bit(0) == 1) {
			q.Add(q, big.NewInt(1))
		}
		total.Add(total, q)
	}
	if !total.IsInt64() {
		return 0, ErrInvalidCost
	}
	return total.Int64(), nil
}
