// Package pricing computes per-request cost in integer micro-USD (µUSD) from a
// (provider, model)-keyed rate table. Money is NEVER float (design §5.3) — all
// rates are µUSD-per-million-tokens (int64) and the per-request cost is a
// single round-half-even division. cache write is TTL-tiered (5m vs 1h) and
// cache read is billed separately.
package pricing

import "math/big"

type Key struct {
	Provider string
	Model    string
}

// Rate holds µUSD per 1,000,000 tokens for each token class.
type Rate struct {
	InputPerMTok        int64
	OutputPerMTok       int64
	CacheReadPerMTok    int64
	CacheWrite5mPerMTok int64
	CacheWrite1hPerMTok int64
}

type Usage struct {
	Input        int64
	Output       int64
	CacheRead    int64
	CacheWrite5m int64
	CacheWrite1h int64
}

type OnMissing int

const (
	OnMissingAllow OnMissing = iota // cost 0 + missing=true (default; self-hosted chargeback unknown)
	OnMissingBlock
)

type Table struct {
	onMissing OnMissing
	rates     map[Key]Rate
	Version   string
}

func New(onMissing OnMissing, rates map[Key]Rate) *Table {
	if rates == nil {
		rates = map[Key]Rate{}
	}
	return &Table{onMissing: onMissing, rates: rates, Version: "bundled"}
}

func (t *Table) OnMissing() OnMissing { return t.onMissing }

// CostUSDMicros returns the request cost in µUSD and whether the (provider,
// model) rate was missing. Cost is computed once over the full token totals
// (never per-chunk) with round-half-even on the /1e6 division.
func (t *Table) CostUSDMicros(provider, model string, u Usage) (cost int64, missing bool) {
	r, ok := t.rates[Key{provider, model}]
	if !ok {
		return 0, true
	}
	total := int64(0)
	total += mulDivRoundHalfEven(u.Input, r.InputPerMTok)
	total += mulDivRoundHalfEven(u.Output, r.OutputPerMTok)
	total += mulDivRoundHalfEven(u.CacheRead, r.CacheReadPerMTok)
	total += mulDivRoundHalfEven(u.CacheWrite5m, r.CacheWrite5mPerMTok)
	total += mulDivRoundHalfEven(u.CacheWrite1h, r.CacheWrite1hPerMTok)
	return total, false
}

// mulDivRoundHalfEven computes tokens * perMTok / 1_000_000 with banker's
// rounding, using math/big to avoid int64 overflow on large token counts.
func mulDivRoundHalfEven(tokens, perMTok int64) int64 {
	if tokens == 0 || perMTok == 0 {
		return 0
	}
	num := new(big.Int).Mul(big.NewInt(tokens), big.NewInt(perMTok))
	denom := big.NewInt(1_000_000)
	q := new(big.Int)
	rem := new(big.Int)
	q.QuoRem(num, denom, rem)
	// round half to even
	twice := new(big.Int).Mul(rem, big.NewInt(2))
	cmp := twice.CmpAbs(denom)
	if cmp > 0 || (cmp == 0 && q.Bit(0) == 1) {
		q.Add(q, big.NewInt(1))
	}
	return q.Int64()
}
