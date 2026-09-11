package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/inferplane/inferplane/api/v1alpha1"
	"github.com/inferplane/inferplane/internal/budget"
	"github.com/inferplane/inferplane/internal/governance"
	"github.com/inferplane/inferplane/internal/limiter"
	"github.com/inferplane/inferplane/internal/policy"
	"github.com/inferplane/inferplane/internal/tier"
)

func TestE2EStrictBudgetCutoverKeepsCheapRetriesAndTotalCap(t *testing.T) {
	var paidCalls, cheapCalls atomic.Int64
	var cheapFails atomic.Bool
	upstream := func(calls *atomic.Int64, fail *atomic.Bool) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			if fail != nil && fail.Load() {
				http.Error(w, "unavailable", 503)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"msg","type":"message","role":"assistant","model":"up","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`)
		}))
	}
	paid, cheap := upstream(&paidCalls, nil), upstream(&cheapCalls, &cheapFails)
	defer paid.Close()
	defer cheap.Close()
	t.Setenv("BUDGET_ROUTE_TEST_KEY", "local-test-key")
	dataURL, adminURL, _ := bootGateway(t, func(cfg map[string]any, dir string) {
		cfg["providers"] = map[string]any{
			"paid":  map[string]any{"type": "anthropic", "base_url": paid.URL, "data_boundary": "external", "api_key_ref": map[string]any{"env": "BUDGET_ROUTE_TEST_KEY"}},
			"cheap": map[string]any{"type": "anthropic", "base_url": cheap.URL, "data_boundary": "internal", "api_key_ref": map[string]any{"env": "BUDGET_ROUTE_TEST_KEY"}},
		}
		cfg["models"] = map[string]any{
			"premium": map[string]any{"context_window": 100000, "targets": []any{map[string]any{"provider": "paid", "model": "up"}}},
			"economy": map[string]any{"context_window": 100000, "targets": []any{map[string]any{"provider": "cheap", "model": "up"}}},
		}
		cfg["model_fallbacks"] = map[string]string{"economy": "premium"} // must be removed after cutover
		cfg["pricing"] = map[string]any{"overrides": map[string]any{
			"paid":  map[string]any{"up": map[string]any{"input_per_mtok": 1000000, "output_per_mtok": 1000000}},
			"cheap": map[string]any{"up": map[string]any{"input_per_mtok": 100000, "output_per_mtok": 100000}},
		}}
		doc := v1alpha1.GovernancePolicy{
			TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: v1alpha1.KindGovernancePolicy},
			Metadata: v1alpha1.ObjectMeta{Name: "cutover", Generation: 1},
			Spec: v1alpha1.GovernancePolicySpec{Subject: v1alpha1.Subject{Team: "team"}, Rules: []v1alpha1.Rule{
				{Name: "switch", FailurePolicy: v1alpha1.FailOpen, Budget: &v1alpha1.BudgetRule{LimitMilliUSD: 10000}},
				{Name: "total", FailurePolicy: v1alpha1.FailClosed, Budget: &v1alpha1.BudgetRule{LimitMilliUSD: 20000, HardCap: true}},
				{Name: "route", FailurePolicy: v1alpha1.FailOpen, Routing: &v1alpha1.RoutingRule{BudgetTiers: &v1alpha1.BudgetTiersRule{
					BudgetRef: "switch", EnforceTargets: true, Tiers: []v1alpha1.BudgetTier{{ThresholdPercent: 100, Substitute: map[string]string{"premium": "economy"}}},
				}}},
			}},
		}
		body, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "cutover.json")
		if err := os.WriteFile(path, body, 0600); err != nil {
			t.Fatal(err)
		}
		cfg["policies"] = []string{path}
	})
	_, key := createKey(t, adminURL, "team", []string{"premium", "economy"})
	request := func() int {
		resp := postMessages(t, dataURL, key, "premium")
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := request(); got != 200 || paidCalls.Load() != 1 {
		t.Fatalf("initial paid route failed: %d", got)
	}
	if got := request(); got != 200 || paidCalls.Load() != 1 || cheapCalls.Load() != 1 {
		t.Fatalf("exhausted switch threshold blocked cheap service: status=%d paid=%d cheap=%d", got, paidCalls.Load(), cheapCalls.Load())
	}
	cheapFails.Store(true)
	if got := request(); got != 503 || paidCalls.Load() != 1 {
		t.Fatalf("cheap failure escaped to paid fallback: status=%d paid=%d", got, paidCalls.Load())
	}
	cheapFails.Store(false)
	blocked := false
	for i := 0; i < 10; i++ {
		if got := request(); got == http.StatusPaymentRequired {
			blocked = true
			break
		} else if got != 200 {
			t.Fatalf("unexpected response before total exhaustion: %d", got)
		}
	}
	if !blocked || paidCalls.Load() != 1 {
		t.Fatalf("total cap lost or paid route restored: blocked=%v paid=%d", blocked, paidCalls.Load())
	}
}

