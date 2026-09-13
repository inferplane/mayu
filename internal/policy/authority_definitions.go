package policy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/inferplane/inferplane/api/v1alpha1"
)

// ValidateAuthorityBundle prevents a syntactically valid but incomplete
// heartbeat from silently removing a hard budget on the data plane.
func ValidateAuthorityBundle(response *AuthorityResponse) error {
	if response == nil || response.Protocol != AuthorityProtocol || response.Policies == nil ||
		response.Budgets == nil || response.Generation != GenerationOf(response.Policies) {
		return fmt.Errorf("authority: incomplete policy bundle")
	}
	expected, err := AuthorityBudgets(response.Policies, response.ServerTime)
	if err != nil {
		return err
	}
	received := slices.Clone(response.Budgets)
	for i := range received {
		received[i].WindowStart = received[i].WindowStart.UTC()
		received[i].WindowEnd = received[i].WindowEnd.UTC()
	}
	slices.SortFunc(received, func(a, b AuthorityBudget) int {
		if a.Key < b.Key {
			return -1
		}
		if a.Key > b.Key {
			return 1
		}
		return 0
	})
	want, err := json.Marshal(expected)
	if err != nil {
		return fmt.Errorf("authority: definition encoding failed")
	}
	got, err := json.Marshal(received)
	if err != nil || !bytes.Equal(want, got) {
		return fmt.Errorf("authority: budget definitions do not match policy")
	}
	return nil
}

// AuthorityBudgets derives every finite budget from an authoritative policy
// snapshot and database time. Neither policy generation nor process identity
// changes an account/window key, so edits/restarts cannot replenish it.
func AuthorityBudgets(docs []v1alpha1.GovernancePolicy, databaseTime time.Time) ([]AuthorityBudget, error) {
	if databaseTime.IsZero() {
		return nil, fmt.Errorf("authority: missing database clock")
	}
	now := databaseTime.UTC()
	budgets := make([]AuthorityBudget, 0)
	seen := make(map[string]bool, len(docs))
	for i := range docs {
		if seen[docs[i].Metadata.Name] {
			return nil, fmt.Errorf("authority: duplicate policy")
		}
		seen[docs[i].Metadata.Name] = true
		p, err := FromV1Alpha1(&docs[i])
		if err != nil {
			return nil, fmt.Errorf("authority: invalid policy: %w", err)
		}
		for _, r := range p.Rules {
			if r.Budget == nil || r.Budget.Unlimited {
				continue
			}
			b := r.Budget
			start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
			end := start.AddDate(0, 1, 0)
			if b.Period == v1alpha1.PeriodCalendarDay {
				start = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
				end = start.AddDate(0, 0, 1)
			}
			key := authorityHash([]string{p.Name, r.Name, p.Subject.Team, p.Subject.User, string(b.Period)})
			seconds := int(3 * b.LeaseRenewInterval / time.Second)
			if seconds < 1 {
				return nil, fmt.Errorf("authority: invalid lease duration")
			}
			budgets = append(budgets, AuthorityBudget{
				Key: key, Policy: p.Name, Rule: r.Name, Team: p.Subject.Team, User: p.Subject.User,
				Revision: authorityHash(struct {
					Budget        *Budget
					FailurePolicy v1alpha1.FailurePolicy
				}{b, r.FailurePolicy}),
				WindowID:    authorityHash([]string{key, start.Format(time.RFC3339), end.Format(time.RFC3339)}),
				WindowStart: start, WindowEnd: end, LimitMicroUSD: b.LimitMicroUSD,
				GrantMicroUSD: b.LeaseGrantMicroUSD, LeaseSeconds: seconds, HardCap: b.HardCap,
			})
		}
	}
	slices.SortFunc(budgets, func(a, b AuthorityBudget) int {
		if a.Key < b.Key {
			return -1
		}
		if a.Key > b.Key {
			return 1
		}
		return 0
	})
	return budgets, nil
}

func authorityHash(value any) string {
	raw, _ := json.Marshal(value) // all callers use concrete JSON-safe scalar types
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
