package controlplane

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/inferplane/inferplane/api/v1alpha1"
	"github.com/inferplane/inferplane/internal/policy"
)

func TestSyncRejectsNegativeConsumptionBeforeMutation(t *testing.T) {
	for _, tc := range []struct {
		name, dataplane, badRule string
		badPeriod                v1alpha1.BudgetPeriod
		spent                    int64
	}{
		{name: "new dataplane", dataplane: "new", badRule: "cap", spent: -1},
		{name: "existing dataplane", dataplane: "existing", badRule: "cap", spent: math.MinInt64},
		{name: "stale period", dataplane: "existing", badRule: "cap", badPeriod: v1alpha1.PeriodCalendarDay, spent: -1},
		{name: "unknown rule", dataplane: "new", badRule: "unknown", spent: -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, ts := newTierTestServer(t, "sync-token")
			now := time.Now()
			s.now = func() time.Time { return now }
			doSync(t, ts.URL, "sync-token", policy.SyncRequest{
				Dataplane: "existing",
				Reports: []policy.ConsumptionReport{{
					Policy: "team-a", Rule: "cap", Team: "alpha", SpentMicroUSD: 20_000,
				}},
			})
			beforeLedger, beforeDataplanes := accountingSnapshot(s)
			// An accepted request would also prune the existing dataplane.
			s.mu.Lock()
			now = now.Add(pruneAfter + time.Hour)
			s.mu.Unlock()
			body, err := json.Marshal(policy.SyncRequest{
				Dataplane:   tc.dataplane,
				APIVersions: []string{"changed"},
				Generation:  "changed",
				Rejections:  []policy.Rejection{{Policy: "changed", Reason: "changed"}},
				Reports: []policy.ConsumptionReport{
					// A valid report before the invalid one must not partially
					// mutate spend, grant authority, or activate a routing tier.
					{Policy: "team-a", Rule: "cap", Team: "alpha", SpentMicroUSD: 90_000},
					{Policy: "team-a", Rule: tc.badRule, Team: "alpha", Period: tc.badPeriod, SpentMicroUSD: tc.spent},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1alpha1/sync", bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer sync-token")
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			payload, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("negative consumption status=%d, want 400; body=%s", resp.StatusCode, payload)
			}
			if bytes.Contains(payload, []byte(`"allowanceMicroUSD"`)) {
				t.Errorf("invalid report received lease authority: %s", payload)
			}
			afterLedger, afterDataplanes := accountingSnapshot(s)
			if !reflect.DeepEqual(beforeLedger, afterLedger) || !reflect.DeepEqual(beforeDataplanes, afterDataplanes) {
				t.Error("invalid batch mutated accounting or dataplane registration")
			}
		})
	}
}

func accountingSnapshot(s *Server) (map[ruleKey]ruleLedger, map[string]dpInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ledger := make(map[ruleKey]ruleLedger, len(s.ledger))
	for key, row := range s.ledger {
		copy := *row
		copy.spent, copy.allowance = maps.Clone(row.spent), maps.Clone(row.allowance)
		ledger[key] = copy
	}
	dataplanes := make(map[string]dpInfo, len(s.dataplanes))
	for id, dp := range s.dataplanes {
		copy := *dp
		copy.APIVersions, copy.Rejections = slices.Clone(dp.APIVersions), slices.Clone(dp.Rejections)
		dataplanes[id] = copy
	}
	return ledger, dataplanes
}