func TestE2EPolicyCannotRaiseConfiguredHardBudget(t *testing.T) {
	up := newAnthropicUpstream(t)
	dataURL, adminURL, _ := bootGateway(t, func(cfg map[string]any, dir string) {
		withAnthropicProvider(up.srv.URL)(cfg, dir)
		cfg["teams"] = map[string]any{"bounded": map[string]any{
			"budget": map[string]any{"usd_per_month": 0.001, "on_exceeded": "block"},
		}}
		cfg["pricing"] = map[string]any{"overrides": map[string]any{
			"up": map[string]any{"claude-test": map[string]any{"input_per_mtok": 1000000, "output_per_mtok": 1000000}},
		}}
		doc := v1alpha1.GovernancePolicy{
			TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: v1alpha1.KindGovernancePolicy},
			Metadata: v1alpha1.ObjectMeta{Name: "larger-cap", Generation: 1},
			Spec: v1alpha1.GovernancePolicySpec{Subject: v1alpha1.Subject{Team: "bounded"}, Rules: []v1alpha1.Rule{{
				Name: "cap", FailurePolicy: v1alpha1.FailClosed, Budget: &v1alpha1.BudgetRule{LimitMilliUSD: 1000000000, HardCap: true},
			}}},
		}
		body, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "budget-policy.json")
		if err := os.WriteFile(path, body, 0600); err != nil {
			t.Fatal(err)
		}
		cfg["policies"] = []string{path}
	})
	_, key := createKey(t, adminURL, "bounded", []string{"claude-test"})
	first := postMessages(t, dataURL, key, "claude-test")
	io.Copy(io.Discard, first.Body)
	first.Body.Close()
	if first.StatusCode != http.StatusOK {
		t.Fatalf("initial request status=%d", first.StatusCode)
	}
	second := postMessages(t, dataURL, key, "claude-test")
	defer second.Body.Close()
	if second.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("policy widened configured cap: second request status=%d", second.StatusCode)
	}
}

func TestStandaloneStrictTierUsesReferencedDailySpend(t *testing.T) {
	store := policy.NewEmptyStore()
	doc := v1alpha1.GovernancePolicy{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: v1alpha1.KindGovernancePolicy},
		Metadata: v1alpha1.ObjectMeta{Name: "switch", Generation: 1},
		Spec: v1alpha1.GovernancePolicySpec{Subject: v1alpha1.Subject{Team: "team"}, Rules: []v1alpha1.Rule{
			{Name: "threshold", FailurePolicy: v1alpha1.FailOpen, Budget: &v1alpha1.BudgetRule{LimitMilliUSD: 100, Period: v1alpha1.PeriodCalendarDay}},
			{Name: "route", FailurePolicy: v1alpha1.FailOpen, Routing: &v1alpha1.RoutingRule{BudgetTiers: &v1alpha1.BudgetTiersRule{
				BudgetRef: "threshold", EnforceTargets: true,
				Tiers: []v1alpha1.BudgetTier{{ThresholdPercent: 100, Substitute: map[string]string{"premium": "economy"}}},
			}}},
		}},
	}
	if rejected := store.ApplyWire([]v1alpha1.GovernancePolicy{doc}); len(rejected) != 0 {
		t.Fatal(rejected)
	}
	spend := budget.NewMemory()
	gov := governance.NewGovernor(map[string]governance.TeamPolicy{
		"team": {BudgetMicrosPerMonth: 1000000, BudgetMicrosPerDay: 1000000},
	}, limiter.NewMemory(), spend, nil)
	day, month := budget.CalendarDayIn(time.UTC), budget.CalendarMonthIn(time.UTC)
	spend.Debit(budget.Key(budget.ScopeTeam, "team", month), 500000, month)
	latch := tier.NewLatch()
	if got := standaloneActiveTierSubstitutions(store, gov, latch, "team", time.Now().UTC()); len(got) != 0 {
		t.Fatalf("monthly spend incorrectly exhausted daily threshold: %v", got)
	}
	spend.Debit(budget.Key(budget.ScopeTeam, "team", day), 100000, day)
	if got := standaloneActiveTierSubstitutions(store, gov, latch, "team", time.Now().UTC()); got["premium"] != "economy" {
		t.Fatalf("exact daily exhaustion did not activate cutover: %v", got)
	}
}
