package governance

import (
	"context"
	"errors"
)

var (
	ErrAuthorityUnavailable = errors.New("budget authority unavailable")
	ErrAuthorityExhausted   = errors.New("budget authority exhausted")
	ErrAuthorityInvalid     = errors.New("invalid budget authority state")
)

type BudgetPermit struct {
	ID            string
	BoundMicroUSD int64
	// InvalidUsage marks an observed violation of the declared token bounds or
	// cost representation. Retain authority and poison admission without
	// fabricating a monetary observation.
	InvalidUsage bool
}

// BudgetAuthority is deliberately independent of config, policy and server.
// Every implementation must commit a reservation before returning a permit.
// Incomplete outcomes retain their bound; actual cost is never fabricated.
type BudgetAuthority interface {
	Reserve(context.Context, Subject, int64) (*BudgetPermit, error)
	Finish(context.Context, *BudgetPermit, *int64, bool) error
}

func (g *Governor) SetBudgetAuthority(a BudgetAuthority) { g.authority = a }
func (g *Governor) HasBudgetAuthority() bool             { return g != nil && g.authority != nil }

func (g *Governor) ReserveBudget(ctx context.Context, s Subject, bound int64) (*BudgetPermit, error) {
	if !g.HasBudgetAuthority() {
		return nil, nil
	}
	return g.authority.Reserve(ctx, s, bound)
}

func (g *Governor) FinishBudget(ctx context.Context, permit *BudgetPermit, actual *int64, complete bool) error {
	if permit == nil || !g.HasBudgetAuthority() {
		return nil
	}
	return g.authority.Finish(ctx, permit, actual, complete)
}
