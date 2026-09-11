// Package tier holds the ADR-041 budget-tier model substitution primitives
// shared by the control plane (which judges utilization and decides the
// active tier) and the data plane (which applies it at ingress): the
// request-path Table of per-team substitution maps, and the Latch that
// makes tier activation monotone within a budget window instead of flapping
// on every heartbeat's utilization sample.
//
// A leaf package: no dependency on router/server/governance, only on
// internal/policy for the wire ActiveTier shape it consumes — the same
// posture internal/proxy already takes toward policy.LeaseGrant.
package tier

import (
	"sync"
	"time"

	"github.com/inferplane/inferplane/internal/policy"
)

// Table is the request-path view of the currently active per-team
// substitution map: requested model name -> target model name. Reads are on
// the ingress hot path; writes happen once per heartbeat (control-plane
// mode) or once per governance evaluation (standalone mode).
type Table struct {
	mu     sync.RWMutex
	byTeam map[string]map[string]string
	strict map[string]map[string]string
}

// NewTable returns an empty table.
func NewTable() *Table {
	return &Table{byTeam: map[string]map[string]string{}}
}

// Get returns a defensive copy of team's active substitution map, or nil if
// no tier is active for it.
func (t *Table) Get(team string) map[string]string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	m := t.byTeam[team]
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// Constraints returns an owned map of strict source->target restrictions.
// An empty target denotes conflicting restrictions and must deny, never restore
// the original model. Legacy tiers cannot loosen these independent constraints.
func (t *Table) Constraints(team string) map[string]string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	m := t.strict[team]
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for from, to := range m {
		out[from] = to
	}
	return out
}

// winner tracks which active tier contributed one substitution key, so a
// conflict between two rules matching the same team can be resolved
// deterministically.
type winner struct {
	threshold    int
	policy, rule string
}

// higherPressure reports whether cand should win over cur: the tier with the
// higher threshold reflects deeper budget pressure and wins; ties break
// lexicographically by (policy, rule) so the outcome never depends on slice
// order.
func higherPressure(cand, cur winner) bool {
	if cand.threshold != cur.threshold {
		return cand.threshold > cur.threshold
	}
	if cand.policy != cur.policy {
		return cand.policy < cur.policy
	}
	return cand.rule < cur.rule
}

// Set replaces the table from one heartbeat's (or one local evaluation's)
// active tiers. Merge rule: the union of every active tier's substitution
// map, per team; when two active tiers of DIFFERENT rules disagree on the
// same requested-model key, the tier with the higher ThresholdPercent wins
// (it represents deeper budget pressure), ties broken by (Policy, Rule) for
// determinism. Exported so both the control-plane sync client (mayu) and a
// standalone local evaluator can drive it.
func (t *Table) Set(active []policy.ActiveTier) {
	byTeam := make(map[string]map[string]string, len(active))
	chosen := make(map[string]map[string]winner, len(active))
	strict := make(map[string]map[string]string)
	for _, a := range active {
		if a.EnforceTargets {
			if strict[a.Team] == nil {
				strict[a.Team] = make(map[string]string)
			}
			for from, to := range a.Substitute {
				if old, exists := strict[a.Team][from]; exists && old != to {
					strict[a.Team][from] = ""
				} else {
					strict[a.Team][from] = to
				}
			}
		}
		m, ok := byTeam[a.Team]
		if !ok {
			m = map[string]string{}
			byTeam[a.Team] = m
		}
		w, ok := chosen[a.Team]
		if !ok {
			w = map[string]winner{}
			chosen[a.Team] = w
		}
		cand := winner{threshold: a.ThresholdPercent, policy: a.Policy, rule: a.Rule}
		for from, to := range a.Substitute {
			if cur, exists := w[from]; exists && !higherPressure(cand, cur) {
				continue
			}
			w[from] = cand
			m[from] = to
		}
	}
	t.mu.Lock()
	t.byTeam = byTeam
	t.strict = strict
	t.mu.Unlock()
}

// Latch makes budget-tier activation monotone within one budget window:
// once a tier fires it stays active — even if a later sample's utilization
// dips (e.g. a large outstanding grant not yet reported as spent) — until
// the window key changes, at which point it resets to no-tier-active. This
// is the ADR-041 analogue of internal/alert's ratio-drop re-arm heuristic,
// but keyed on an explicit window identity instead of inferring rollover
// from a ratio decrease, so it cannot mistake within-window noise for a
// rollover. Safe for concurrent use.
type Latch struct {
	mu    sync.Mutex
	state map[string]latchEntry
}

type latchEntry struct {
	window    string
	tierIndex int // -1 = no tier active
}

// NewLatch returns an empty latch.
func NewLatch() *Latch {
	return &Latch{state: map[string]latchEntry{}}
}

// Evaluate returns the active tier index into thresholds (0-based, -1 for
// none) for one rule (identified by key, e.g. "policy/rule"). thresholds
// must be strictly increasing (FromV1Alpha1 validates this at load).
// percentUtilized is the caller's already-computed utilization, as a whole
// percent of the referenced budget rule's limit. window is the current
// budget-window identity (WindowKey); a change from the latch's stored
// window resets tierIndex to -1 before evaluating the new sample.
func (l *Latch) Evaluate(key, window string, thresholds []int, percentUtilized int) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.state[key]
	if !ok || e.window != window {
		e = latchEntry{window: window, tierIndex: -1}
	}
	crossed := -1
	for i, th := range thresholds {
		if percentUtilized >= th {
			crossed = i
		}
	}
	if crossed > e.tierIndex {
		e.tierIndex = crossed
	}
	// A latch survives a policy reload by design (ADR-041 D2), so a rule
	// edited to FEWER tiers can hold an index past the new slice — both
	// consumers index thresholds' companion tier list unchecked, so an
	// unclamped return is a per-heartbeat (control plane) or per-request
	// (standalone mayu) panic. Clamp to the deepest tier that still exists;
	// monotonicity within the window is unaffected.
	if e.tierIndex >= len(thresholds) {
		e.tierIndex = len(thresholds) - 1
	}
	l.state[key] = e
	return e.tierIndex
}

// Forget drops a rule's latch state, e.g. when its policy is removed on
// reload. A rule not passed to Forget simply keeps its state, which is what
// lets a latch survive a policy edit that leaves the rule's identity intact
// (ADR-041 D2: the latch must survive applyWire the same way lease spend
// does, or every policy edit would silently un-latch every team).
func (l *Latch) Forget(key string) {
	l.mu.Lock()
	delete(l.state, key)
	l.mu.Unlock()
}

// WindowKey derives the interim ADR-041 budget-window identity: calendar
// month UTC ("2026-08"). This is a stand-in for the control-plane-computed
// windowID that docs/roadmap.md item ② (durable ledger + owned budget
// windows) will introduce; once that lands, the latch should be keyed on
// the real windowID instead of re-deriving one from the clock.
func WindowKey(now time.Time) string {
	return now.UTC().Format("2006-01")
}
