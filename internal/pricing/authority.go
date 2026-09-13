package pricing

import (
	"errors"
	"math/big"
)

var ErrAuthorityBound = errors.New("pricing: finite authority bound unavailable")

// AuthorityBound uses the declared enforced model window, not a cheap token
// estimate. Each input/cache category is conservatively bounded separately,
// including overlapping cache accounting; output is bounded independently.
// Upward rounding dominates the settlement's round-half-even cost. A provider
// violating its declared limits is an accounting breach, not a safe refund.
func (t *Table) AuthorityBound(provider, model string, inputLimit, outputLimit int64) (int64, error) {
	if t == nil || inputLimit <= 0 || outputLimit <= 0 {
		return 0, ErrAuthorityBound
	}
	r, ok := t.rates[Key{provider, model}]
	if !ok {
		if base := normalizeModel(model); base != "" {
			r, ok = t.rates[Key{provider, base}]
		}
	}
	if !ok {
		return 0, ErrAuthorityBound
	}
	total := new(big.Int)
	denominator := big.NewInt(1000000)
	for _, item := range []struct{ count, rate int64 }{
		{inputLimit, r.InputPerMTok}, {inputLimit, r.CacheReadPerMTok},
		{inputLimit, r.CacheWrite5mPerMTok}, {inputLimit, r.CacheWrite1hPerMTok},
		{outputLimit, r.OutputPerMTok},
	} {
		if item.rate < 0 {
			return 0, ErrAuthorityBound
		}
		n := new(big.Int).Mul(big.NewInt(item.count), big.NewInt(item.rate))
		q, rem := new(big.Int), new(big.Int)
		q.QuoRem(n, denominator, rem)
		if rem.Sign() > 0 {
			q.Add(q, big.NewInt(1))
		}
		total.Add(total, q)
	}
	if !total.IsInt64() {
		return 0, ErrAuthorityBound
	}
	return total.Int64(), nil
}
