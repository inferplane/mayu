package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"sort"
	"time"

	v1alpha1 "github.com/inferplane/inferplane/api/v1alpha1"
	"github.com/inferplane/inferplane/internal/governance"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/policy"
	"github.com/jackc/pgx/v5"
)

// Postgres clocks have microsecond precision. Debt is in token*60,000,000
// units, so refill = elapsedMicroseconds*RPM/TPM is exact, even near MaxInt64.
const sharedRateScale int64 = 60_000_000

type sharedDefinition struct {
	id                                          accountKey
	scope, kind, policy, rule, window, moneyKey string
	limit                                       int64
	hard                                        bool
	start, end                                  time.Time
}

func (d sharedDefinition) rate() int64 {
	if d.kind == "rpm" || d.kind == "tpm" {
		return d.limit
	}
	return 0
}

func (d sharedDefinition) bound(r governance.SharedRequest) int64 {
	switch d.kind {
	case "rpm":
		return 1
	case "microUSD":
		return r.CostBoundMicroUSD
	default:
		return r.TokenBound
	}
}

func sharedHash(v any) string {
	b, _ := json.Marshal(v)
	return capabilityHash(string(b))
}

func sharedWindow(now time.Time, period v1alpha1.BudgetPeriod) (time.Time, time.Time) {
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	if period == v1alpha1.PeriodCalendarDay {
		start = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		return start, start.AddDate(0, 0, 1)
	}
	return start, start.AddDate(0, 1, 0)
}

func sharedScope(team, user string) string {
	if team != "" && user != "" {
		return "team-user"
	}
	if user != "" {
		return "user"
	}
	return "team"
}

func sharedMatch(team, user string, p keystore.Principal) bool {
	return (team == "" || team == p.Team) && (user == "" || user == p.Owner)
}

