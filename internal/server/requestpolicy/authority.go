package requestpolicy

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/inferplane/inferplane/internal/audit"
	"github.com/inferplane/inferplane/internal/governance"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/live"
	"github.com/inferplane/inferplane/internal/pricing"
	"github.com/inferplane/inferplane/internal/router"
	"github.com/inferplane/inferplane/pkg/schema"
)

type budgetKey struct{}

var errMultiplicity = errors.New("durable budgets require a single generation per request")

type budgetAttempt struct {
	governor           *governance.Governor
	permit             *governance.BudgetPermit
	shared             *governance.SharedPermit
	subject            governance.Subject
	done               atomic.Bool
	table              *pricing.Table
	provider, upstream string
	window             int64
}

// ReserveBudget commits local escrow before the caller contacts its provider.
// The returned finalizer MUST run even after a panic/cancellation or early error;
// absent a proved complete result it retains the entire reservation as uncertain.
func ReserveBudget(req *http.Request, g *governance.Governor, p keystore.Principal, target router.ChainTarget, state *live.State, raw []byte) (*http.Request, func(), error) {
	if !g.HasBudgetAuthority() && !g.HasSharedAuthority() {
		return req, func() {}, nil
	}
	if err := singleGeneration(raw); err != nil {
		return req, nil, err
	}
	if state == nil {
		return req, nil, governance.ErrAuthorityUnavailable
	}
	model, ok := state.Route(target.Model)
	if !ok {
		return req, nil, governance.ErrAuthorityUnavailable
	}
	bound, err := state.Pricing().AuthorityBound(target.ProviderName, target.Upstream, model.ContextWindow, model.ContextWindow)
	if err != nil {
		return req, nil, governance.ErrAuthorityUnavailable
	}
	if g.HasSharedAuthority() {
		return reserveShared(req, g, p, target, state, model.ContextWindow, bound)
	}
	permit, err := g.ReserveBudget(req.Context(), governance.Subject{Team: p.Team, KeyID: p.KeyID, User: p.Owner}, bound)
	if err != nil {
		return req, nil, err
	}
	if permit == nil {
		return req, nil, governance.ErrAuthorityInvalid
	}
	a := &budgetAttempt{governor: g, permit: permit, table: state.Pricing(),
		provider: target.ProviderName, upstream: target.Upstream, window: model.ContextWindow}
	req = req.WithContext(context.WithValue(req.Context(), budgetKey{}, a))
	return req, func() { a.finish(nil, false) }, nil
}

func (a *budgetAttempt) finish(actual *int64, complete bool) {
	if a.shared != nil {
		a.finishShared(governance.SharedSettlement{CostMicroUSD: actual, Complete: complete})
		return
	}
	if !a.done.CompareAndSwap(false, true) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.governor.FinishBudget(ctx, a.permit, actual, complete); err != nil {
		// Disk/state errors must leave the reservation held, not fall back to
		// memory. Avoid including private journal/capability material in logs.
		log.Print("inferplane: budget authority finalization failed; reservation retained")
	}
}

func BudgetStatus(err error) int {
	var shared *governance.SharedDenial
	if errors.As(err, &shared) {
		return shared.Status
	}
	if errors.Is(err, governance.ErrSharedUnavailable) {
		return http.StatusServiceUnavailable
	}
	if errors.Is(err, errMultiplicity) {
		return http.StatusBadRequest
	}
	if errors.Is(err, governance.ErrAuthorityExhausted) {
		return http.StatusPaymentRequired
	}
	return http.StatusServiceUnavailable
}

// SettleBudget distinguishes actual observed cost from authority retained for
// uncertain outcomes. Missing/malformed usage can never prove a safe refund.
func SettleBudget(req *http.Request, _ *audit.CostRef, usage *schema.Usage, complete bool) {
	a, _ := req.Context().Value(budgetKey{}).(*budgetAttempt)
	if a == nil {
		return
	}
	if a.shared != nil {
		a.settleShared(usage, complete)
		return
	}
	u, exact, invalid := authorityUsage(usage, a.window)
	a.permit.InvalidUsage = invalid
	if !exact || invalid {
		a.finish(nil, false)
		return
	}
	actual, err := a.table.CostUSDMicrosChecked(a.provider, a.upstream, u)
	if err != nil {
		a.permit.InvalidUsage = true
		a.finish(nil, false)
		return
	}
	a.finish(&actual, complete)
}

// Inspect only generation controls, never arbitrary tool arguments. The
// supported APIs are single-generation here; opaque vendor extensions must not
// silently multiply output charged against one model window.
func singleGeneration(raw []byte) error {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return errMultiplicity
	}
	for _, key := range []string{"n", "best_of", "num_generations", "num_return_sequences", "candidate_count", "candidateCount"} {
		if value, ok := fields[key]; ok {
			var n int64
			if json.Unmarshal(value, &n) != nil || n != 1 {
				return errMultiplicity
			}
		}
	}
	for _, key := range []string{"inferenceConfig", "textGenerationConfig", "generation_config"} {
		if value, ok := fields[key]; ok {
			if err := singleGeneration(value); err != nil {
				return err
			}
		}
	}
	return nil
}

func authorityUsage(u *schema.Usage, window int64) (pricing.Usage, bool, bool) {
	var out pricing.Usage
	if u == nil {
		return out, false, false
	}
	values := []*int64{u.InputTokens, u.OutputTokens, u.CacheReadInputTokens, u.CacheCreationInputTokens}
	if u.CacheCreation != nil {
		values = append(values, u.CacheCreation.Ephemeral5mInputTokens, u.CacheCreation.Ephemeral1hInputTokens)
	}
	for _, v := range values {
		if v != nil && (*v < 0 || *v > window) {
			return out, false, true
		}
	}
	if u.AccountingUncertain {
		return out, false, false
	}
	if u.InputTokens == nil || u.OutputTokens == nil {
		return out, false, false
	}
	out.Input, out.Output = *u.InputTokens, *u.OutputTokens
	if u.CacheReadInputTokens != nil {
		out.CacheRead = *u.CacheReadInputTokens
	}
	total := int64(0)
	if u.CacheCreationInputTokens != nil {
		total = *u.CacheCreationInputTokens
	}
	if u.CacheCreation == nil {
		return out, total == 0, false
	}
	if u.CacheCreation.Ephemeral5mInputTokens == nil || u.CacheCreation.Ephemeral1hInputTokens == nil {
		return out, false, false // an omitted TTL category cannot prove a refund
	}
	out.CacheWrite5m, out.CacheWrite1h = *u.CacheCreation.Ephemeral5mInputTokens, *u.CacheCreation.Ephemeral1hInputTokens
	if u.CacheCreationInputTokens != nil &&
		(out.CacheWrite5m > total || out.CacheWrite1h != total-out.CacheWrite5m) {
		return out, false, true
	}
	return out, true, false
}
