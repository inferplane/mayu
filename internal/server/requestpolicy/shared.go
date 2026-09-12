package requestpolicy

import (
	"context"
	"errors"
	"log"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/inferplane/inferplane/internal/governance"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/live"
	"github.com/inferplane/inferplane/internal/router"
	"github.com/inferplane/inferplane/pkg/schema"
)

func reserveShared(req *http.Request, g *governance.Governor, p keystore.Principal, target router.ChainTarget, state *live.State, window, costBound int64) (*http.Request, func(), error) {
	if g.HasBudgetAuthority() || window <= 0 || window > math.MaxInt64/5 {
		return req, nil, governance.ErrSharedUnavailable
	}
	bundle, _ := req.Context().Value(bundleKey{}).(bundleRef)
	subject := governance.Subject{Team: p.Team, KeyID: p.KeyID, User: p.Owner}
	permit, err := g.SharedAuthority().ReserveShared(req.Context(), governance.SharedRequest{
		Subject: subject, AuthRevision: p.SharedRevision, PolicyGeneration: bundle.generation,
		RequestedModel: bundle.requested, Model: target.Model, TokenBound: 5 * window, CostBoundMicroUSD: costBound,
	})
	if err != nil {
		return req, nil, err
	}
	if permit == nil {
		return req, nil, governance.ErrSharedUnavailable
	}
	a := &budgetAttempt{governor: g, shared: permit, subject: subject, table: state.Pricing(),
		provider: target.ProviderName, upstream: target.Upstream, window: window}
	req = req.WithContext(context.WithValue(req.Context(), budgetKey{}, a))
	return req, func() { a.finishShared(governance.SharedSettlement{}) }, nil
}

func (a *budgetAttempt) settleShared(usage *schema.Usage, complete bool) {
	u, exactCost, invalid := authorityUsage(usage, a.window)
	tokens, exactTokens, invalidTokens := authorityTokens(usage, a.window)
	settlement := governance.SharedSettlement{Complete: complete, InvalidUsage: invalid || invalidTokens}
	if exactTokens && !settlement.InvalidUsage {
		settlement.Tokens = &tokens
	}
	if exactCost && !settlement.InvalidUsage {
		cost, err := a.table.CostUSDMicrosChecked(a.provider, a.upstream, u)
		if err != nil {
			settlement.InvalidUsage = true
		} else {
			settlement.CostMicroUSD = &cost
		}
	}
	a.finishShared(settlement)
}

func (a *budgetAttempt) finishShared(settlement governance.SharedSettlement) {
	if !a.done.CompareAndSwap(false, true) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.governor.SharedAuthority().FinishShared(ctx, a.shared, settlement); err != nil {
		log.Print("inferplane: shared finalization failed; committed reservation retained")
		return
	}
	a.governor.ObserveShared(ctx, a.subject)
}

// Token quantity can be exact even when the cache TTL price is unknown. Count
// either the aggregate creation count or a complete split, never both.
func authorityTokens(u *schema.Usage, window int64) (int64, bool, bool) {
	if u == nil {
		return 0, false, false
	}
	_, _, invalid := authorityUsage(u, window)
	if invalid {
		return 0, false, true
	}
	if u.AccountingUncertain || u.InputTokens == nil || u.OutputTokens == nil {
		return 0, false, false
	}
	counts := []*int64{u.InputTokens, u.OutputTokens, u.CacheReadInputTokens}
	var creation int64
	if u.CacheCreationInputTokens != nil {
		creation = *u.CacheCreationInputTokens
		if cc := u.CacheCreation; cc != nil {
			known := int64(0)
			for _, n := range []*int64{cc.Ephemeral5mInputTokens, cc.Ephemeral1hInputTokens} {
				if n != nil {
					if *n < 0 || *n > creation-known {
						return 0, false, true
					}
					known += *n
				}
			}
		}
	} else if cc := u.CacheCreation; cc != nil {
		if cc.Ephemeral5mInputTokens == nil || cc.Ephemeral1hInputTokens == nil {
			return 0, false, false
		}
		if *cc.Ephemeral5mInputTokens > math.MaxInt64-*cc.Ephemeral1hInputTokens {
			return 0, false, true
		}
		creation = *cc.Ephemeral5mInputTokens + *cc.Ephemeral1hInputTokens
	}
	counts = append(counts, &creation)
	var total int64
	for _, n := range counts {
		if n != nil {
			if *n < 0 || total > math.MaxInt64-*n {
				return 0, false, true
			}
			total += *n
		}
	}
	return total, true, false
}

func BudgetErrorHeaders(w http.ResponseWriter, err error) {
	var denied *governance.SharedDenial
	if errors.As(err, &denied) && denied.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(denied.RetryAfter))
	} else if BudgetStatus(err) == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", "1")
	}
}

// TeamSnapshot uses the same revision authenticated by KeyAuth. A missing team
// in a captured shared snapshot is authoritative absence, never a live fallback.
func TeamSnapshot(p keystore.Principal, lookup func(string) (keystore.TeamRecord, bool)) (keystore.TeamRecord, bool) {
	if p.TeamSnapshotLoaded {
		if p.TeamSnapshot == nil {
			return keystore.TeamRecord{}, false
		}
		return *p.TeamSnapshot, true
	}
	if lookup != nil {
		rec, ok := lookup(p.Team)
		if ok {
			return rec, true
		}
	}
	return keystore.TeamRecord{}, false
}
