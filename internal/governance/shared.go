package governance

import (
	"context"
	"errors"
	"time"
)

// SharedAuthority is an explicit deployment profile: one Postgres transaction
// reserves every applicable resource before a provider attempt. It is separate
// from node-local BudgetAuthority and never mixes their admission mechanisms.
type SharedAuthority interface {
	ReserveShared(context.Context, SharedRequest) (*SharedPermit, error)
	FinishShared(context.Context, *SharedPermit, SharedSettlement) error
	CancelShared(context.Context, *SharedPermit) error
	SharedUsage(context.Context, Subject) ([]SharedLimit, error)
	Ready(context.Context) error
}

type SharedRequest struct {
	Subject           Subject
	AuthRevision      string
	PolicyGeneration  string
	RequestedModel    string
	Model             string
	TokenBound        int64
	CostBoundMicroUSD int64
}

type SharedPermit struct {
	ID                string
	TokenBound        int64
	CostBoundMicroUSD int64
}

// Nil observations are unknown, not zero. Complete may release a dimension's
// unused bound only when that dimension's observation is present and valid.
type SharedSettlement struct {
	Tokens       *int64
	CostMicroUSD *int64
	Complete     bool
	InvalidUsage bool
}

type SharedLimit struct {
	Scope     string    `json:"scope"`
	Kind      string    `json:"kind"` // rpm, tpm, tokens, microUSD
	Policy    string    `json:"policy,omitempty"`
	Rule      string    `json:"rule,omitempty"`
	Window    string    `json:"window"`
	Limit     int64     `json:"limit"`
	Used      int64     `json:"used"` // committed, including reservations/uncertainty
	Reserved  int64     `json:"reserved,omitempty"`
	Remaining int64     `json:"remaining"`
	ResetsAt  time.Time `json:"resets_at,omitempty"`
	Hard      bool      `json:"hard"`
}

var ErrSharedUnavailable = errors.New("shared governance unavailable")

// SharedDenial is safe to expose: callers must use fixed reasons, never DB
// details, capabilities, hashes or key/user identifiers.
type SharedDenial struct {
	Status     int
	Reason     string
	RetryAfter int
}

func (e *SharedDenial) Error() string { return e.Reason }

func (g *Governor) SetSharedAuthority(s SharedAuthority) { g.shared = s }
func (g *Governor) HasSharedAuthority() bool             { return g != nil && g.shared != nil }
func (g *Governor) SharedAuthority() SharedAuthority {
	if g == nil {
		return nil
	}
	return g.shared
}

// ObserveShared is best-effort reporting after a committed settlement. It never
// participates in admission or fabricates a zero counter on lookup failure.
func (g *Governor) ObserveShared(ctx context.Context, subject Subject) {
	if !g.HasSharedAuthority() {
		return
	}
	limits, err := g.shared.SharedUsage(ctx, subject)
	if err != nil {
		return
	}
	var budgetPressure, quotaPressure float64
	for _, l := range limits {
		if l.Limit <= 0 {
			continue
		}
		ratio := float64(l.Used) / float64(l.Limit) // observability only
		if l.Scope == "team" && l.Kind == "microUSD" && l.Window == "CalendarMonth" {
			budgetPressure = max(budgetPressure, ratio)
			if g.notifyBudget != nil {
				g.notifyBudget(subject.Team, l.Used, l.Limit)
			}
		}
		if l.Scope == "key" && l.Kind == "microUSD" && l.Window == "CalendarMonth" && g.notifyKeyBudget != nil {
			g.notifyKeyBudget(subject.Team, subject.KeyID, l.Used, l.Limit)
		}
		if l.Scope == "team" && l.Kind == "tokens" && l.Window == "CalendarDay" {
			quotaPressure = max(quotaPressure, ratio)
		}
	}
	g.metrics.SetBudgetUtilization(subject.Team, budgetPressure)
	g.metrics.SetQuotaUtilization(subject.Team, "day", quotaPressure)
}