func TestSoftBudgetReloadAsRoutingOnlyDropsAllowancePreservesSpend(t *testing.T) {
	for _, period := range []v1alpha1.BudgetPeriod{v1alpha1.PeriodCalendarDay, v1alpha1.PeriodCalendarMonth} {
		for _, spent := range []int64{0, 25_000} {
			t.Run(fmt.Sprintf("%s/spent=%d", period, spent), func(t *testing.T) {
				s, err := NewServer("", t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				now := time.Now()
				s.now = func() time.Time { return now }
				doc := switchingBudgetDocument(period, false)
				if err := s.applyWire([]v1alpha1.GovernancePolicy{doc}, nil); err != nil {
					t.Fatal(err)
				}
				mux := http.NewServeMux()
				s.Mount(mux)
				ts := httptest.NewServer(mux)
				defer ts.Close()
				req := policy.SyncRequest{Dataplane: "dp", Reports: []policy.ConsumptionReport{
					{Policy: "team", Rule: "switch", Team: "alpha", Period: period, SpentMicroUSD: spent},
					{Policy: "team", Rule: "total", Team: "alpha", Period: period, SpentMicroUSD: spent},
				}}
				first := doSync(t, ts.URL, "", req)
				if len(first.Leases) != 2 {
					t.Fatalf("initial soft and hard admission leases missing: %+v", first.Leases)
				}
				before, _ := accountingSnapshot(s)
				switchKey, totalKey := ruleKey{"team", "switch"}, ruleKey{"team", "total"}
				if before[switchKey].allowance["dp"] != 100_000 {
					t.Fatalf("fixture did not reserve the soft budget: %+v", before[switchKey])
				}
				doc.Spec.Rules = append(doc.Spec.Rules, strictSwitchRule())
				if err := s.applyWire([]v1alpha1.GovernancePolicy{doc}, nil); err != nil {
					t.Fatal(err)
				}
				after, _ := accountingSnapshot(s)
				if !after[switchKey].routingOnly || len(after[switchKey].allowance) != 0 {
					t.Errorf("routing-only rule retained obsolete allowance: %+v", after[switchKey])
				}
				if !reflect.DeepEqual(before[switchKey].spent, after[switchKey].spent) {
					t.Error("routing-only transition discarded settled spend")
				}
				if !reflect.DeepEqual(before[totalKey], after[totalKey]) {
					t.Error("routing-only transition changed the independent hard budget")
				}
				for range 2 {
					next := doSync(t, ts.URL, "", req)
					if len(next.Leases) != 1 || next.Leases[0].Rule != "total" || !next.Leases[0].HardCap {
						t.Errorf("switch threshold issued admission authority: %+v", next.Leases)
					}
					if len(next.ActiveTiers) != 0 {
						t.Errorf("unspent allowance activated switching threshold: %+v", next.ActiveTiers)
					}
					s.mu.Lock()
					now = now.Add(time.Hour)
					s.mu.Unlock()
				}
				req.Reports[0].SpentMicroUSD = 100_000
				exhausted := doSync(t, ts.URL, "", req)
				if len(exhausted.ActiveTiers) != 1 || !exhausted.ActiveTiers[0].EnforceTargets {
					t.Fatalf("actual threshold spend did not activate strict routing: %+v", exhausted.ActiveTiers)
				}
			})
		}
	}
}

func TestStrictReferencePreservesHardBudgetLeaseOnReload(t *testing.T) {
	s, err := NewServer("", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	doc := switchingBudgetDocument(v1alpha1.PeriodCalendarMonth, true)
	if err := s.applyWire([]v1alpha1.GovernancePolicy{doc}, nil); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	s.Mount(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()
	doSync(t, ts.URL, "", policy.SyncRequest{Dataplane: "dp"})
	before, _ := accountingSnapshot(s)
	doc.Spec.Rules = append(doc.Spec.Rules, strictSwitchRule())
	if err := s.applyWire([]v1alpha1.GovernancePolicy{doc}, nil); err != nil {
		t.Fatal(err)
	}
	after, _ := accountingSnapshot(s)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("adding a strict reference changed hard budget accounting")
	}
	next := doSync(t, ts.URL, "", policy.SyncRequest{Dataplane: "dp"})
	if len(next.Leases) != 2 {
		t.Fatalf("strict reference removed hard budget authority: %+v", next.Leases)
	}
	for _, lease := range next.Leases {
		if !lease.HardCap {
			t.Fatalf("hard budget lease lost its flag: %+v", lease)
		}
	}
}

func TestRoutingOnlyTransitionReevaluatesPreviouslyLatchedOptionalTier(t *testing.T) {
	for _, spent := range []int64{0, 100_000} {
		t.Run(fmt.Sprintf("spent=%d", spent), func(t *testing.T) {
			s, err := NewServer("", t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			doc := switchingBudgetDocument(v1alpha1.PeriodCalendarMonth, false)
			optional := strictSwitchRule()
			optional.Routing.BudgetTiers.EnforceTargets = false
			optional.Routing.BudgetTiers.Tiers[0].ThresholdPercent = 80
			doc.Spec.Rules = append(doc.Spec.Rules, optional)
			if err := s.applyWire([]v1alpha1.GovernancePolicy{doc}, nil); err != nil {
				t.Fatal(err)
			}
			mux := http.NewServeMux()
			s.Mount(mux)
			ts := httptest.NewServer(mux)
			defer ts.Close()
			req := policy.SyncRequest{Dataplane: "dp", Reports: []policy.ConsumptionReport{{
				Policy: "team", Rule: "switch", Team: "alpha", SpentMicroUSD: spent,
			}}}
			first := doSync(t, ts.URL, "", req)
			if len(first.ActiveTiers) != 1 || first.ActiveTiers[0].EnforceTargets {
				t.Fatalf("fixture did not latch the optional tier: %+v", first.ActiveTiers)
			}
			// Keep the same rule identity, replacing only its accounting mode.
			doc.Spec.Rules[2] = strictSwitchRule()
			if err := s.applyWire([]v1alpha1.GovernancePolicy{doc}, nil); err != nil {
				t.Fatal(err)
			}
			next := doSync(t, ts.URL, "", req)
			if spent == 0 && len(next.ActiveTiers) != 0 {
				t.Fatalf("obsolete reservation survived in the tier latch: %+v", next.ActiveTiers)
			}
			if spent == 100_000 && (len(next.ActiveTiers) != 1 || !next.ActiveTiers[0].EnforceTargets) {
				t.Fatalf("real settled spend did not reactivate the strict tier: %+v", next.ActiveTiers)
			}
		})
	}
}

func switchingBudgetDocument(period v1alpha1.BudgetPeriod, hard bool) v1alpha1.GovernancePolicy {
	failure := v1alpha1.FailOpen
	if hard {
		failure = v1alpha1.FailClosed
	}
	return v1alpha1.GovernancePolicy{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: v1alpha1.KindGovernancePolicy},
		Metadata: v1alpha1.ObjectMeta{Name: "team", Generation: 1},
		Spec: v1alpha1.GovernancePolicySpec{Subject: v1alpha1.Subject{Team: "alpha"}, Rules: []v1alpha1.Rule{
			{Name: "switch", FailurePolicy: failure, Budget: &v1alpha1.BudgetRule{
				LimitMilliUSD: 100, Period: period, HardCap: hard, Lease: v1alpha1.LeaseSpec{GrantMilliUSD: 100},
			}},
			{Name: "total", FailurePolicy: v1alpha1.FailClosed, Budget: &v1alpha1.BudgetRule{
				LimitMilliUSD: 1000, Period: period, HardCap: true, Lease: v1alpha1.LeaseSpec{GrantMilliUSD: 100},
			}},
		}},
	}
}

func strictSwitchRule() v1alpha1.Rule {
	return v1alpha1.Rule{Name: "route", FailurePolicy: v1alpha1.FailOpen, Routing: &v1alpha1.RoutingRule{
		BudgetTiers: &v1alpha1.BudgetTiersRule{
			BudgetRef: "switch", EnforceTargets: true,
			Tiers: []v1alpha1.BudgetTier{{ThresholdPercent: 100, Substitute: map[string]string{"premium": "economy"}}},
		},
	}}
}