func sharedDefinitions(ctx context.Context, tx pgx.Tx, p keystore.Principal, now time.Time, req *governance.SharedRequest) ([]sharedDefinition, []policy.AuthorityBudget, error) {
	docs, err := readPolicies(ctx, tx)
	if err != nil {
		return nil, nil, sharedError(err)
	}
	// The routing decision and synchronous accounting must see the same
	// complete policy set, including privacy rules which are enforced before
	// this call. Missing generations must not silently mean "empty policies".
	if req != nil && (req.PolicyGeneration == "" || req.PolicyGeneration != policy.GenerationOf(docs)) {
		return nil, nil, &governance.SharedDenial{Status: 503, Reason: "shared policy snapshot changed", RetryAfter: 1}
	}
	allBudgets, err := policy.AuthorityBudgets(docs, now)
	if err != nil {
		return nil, nil, sharedError(err)
	}
	var defs []sharedDefinition
	add := func(scope, identity, kind, pol, rule string, limit int64, hard bool, period v1alpha1.BudgetPeriod) {
		if limit == 0 {
			return
		}
		d := sharedDefinition{scope: scope, kind: kind, policy: pol, rule: rule, limit: limit, hard: hard, window: "minute"}
		d.id.key = sharedHash([]string{scope, identity, kind, pol, rule, string(period)})
		d.id.window = "bucket"
		if period != "" {
			d.start, d.end = sharedWindow(now, period)
			d.window = string(period)
			d.id.window = d.start.Format(time.RFC3339)
		}
		defs = append(defs, d)
	}
	t := p.TeamSnapshot
	if t == nil {
		return nil, nil, sharedChanged()
	}
	for _, n := range []int64{t.RPM, t.TPM, t.TokensPerDay, t.BudgetUSDMicros, t.BudgetUSDMicrosPerDay, p.RPM, p.TPM, p.BudgetUSDMicros, p.BudgetUSDMicrosPerDay} {
		if n < 0 {
			return nil, nil, governance.ErrSharedUnavailable
		}
	}
	if (t.QuotaOnExceeded != "" && t.QuotaOnExceeded != "block" && t.QuotaOnExceeded != "warn") ||
		(t.BudgetOnExceeded != "" && t.BudgetOnExceeded != "block" && t.BudgetOnExceeded != "warn") {
		return nil, nil, governance.ErrSharedUnavailable
	}
	add("team", p.Team, "rpm", "", "", t.RPM, true, "")
	add("team", p.Team, "tpm", "", "", t.TPM, true, "")
	add("team", p.Team, "tokens", "", "", t.TokensPerDay, t.QuotaOnExceeded != "warn", v1alpha1.PeriodCalendarDay)
	add("team", p.Team, "microUSD", "", "", t.BudgetUSDMicros, t.BudgetOnExceeded != "warn", v1alpha1.PeriodCalendarMonth)
	add("team", p.Team, "microUSD", "", "", t.BudgetUSDMicrosPerDay, t.BudgetOnExceeded != "warn", v1alpha1.PeriodCalendarDay)
	add("key", p.KeyID, "rpm", "", "", p.RPM, true, "")
	add("key", p.KeyID, "tpm", "", "", p.TPM, true, "")
	add("key", p.KeyID, "microUSD", "", "", p.BudgetUSDMicros, true, v1alpha1.PeriodCalendarMonth)
	add("key", p.KeyID, "microUSD", "", "", p.BudgetUSDMicrosPerDay, true, v1alpha1.PeriodCalendarDay)
	for _, doc := range docs {
		parsed, err := policy.FromV1Alpha1(&doc)
		if err != nil {
			return nil, nil, sharedError(err)
		}
		if !sharedMatch(parsed.Subject.Team, parsed.Subject.User, p) {
			continue
		}
		scope := sharedScope(parsed.Subject.Team, parsed.Subject.User)
		identity := sharedHash([]string{parsed.Subject.Team, parsed.Subject.User})
		for _, r := range parsed.Rules {
			if r.Rate != nil && !r.Rate.Unlimited {
				add(scope, identity, "rpm", parsed.Name, r.Name, r.Rate.RPM, true, "")
				add(scope, identity, "tpm", parsed.Name, r.Name, r.Rate.TPM, true, "")
			}
			if r.TokenQuota != nil {
				add(scope, identity, "tokens", parsed.Name, r.Name, r.TokenQuota.LimitTokens, true, r.TokenQuota.Period)
			}
		}
	}
	var budgets []policy.AuthorityBudget
	for _, b := range allBudgets {
		if !sharedMatch(b.Team, b.User, p) {
			continue
		}
		budgets = append(budgets, b)
		period := v1alpha1.PeriodCalendarMonth
		if b.WindowEnd.Sub(b.WindowStart) == 24*time.Hour {
			period = v1alpha1.PeriodCalendarDay
		}
		defs = append(defs, sharedDefinition{id: accountKey{"money:" + b.Key, b.WindowID}, scope: sharedScope(b.Team, b.User),
			kind: "microUSD", policy: b.Policy, rule: b.Rule, window: string(period), moneyKey: b.Key,
			limit: b.LimitMicroUSD, hard: b.HardCap, start: b.WindowStart, end: b.WindowEnd})
	}
	sort.Slice(defs, func(i, j int) bool {
		if defs[i].id.key != defs[j].id.key {
			return defs[i].id.key < defs[j].id.key
		}
		return defs[i].id.window < defs[j].id.window
	})
	return defs, budgets, nil
}

type sharedCounter struct {
	used, reserved, rate int64
	debt, refilled       big.Int
	updated              time.Time
	frozen               bool
}

