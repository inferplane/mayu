package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/inferplane/inferplane/api/v1alpha1"
	"github.com/inferplane/inferplane/internal/budget"
	"github.com/inferplane/inferplane/internal/governance"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/limiter"
	"github.com/inferplane/inferplane/internal/policy"
	"github.com/inferplane/inferplane/internal/server/openaiapi"
	"github.com/inferplane/inferplane/internal/server/routingtest"
	"github.com/inferplane/inferplane/internal/tier"
	"github.com/inferplane/inferplane/pkg/schema"
)

func TestInstalledRoutingSurvivesControlPlaneFailureWithinValidAuthority(t *testing.T) {
	doc := v1alpha1.GovernancePolicy{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: v1alpha1.KindGovernancePolicy},
		Metadata: v1alpha1.ObjectMeta{Name: "offline", Generation: 1},
		Spec: v1alpha1.GovernancePolicySpec{Subject: v1alpha1.Subject{Team: "team"}, Rules: []v1alpha1.Rule{
			{Name: "switch", FailurePolicy: v1alpha1.FailOpen, Budget: &v1alpha1.BudgetRule{LimitMilliUSD: 100}},
			{Name: "total", FailurePolicy: v1alpha1.FailClosed, Budget: &v1alpha1.BudgetRule{LimitMilliUSD: 1000, HardCap: true}},
			{Name: "cutover", FailurePolicy: v1alpha1.FailOpen, Routing: &v1alpha1.RoutingRule{BudgetTiers: &v1alpha1.BudgetTiersRule{
				BudgetRef: "switch", EnforceTargets: true, Tiers: []v1alpha1.BudgetTier{{ThresholdPercent: 100, Substitute: map[string]string{"premium": "economy"}}},
			}}},
			{Name: "private", FailurePolicy: v1alpha1.FailClosed, SensitiveData: &v1alpha1.SensitiveDataRule{
				OnDetected: v1alpha1.InternalOnly, OnUninspectable: v1alpha1.Block, InternalModels: []string{"economy"},
			}},
		}},
	}
	grant := policy.LeaseGrant{Policy: "offline", Rule: "total", Team: "team", AllowanceMicroUSD: 10000, HardCap: true, ExpiresAt: time.Now().Add(time.Minute)}
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(policy.SyncResponse{
			Generation: "one", Policies: []v1alpha1.GovernancePolicy{doc}, Leases: []policy.LeaseGrant{grant},
			ActiveTiers: []policy.ActiveTier{{Policy: "offline", Rule: "cutover", BudgetRef: "switch", Team: "team",
				ThresholdPercent: 100, EnforceTargets: true, Substitute: map[string]string{"premium": "economy"}}},
		})
	}))
	defer cp.Close()
	store, leases, tiers := policy.NewEmptyStore(), NewLeaseTable(), tier.NewTable()
	syncer := &Syncer{URL: cp.URL, Dataplane: "local-node", Store: store, Leases: leases, Tiers: tiers}
	if _, err := syncer.syncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	cp.Close()
	if _, err := syncer.syncOnce(context.Background()); err == nil {
		t.Fatal("control plane failure was not exercised")
	}
	if ready, reason := syncer.GovernanceReady(time.Minute); !ready {
		t.Fatalf("fresh installed policy became unusable: %s", reason)
	}
	f := routingtest.New(t, nil)
	f.Router.SetRoutingPolicyLookup(store.MatchingRoutingPolicies)
	f.Router.SetTierGate(func(p keystore.Principal) map[string]string { return tiers.Get(p.Team) })
	f.Router.SetBudgetConstraintGate(func(p keystore.Principal) map[string]string { return tiers.Constraints(p.Team) })
	in, out := int64(2), int64(1)
	f.Private.Usage = &schema.Usage{InputTokens: &in, OutputTokens: &out}
	gov := governance.NewGovernor(map[string]governance.TeamPolicy{
		"team": {BudgetMicrosPerMonth: grant.AllowanceMicroUSD, BudgetExceeded: "block"},
	}, limiter.NewMemory(), budget.NewMemory(), nil)
	gov.SetLeaseGate(leases.Blocked)
	handler := openaiapi.NewChatHandlerFull(f.Router, nil, gov)
	request := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, routingtest.Request("/v1/chat/completions",
			`{"model":"premium","messages":[{"role":"user","content":"alice@example.test"}]}`))
		return rec
	}
	if rec := request(); rec.Code != 200 || len(f.Private.Calls) != 1 || len(f.Public.Calls) != 0 {
		t.Fatalf("installed private/cost route lost during outage: status=%d private=%d public=%d", rec.Code, len(f.Private.Calls), len(f.Public.Calls))
	}
	grant.ExpiresAt = time.Now().Add(-time.Second)
	leases.set([]policy.LeaseGrant{grant})
	if rec := request(); rec.Code != 402 || len(f.Private.Calls) != 1 || len(f.Public.Calls) != 0 {
		t.Fatalf("expired hard authority was bypassed by cheap route: status=%d private=%d public=%d", rec.Code, len(f.Private.Calls), len(f.Public.Calls))
	}
}
