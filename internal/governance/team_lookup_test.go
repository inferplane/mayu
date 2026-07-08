package governance

import (
	"testing"
	"time"

	"github.com/inferplane/inferplane/internal/budget"
	"github.com/inferplane/inferplane/internal/limiter"
	"github.com/inferplane/inferplane/internal/pricing"
)

// budgetWindow mirrors PreCheck/Settle's own 30-day budget window — a Debit
// with window 0 would expire itself before the very next Check under the
// budget.Memory implementation (windowEnd == the Debit's own timestamp).
const budgetWindow = 30 * 24 * time.Hour

// TestTeamLookup_dbOnlyTeamEnforced proves a team with NO config entry, known
// only to the dynamic lookup (the D3 keystore-record case), is governed —
// today's "unknown team is ungoverned" only applies when neither source has it.
func TestTeamLookup_dbOnlyTeamEnforced(t *testing.T) {
	g := NewGovernor(map[string]TeamPolicy{}, limiter.NewMemory(), budget.NewMemory(), nil)
	g.SetTeamLookup(func(team string) (TeamPolicy, bool) {
		if team == "db-team" {
			return TeamPolicy{BudgetMicrosPerMonth: 1, BudgetExceeded: "block"}, true
		}
		return TeamPolicy{}, false
	})
	g.bud.Debit("budget:db-team", 2, budgetWindow) // spend past the 1-µUSD budget
	dec := g.PreCheck("db-team", "", KeyPolicy{}, 0)
	if dec.Allowed || dec.Status != 402 {
		t.Fatalf("db-only team budget must enforce: %+v", dec)
	}
}

// TestTeamLookup_recordWinsOverConfig pins ADR-016's precedence rule: when the
// SAME team name exists in both the static config map and the dynamic lookup,
// the lookup value must win (never silently shadowed by the config file).
func TestTeamLookup_recordWinsOverConfig(t *testing.T) {
	g := NewGovernor(map[string]TeamPolicy{
		"t": {BudgetMicrosPerMonth: 1_000_000_000, BudgetExceeded: "block"}, // config: effectively unlimited
	}, limiter.NewMemory(), budget.NewMemory(), nil)
	g.SetTeamLookup(func(team string) (TeamPolicy, bool) {
		return TeamPolicy{BudgetMicrosPerMonth: 1, BudgetExceeded: "block"}, true // record: 1 µUSD
	})
	g.bud.Debit("budget:t", 2, budgetWindow)
	dec := g.PreCheck("t", "", KeyPolicy{}, 0)
	if dec.Allowed || dec.Status != 402 {
		t.Fatalf("record policy must win over config for the same team: %+v", dec)
	}
}

// TestTeamLookup_missFallsThroughToConfig proves a lookup miss (ok=false) for
// a team the config DOES know about still applies the config policy —
// existing config-only teams are not silently ungoverned by installing a lookup.
func TestTeamLookup_missFallsThroughToConfig(t *testing.T) {
	g := NewGovernor(map[string]TeamPolicy{
		"t": {BudgetMicrosPerMonth: 1, BudgetExceeded: "block"},
	}, limiter.NewMemory(), budget.NewMemory(), nil)
	g.SetTeamLookup(func(string) (TeamPolicy, bool) { return TeamPolicy{}, false })
	g.bud.Debit("budget:t", 2, budgetWindow)
	dec := g.PreCheck("t", "", KeyPolicy{}, 0)
	if dec.Allowed || dec.Status != 402 {
		t.Fatalf("lookup miss must fall through to config policy: %+v", dec)
	}
}

// TestTeamLookup_nilLookupIsConfigOnlyBackCompat proves a Governor that never
// calls SetTeamLookup behaves exactly as before — the zero-value g.lookup is
// nil and policyOf must not panic or otherwise change behavior.
func TestTeamLookup_nilLookupIsConfigOnlyBackCompat(t *testing.T) {
	g := testGovernor() // config-only team "t", no SetTeamLookup call
	dec := g.PreCheck("t", "", KeyPolicy{}, 100)
	if !dec.Allowed {
		t.Fatalf("nil lookup must not change existing config-only behavior: %+v", dec)
	}
}

// TestTeamLookup_dynamicChangeTakesEffectNextCallNoRestart is the core D3
// claim: mutating what the lookup returns BETWEEN two PreCheck calls changes
// enforcement on the very next call — no Governor rebuild, no reload.
func TestTeamLookup_dynamicChangeTakesEffectNextCallNoRestart(t *testing.T) {
	g := NewGovernor(map[string]TeamPolicy{}, limiter.NewMemory(), budget.NewMemory(), nil)
	current := TeamPolicy{} // starts ungoverned
	g.SetTeamLookup(func(string) (TeamPolicy, bool) {
		if current == (TeamPolicy{}) {
			return TeamPolicy{}, false
		}
		return current, true
	})

	if dec := g.PreCheck("t", "", KeyPolicy{}, 0); !dec.Allowed {
		t.Fatalf("team must start ungoverned: %+v", dec)
	}

	current = TeamPolicy{BudgetMicrosPerMonth: 1, BudgetExceeded: "block"}
	g.bud.Debit("budget:t", 2, budgetWindow)
	if dec := g.PreCheck("t", "", KeyPolicy{}, 0); dec.Allowed {
		t.Fatal("edited policy must enforce on the very next PreCheck call, no restart")
	}
}

// TestTeamLookup_settleDebitsSameCounterKeyPreCheckReads proves Settle debits
// the SAME counter key ("budget:"+team) that PreCheck reads, regardless of
// whether the policy came from the lookup or the config map — a policy-source
// switch must not orphan or fork counters.
func TestTeamLookup_settleDebitsSameCounterKeyPreCheckReads(t *testing.T) {
	g := NewGovernor(map[string]TeamPolicy{}, limiter.NewMemory(), budget.NewMemory(), nil)
	g.SetTeamLookup(func(team string) (TeamPolicy, bool) {
		return TeamPolicy{BudgetMicrosPerMonth: 1000, BudgetExceeded: "block"}, true
	})
	cost, missing := g.Settle("t", "", KeyPolicy{}, "p", "m", pricing.Usage{Input: 1000, Output: 500}, testTable())
	if missing || cost != 1500 {
		t.Fatalf("settle cost=%d missing=%v, want 1500/false", cost, missing)
	}
	// The 1500 µUSD debit must have landed on "budget:t" — the same key
	// PreCheck reads — so a subsequent PreCheck against the now-exceeded
	// 1000 µUSD budget must block.
	if dec := g.PreCheck("t", "", KeyPolicy{}, 0); dec.Allowed {
		t.Fatal("Settle's debit under a lookup-sourced policy must be visible to the next PreCheck via the same counter key")
	}
}