func lockSharedCounters(ctx context.Context, tx pgx.Tx, defs []sharedDefinition, now time.Time, create bool) (map[accountKey]*sharedCounter, error) {
	out := make(map[accountKey]*sharedCounter, len(defs))
	wanted := make(map[accountKey]sharedDefinition, len(defs))
	for _, d := range defs {
		wanted[d.id] = d
	}
	for _, id := range orderedKeys(wanted) {
		d := wanted[id]
		if create {
			if _, err := tx.Exec(ctx, `INSERT INTO shared_counters(counter_key,window_id,rate,updated_at)
				VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, id.key, id.window, d.rate(), now); err != nil {
				return nil, err
			}
		}
		c := &sharedCounter{}
		var debt, refilled string
		err := tx.QueryRow(ctx, `SELECT used,reserved,debt::text,refilled::text,rate,updated_at,frozen
			FROM shared_counters WHERE counter_key=$1 AND window_id=$2 FOR UPDATE`, id.key, id.window).
			Scan(&c.used, &c.reserved, &debt, &refilled, &c.rate, &c.updated, &c.frozen)
		if err != nil {
			return nil, err
		}
		if _, ok := c.debt.SetString(debt, 10); !ok {
			return nil, errors.New("invalid rate debt")
		}
		if _, ok := c.refilled.SetString(refilled, 10); !ok {
			return nil, errors.New("invalid rate refill")
		}
		out[id] = c
	}
	return out, nil
}

func persistSharedCounters(ctx context.Context, tx pgx.Tx, cs map[accountKey]*sharedCounter) error {
	for _, id := range orderedKeys(cs) {
		c := cs[id]
		if _, err := tx.Exec(ctx, `UPDATE shared_counters SET used=$3,reserved=$4,debt=$5::numeric,
			refilled=$6::numeric,rate=$7,updated_at=$8,frozen=$9 WHERE counter_key=$1 AND window_id=$2`,
			id.key, id.window, c.used, c.reserved, c.debt.String(), c.refilled.String(), c.rate, c.updated, c.frozen); err != nil {
			return err
		}
	}
	return nil
}

func (c *sharedCounter) refill(now time.Time, rate int64) error {
	if now.Before(c.updated) {
		return errors.New("database clock moved backwards")
	}
	if c.rate > 0 {
		var delta big.Int
		delta.Sub(big.NewInt(now.UnixMicro()), big.NewInt(c.updated.UnixMicro()))
		// A cut must not forgive the old deficit using the obsolete faster
		// rate. An increase does not retroactively earn faster refill either.
		delta.Mul(&delta, big.NewInt(min(c.rate, rate)))
		if delta.Cmp(&c.debt) > 0 {
			delta.Set(&c.debt)
		}
		c.debt.Sub(&c.debt, &delta)
		c.refilled.Add(&c.refilled, &delta)
	}
	c.rate = rate
	c.updated = now
	return nil
}

func sharedScaled(amount int64) *big.Int {
	return new(big.Int).Mul(big.NewInt(amount), big.NewInt(sharedRateScale))
}

func sharedCeil(value *big.Int) (int64, error) {
	n := new(big.Int).Add(value, big.NewInt(sharedRateScale-1))
	n.Quo(n, big.NewInt(sharedRateScale))
	if !n.IsInt64() || n.Sign() < 0 {
		return 0, errors.New("rate accounting overflow")
	}
	return n.Int64(), nil
}

func sharedCheckCapacity(d sharedDefinition, c *sharedCounter, accounts map[accountKey]*account, amount int64) error {
	used := c.used
	if d.moneyKey != "" {
		a := accounts[accountKey{d.moneyKey, d.id.window}]
		var err error
		used, err = checkedAdd(a.hardEncumbered, a.softConsumed)
		if err != nil {
			return sharedError(err)
		}
	}
	next, err := checkedAdd(used, amount)
	if err != nil {
		return sharedError(err)
	}
	if _, err = checkedAdd(c.used, amount); err != nil {
		return sharedError(err)
	}
	if _, err = checkedAdd(c.reserved, amount); err != nil {
		return sharedError(err)
	}
	exhausted := next > d.limit
	if d.rate() > 0 {
		nextDebt := new(big.Int).Add(&c.debt, sharedScaled(amount))
		exhausted = nextDebt.Cmp(sharedScaled(d.limit)) > 0
	}
	if d.hard && exhausted {
		if d.kind == "microUSD" {
			return &governance.SharedDenial{Status: 402, Reason: "shared monetary budget exhausted"}
		}
		retry := 1
		if d.rate() > 0 {
			retry = 60
		}
		return &governance.SharedDenial{Status: 429, Reason: "shared rate or token quota exhausted", RetryAfter: retry}
	}
	return nil
}

func sharedBook(d sharedDefinition, c *sharedCounter, accounts map[accountKey]*account, amount int64) error {
	var err error
	if c.used, err = checkedAdd(c.used, amount); err != nil {
		return err
	}
	if c.reserved, err = checkedAdd(c.reserved, amount); err != nil {
		return err
	}
	if d.rate() > 0 {
		c.debt.Add(&c.debt, sharedScaled(amount))
	}
	if d.moneyKey != "" {
		a := accounts[accountKey{d.moneyKey, d.id.window}]
		if d.hard {
			a.hardEncumbered, err = checkedAdd(a.hardEncumbered, amount)
		} else {
			a.softConsumed, err = checkedAdd(a.softConsumed, amount)
			if err == nil {
				a.softPending, err = checkedAdd(a.softPending, amount)
			}
		}
	}
	return err
}
