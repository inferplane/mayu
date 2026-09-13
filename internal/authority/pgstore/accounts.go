package pgstore

import (
	"context"
	"errors"
	"math"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	v1alpha1 "github.com/inferplane/inferplane/api/v1alpha1"
	"github.com/inferplane/inferplane/internal/policy"
	"github.com/inferplane/inferplane/internal/tier"
)

type accountKey struct{ key, window string }

type account struct {
	hardEncumbered, hardConsumed, softConsumed int64
	// softPending is the in-flight subset of softConsumed. It remains
	// admission liability but cannot activate an accounting-only soft tier.
	softPending           int64
	acceptsMeters, frozen bool
}

func orderedKeys[V any](items map[accountKey]V) []accountKey {
	keys := make([]accountKey, 0, len(items))
	for k := range items {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].key != keys[j].key {
			return keys[i].key < keys[j].key
		}
		return keys[i].window < keys[j].window
	})
	return keys
}

// Historical accounts are immutable identities, not today's policy lookup.
// Removing a policy cannot make a late report vanish or forgive its liability.
func lockAccounts(ctx context.Context, tx pgx.Tx, owner string, req policy.AuthorityRequest, budgets []policy.AuthorityBudget) (map[accountKey]*account, error) {
	current := make(map[accountKey]policy.AuthorityBudget, len(budgets))
	wanted := make(map[accountKey]bool, len(budgets))
	for _, b := range budgets {
		k := accountKey{b.Key, b.WindowID}
		current[k] = b
		wanted[k] = true
	}
	for _, r := range req.Reports {
		g, err := reportGrant(ctx, tx, owner, r)
		if err != nil {
			return nil, err
		}
		wanted[accountKey{g.key, g.window}] = true
	}
	for _, m := range req.Meters {
		wanted[accountKey{m.Key, m.WindowID}] = true
	}
	accounts := make(map[accountKey]*account, len(wanted))
	for _, k := range orderedKeys(wanted) {
		b, active := current[k]
		if active {
			if _, err := tx.Exec(ctx, `INSERT INTO authority_accounts(budget_key,window_id,accepts_meters)
				VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, k.key, k.window, !b.HardCap); err != nil {
				return nil, databaseError("ensure budget account", err)
			}
		}
		a := &account{}
		err := tx.QueryRow(ctx, `SELECT hard_encumbered,hard_consumed,soft_consumed,soft_pending,accepts_meters,frozen
			FROM authority_accounts WHERE budget_key=$1 AND window_id=$2 FOR UPDATE`, k.key, k.window).
			Scan(&a.hardEncumbered, &a.hardConsumed, &a.softConsumed, &a.softPending, &a.acceptsMeters, &a.frozen)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errors.New("authority postgres: unknown historical budget window")
		}
		if err != nil {
			return nil, databaseError("lock budget account", err)
		}
		if active && !b.HardCap {
			a.acceptsMeters = true
		}
		accounts[k] = a
	}
	return accounts, nil
}

func persistAccounts(ctx context.Context, tx pgx.Tx, accounts map[accountKey]*account) error {
	for _, k := range orderedKeys(accounts) {
		a := accounts[k]
		if _, err := checkedAdd(a.hardEncumbered, a.softConsumed); err != nil {
			return err
		}
		if a.softPending < 0 || a.softPending > a.softConsumed {
			return errors.New("authority postgres: inconsistent pending soft accounting")
		}
		if _, err := tx.Exec(ctx, `UPDATE authority_accounts
			SET hard_encumbered=$3,hard_consumed=$4,soft_consumed=$5,accepts_meters=$6,frozen=$7,soft_pending=$8
			WHERE budget_key=$1 AND window_id=$2`, k.key, k.window,
			a.hardEncumbered, a.hardConsumed, a.softConsumed, a.acceptsMeters, a.frozen, a.softPending); err != nil {
			return databaseError("persist budget account", err)
		}
	}
	return nil
}

func activeTiers(ctx context.Context, tx pgx.Tx, docs []v1alpha1.GovernancePolicy, budgets []policy.AuthorityBudget, accounts map[accountKey]*account) ([]policy.ActiveTier, error) {
	type ruleKey struct{ policy, rule string }
	byRule := make(map[ruleKey]policy.AuthorityBudget, len(budgets))
	for _, b := range budgets {
		byRule[ruleKey{b.Policy, b.Rule}] = b
	}
	var out []policy.ActiveTier
	for _, doc := range docs {
		for _, rule := range doc.Spec.Rules {
			if rule.Routing == nil || rule.Routing.BudgetTiers == nil {
				continue
			}
			tr := rule.Routing.BudgetTiers
			b, ok := byRule[ruleKey{doc.Metadata.Name, tr.BudgetRef}]
			if !ok {
				return nil, errors.New("authority postgres: tier has no finite budget window")
			}
			a := accounts[accountKey{b.Key, b.WindowID}]
			consumed := a.hardConsumed
			soft := a.softConsumed
			if b.HardCap {
				consumed = a.hardEncumbered
			} else {
				// Soft routing meters observe terminal spend/uncertainty,
				// not an in-flight conservative bound. A hard policy still
				// judges every commitment, including pending soft bookings.
				soft -= a.softPending
			}
			percent := tier.UtilizedPercent(consumed, soft, b.LimitMicroUSD)
			threshold := 0
			for _, candidate := range tr.Tiers {
				if percent >= candidate.ThresholdPercent {
					threshold = candidate.ThresholdPercent
				}
			}
			// Store a threshold, not a slice index: changing a tier ladder
			// must not accidentally turn its previous index into a reset.
			if threshold > 0 {
				if _, err := tx.Exec(ctx, `INSERT INTO authority_tiers(budget_key,window_id,policy_name,rule_name,threshold)
					VALUES($1,$2,$3,$4,$5) ON CONFLICT(budget_key,window_id,policy_name,rule_name)
					DO UPDATE SET threshold=GREATEST(authority_tiers.threshold,excluded.threshold)`,
					b.Key, b.WindowID, doc.Metadata.Name, rule.Name, threshold); err != nil {
					return nil, databaseError("latch budget tier", err)
				}
			}
			err := tx.QueryRow(ctx, `SELECT threshold FROM authority_tiers
				WHERE budget_key=$1 AND window_id=$2 AND policy_name=$3 AND rule_name=$4`,
				b.Key, b.WindowID, doc.Metadata.Name, rule.Name).Scan(&threshold)
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			if err != nil {
				return nil, databaseError("read budget tier", err)
			}
			idx := -1
			for i, candidate := range tr.Tiers {
				if candidate.ThresholdPercent <= threshold {
					idx = i
				}
			}
			if idx < 0 {
				continue // The newly configured ladder has no latched tier.
			}
			selected := tr.Tiers[idx]
			out = append(out, policy.ActiveTier{
				Policy: doc.Metadata.Name, Rule: rule.Name, BudgetRef: tr.BudgetRef, Team: doc.Spec.Subject.Team,
				ThresholdPercent: selected.ThresholdPercent, Substitute: selected.Substitute, EnforceTargets: tr.EnforceTargets,
				User: doc.Spec.Subject.User,
			})
		}
	}
	return out, nil
}

func checkedAdd(a, b int64) (int64, error) {
	if a < 0 || b < 0 || a > math.MaxInt64-b {
		return 0, errors.New("authority postgres: invalid or overflowing accounting amount")
	}
	return a + b, nil
}

func validText(s string) bool {
	return s != "" && len(s) <= 1024 && !strings.ContainsRune(s, '\x00')
}

func validCapability(s string) bool { return validText(s) && len(s) >= 32 }

func validateRequest(owner string, req policy.AuthorityRequest) error {
	if req.Protocol != policy.AuthorityProtocol || !validText(owner) || !validText(req.Instance) {
		return errors.New("authority postgres: invalid protocol or node identity")
	}
	if len(req.Requests)+len(req.Reports)+len(req.Meters) > 1024 {
		return errors.New("authority postgres: sync batch too large")
	}
	requests := make(map[string]bool, len(req.Requests))
	for _, r := range req.Requests {
		if !validCapability(r.RequestID) || !validText(r.Key) || !validText(r.WindowID) || !validText(r.Revision) || r.WantMicroUSD < 0 {
			return errors.New("authority postgres: malformed grant request")
		}
		if requests[r.RequestID] {
			return errors.New("authority postgres: duplicate grant request")
		}
		requests[r.RequestID] = true
	}
	reports := make(map[string]bool, len(req.Reports))
	for _, r := range req.Reports {
		if !validText(r.Instance) || !validText(r.GrantID) || !validCapability(r.RequestID) ||
			r.Sequence <= 0 || r.Consumed < 0 || r.Observed < 0 ||
			(!r.Overrun && r.Observed > r.Consumed) || (r.Overrun && !r.Closed) {
			return errors.New("authority postgres: malformed grant report")
		}
		if reports[r.GrantID] {
			return errors.New("authority postgres: duplicate grant report")
		}
		reports[r.GrantID] = true
	}
	type meterKey struct{ instance, key, window string }
	meters := make(map[meterKey]bool, len(req.Meters))
	for _, m := range req.Meters {
		if !validText(m.Instance) || !validText(m.Key) || !validText(m.WindowID) ||
			m.Sequence <= 0 || m.Consumed < 0 || m.Observed < 0 || m.Observed > m.Consumed {
			return errors.New("authority postgres: malformed meter report")
		}
		k := meterKey{m.Instance, m.Key, m.WindowID}
		if meters[k] {
			return errors.New("authority postgres: duplicate meter report")
		}
		meters[k] = true
	}
	return nil
}
