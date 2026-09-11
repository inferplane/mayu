package router

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/inferplane/inferplane/api/v1alpha1"
	"github.com/inferplane/inferplane/internal/live"
	"github.com/inferplane/inferplane/internal/policy"
)

func affinitySetup(t *testing.T) (*Router, RequestRoutingInput, *policy.Policy, *time.Time) {
	t.Helper()
	r, in := routingSetup(t, routingConfig())
	p := contextPolicy("context", v1alpha1.Enforce, "private")
	p.Rules[0].Routing.Context.Stability = &policy.ContextStability{MinHold: 5 * time.Minute, MinRequests: 3, SessionTTL: 30 * time.Minute}
	installRoutingPolicies(r, p)
	now := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	r.SetAffinityClock(func() time.Time { return now })
	in.SessionHint = "sensitive-session-hint"
	return r, in, p, &now
}

func recordAffinity(t *testing.T, r *Router, result RequestRoutingResult, index int) {
	t.Helper()
	if result.AffinityToken == nil {
		t.Fatal("eligible selection has no affinity success token")
	}
	r.RecordAffinitySuccess(result.AffinityToken, result.Chain[index])
}

func TestAffinityPinsActualSuccessfulProviderAndBothHoldLimits(t *testing.T) {
	r, in, _, now := affinitySetup(t)
	first, err := r.RouteRequest(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	// private2 is the actual fallback winner, not the planned private provider.
	recordAffinity(t, r, first, 2)
	in.RawBody = []byte(`{"messages":[{"role":"user","content":"security"}]}`)
	second, _ := r.RouteRequest(context.Background(), in)
	if second.Model != "private" || second.Chain[0].ProviderName != "private2" {
		t.Fatalf("lost actual provider pin: %+v", second)
	}
	recordAffinity(t, r, second, 0)
	*now = now.Add(6 * time.Minute)
	third, _ := r.RouteRequest(context.Background(), in)
	if third.Model != "private" {
		t.Fatal("elapsed time bypassed minimum successful request count")
	}
	recordAffinity(t, r, third, 0)
	fourth, _ := r.RouteRequest(context.Background(), in)
	if fourth.Model != "premium" {
		t.Fatal("pin did not release after both hold limits")
	}
}

func TestAffinityCountDoesNotRefreshOrCreateAndTTLExpires(t *testing.T) {
	r, in, _, now := affinitySetup(t)
	first, _ := r.RouteRequest(context.Background(), in)
	recordAffinity(t, r, first, 0)
	*now = now.Add(29 * time.Minute)
	in.CountOnly = true
	count, _ := r.RouteRequest(context.Background(), in)
	token := reflect.ValueOf(count).FieldByName("AffinityToken")
	if token.IsValid() && !token.IsNil() {
		t.Fatal("count request can refresh affinity")
	}
	*now = now.Add(2 * time.Minute)
	in.CountOnly = false
	in.RawBody = []byte(`{"messages":[{"role":"user","content":"security"}]}`)
	got, _ := r.RouteRequest(context.Background(), in)
	if got.Model != "premium" {
		t.Fatal("count request kept expired pin warm")
	}
}

func TestAffinityScopeAndPrivacyBudgetOverride(t *testing.T) {
	for _, change := range []string{"key", "team", "owner", "policy", "privacy", "budget", "region", "topology"} {
		t.Run(change, func(t *testing.T) {
			r, in, p, _ := affinitySetup(t)
			first, _ := r.RouteRequest(context.Background(), in)
			recordAffinity(t, r, first, 0)
			in.RawBody = []byte(`{"messages":[{"role":"user","content":"security"}]}`)
			want := "premium"
			switch change {
			case "key":
				in.Principal.KeyID = "another-key"
			case "team":
				in.Principal.Team = "another-team"
			case "owner":
				in.Principal.Owner = "another-user"
			case "policy":
				p.Generation++
			case "privacy":
				protectedInput(&in)
				installRoutingPolicies(r, p, privatePolicy("private", "economy"))
				want = "economy"
			case "budget":
				setBudgetConstraint(t, r, map[string]string{"premium": "economy"})
				want = "economy"
			case "region":
				in.AllowedRegions = []string{"us"}
			case "topology":
				cfg := routingConfig()
				delete(cfg.Models, "private")
				delete(cfg.Pricing.Overrides["public"], "private-public")
				delete(cfg.Pricing.Overrides["private"], "private-upstream")
				delete(cfg.Pricing.Overrides["private2"], "private-retry")
				st, _, err := live.BuildState(cfg)
				if err != nil {
					t.Fatal(err)
				}
				in.State = st
			}
			got, err := r.RouteRequest(context.Background(), in)
			if err != nil || got.Model != want {
				t.Fatalf("pin survived %s: %+v %v", change, got, err)
			}
		})
	}
}

func TestAffinityPrefixAndEvidence(t *testing.T) {
	r, in, _, _ := affinitySetup(t)
	in.SessionHint = ""
	first, _ := r.RouteRequest(context.Background(), in)
	recordAffinity(t, r, first, 0)
	in.RawBody = []byte(`{"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"hello"},{"role":"user","content":"security"}],"max_tokens":32}`)
	got, err := r.RouteRequest(context.Background(), in)
	if err != nil || got.Model != "private" {
		t.Fatalf("first-user prefix did not survive another turn: %+v %v", got, err)
	}
	b, _ := json.Marshal(got)
	if strings.Contains(string(b), "hello") || strings.Contains(string(b), "session-hint") || strings.Contains(string(b), "AffinityToken") {
		t.Fatalf("affinity identity or body escaped result serialization: %s", b)
	}
}

func TestAffinityConcurrentSuccess(t *testing.T) {
	r, in, _, _ := affinitySetup(t)
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := r.RouteRequest(context.Background(), in)
			if err != nil {
				t.Error(err)
				return
			}
			recordAffinity(t, r, got, 0)
		}()
	}
	wg.Wait()
	got, err := r.RouteRequest(context.Background(), in)
	if err != nil || got.Model != "private" {
		t.Fatalf("concurrent affinity corrupted routing: %+v %v", got, err)
	}
}

func TestAffinityStorageBoundReplayAndColdStart(t *testing.T) {
	r, in, _, now := affinitySetup(t)
	first, _ := r.RouteRequest(context.Background(), in)
	recordAffinity(t, r, first, 0)
	// Repeated success callbacks must not turn one request into many.
	for range 10 {
		r.RecordAffinitySuccess(first.AffinityToken, first.Chain[0])
	}
	r.affinity.mu.Lock()
	if entry := r.affinity.entries[first.AffinityToken.key]; entry.requests != 1 {
		t.Errorf("token replay charged %d requests", entry.requests)
	}
	r.affinity.mu.Unlock()
	// Fill with distinct session hashes, never retained raw session hints.
	for i := range maxAffinityEntries + 2 {
		in.SessionHint = fmt.Sprintf("hint-%d", i)
		got, err := r.RouteRequest(context.Background(), in)
		if err != nil {
			t.Fatal(err)
		}
		recordAffinity(t, r, got, 0)
	}
	r.affinity.mu.Lock()
	if len(r.affinity.entries) != maxAffinityEntries {
		t.Errorf("unbounded affinity store: %d", len(r.affinity.entries))
	}
	r.affinity.mu.Unlock()
	*now = now.Add(time.Hour)
	in.SessionHint = "after-expiry"
	got, err := r.RouteRequest(context.Background(), in)
	if err != nil {
		t.Fatal("expired full store blocked routing")
	}
	recordAffinity(t, r, got, 0)
	r.affinity.mu.Lock()
	if len(r.affinity.entries) != 1 {
		t.Errorf("expired entries not reclaimed: %d", len(r.affinity.entries))
	}
	r.affinity.mu.Unlock()
	restarted, fresh, _, _ := affinitySetup(t)
	fresh.RawBody = []byte(`{"messages":[{"role":"user","content":"security"}]}`)
	cold, err := restarted.RouteRequest(context.Background(), fresh)
	if err != nil || cold.Model != "premium" {
		t.Fatal("cold start did not safely reselect")
	}
}

func TestAffinityRevalidatesAllTargetMetadata(t *testing.T) {
	for _, change := range []string{"rbac", "window", "price", "identity", "capability", "shadow"} {
		t.Run(change, func(t *testing.T) {
			r, in, p, _ := affinitySetup(t)
			first, _ := r.RouteRequest(context.Background(), in)
			recordAffinity(t, r, first, 0)
			in.RawBody = []byte(`{"messages":[{"role":"user","content":"security"}]}`)
			cfg := routingConfig()
			switch change {
			case "rbac":
				in.Principal.AllowedModels = []string{"premium"}
			case "window":
				m := cfg.Models["private"]
				m.ContextWindow = 1
				cfg.Models["private"] = m
			case "price":
				delete(cfg.Pricing.Overrides["private"], "private-upstream")
			case "identity":
				provider := cfg.Providers["private"]
				provider.BaseURL = "https://changed.invalid"
				cfg.Providers["private"] = provider
			case "capability":
				in.RawBody = []byte(`{"messages":[{"role":"user","content":"security"}],"tools":[{"name":"f","input_schema":{"type":"object"}}]}`)
			case "shadow":
				p.Rules[0].Routing.Context.Mode = v1alpha1.Shadow
			}
			st, _, err := live.BuildState(cfg)
			if err != nil {
				t.Fatal(err)
			}
			in.State = st
			got, err := r.RouteRequest(context.Background(), in)
			if err != nil || got.Model != "premium" || got.Decision.Reason == "context_affinity" {
				t.Fatalf("pin ignored %s: %+v %v", change, got, err)
			}
		})
	}
}

func TestAffinityUnavailablePreferenceStillPinsActualSuccess(t *testing.T) {
	r, in, _, _ := affinitySetup(t)
	cfg := routingConfig()
	m := cfg.Models["private"]
	m.ContextWindow = 1
	cfg.Models["private"] = m
	st, _, err := live.BuildState(cfg)
	if err != nil {
		t.Fatal(err)
	}
	in.State = st
	got, err := r.RouteRequest(context.Background(), in)
	if err != nil || got.Model != "premium" {
		t.Fatalf("optional unavailable preference did not retain original: %+v %v", got, err)
	}
	recordAffinity(t, r, got, 0)
	st, _, err = live.BuildState(routingConfig())
	if err != nil {
		t.Fatal(err)
	}
	in.State = st
	again, err := r.RouteRequest(context.Background(), in)
	if err != nil || again.Model != "premium" || again.Decision.Reason != "context_affinity" {
		t.Fatalf("actual original success was not held: %+v %v", again, err)
	}
}
