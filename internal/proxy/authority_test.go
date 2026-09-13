package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/inferplane/inferplane/api/v1alpha1"
	"github.com/inferplane/inferplane/internal/authority/local"
	"github.com/inferplane/inferplane/internal/policy"
)

type authorityClientFake struct {
	applied int
	wake    chan struct{}
}

func TestDurableSyncCannotPublishSnapshotRejectedByJournalClock(t *testing.T) {
	journal, err := local.Open(filepath.Join(t.TempDir(), "authority.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	at := time.Now().UTC()
	response := policy.SyncResponse{Generation: policy.GenerationOf(nil)}
	response.Authority = &policy.AuthorityResponse{Protocol: policy.AuthorityProtocol, ServerTime: at,
		Generation: response.Generation, Policies: []v1alpha1.GovernancePolicy{}, Budgets: []policy.AuthorityBudget{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer srv.Close()
	s := &Syncer{URL: srv.URL, Dataplane: "node", Store: policy.NewEmptyStore(), Authority: journal}
	if _, err := s.syncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	var doc v1alpha1.GovernancePolicy
	if err := json.Unmarshal([]byte(`{"apiVersion":"inferplane.dev/v1alpha1","kind":"GovernancePolicy","metadata":{"name":"new-cap","generation":1},"spec":{"subject":{"team":"team"},"rules":[{"name":"cap","failurePolicy":"FailClosed","budget":{"limitMilliUSD":100,"hardCap":true}}]}}`), &doc); err != nil {
		t.Fatal(err)
	}
	response.Generation = policy.GenerationOf([]v1alpha1.GovernancePolicy{doc})
	response.Authority.Generation = response.Generation
	response.Authority.Policies = []v1alpha1.GovernancePolicy{doc}
	response.Authority.ServerTime = at.Add(-time.Second)
	response.Authority.Budgets, err = policy.AuthorityBudgets(response.Authority.Policies, response.Authority.ServerTime)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.syncOnce(context.Background()); err == nil {
		t.Fatal("stale journal snapshot reported successful policy publication")
	}
	if ready, _ := s.GovernanceReady(0); ready {
		t.Fatal("inconsistent financial/policy snapshots are ready")
	}
	if s.generation == response.Generation {
		t.Fatal("policy generation published despite discarded financial snapshot")
	}
}

func (f *authorityClientFake) Request(context.Context) (policy.AuthorityRequest, error) {
	return policy.AuthorityRequest{Protocol: policy.AuthorityProtocol, Instance: "boot"}, nil
}
func (f *authorityClientFake) Apply(context.Context, policy.AuthorityResponse, time.Duration) error {
	f.applied++
	return nil
}
func (f *authorityClientFake) Wake() <-chan struct{} { return f.wake }

func TestDurableSyncRejectsMissingAuthorityAndClearsExplicitEmptyPolicies(t *testing.T) {
	var valid atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request policy.SyncRequest
		if json.NewDecoder(r.Body).Decode(&request) != nil || request.Authority == nil || len(request.Reports) != 0 {
			t.Error("wrong durable request")
		}
		response := policy.SyncResponse{Generation: policy.GenerationOf(nil)}
		if valid.Load() {
			response.Authority = &policy.AuthorityResponse{Protocol: policy.AuthorityProtocol, ServerTime: time.Now(), Generation: response.Generation, Policies: []v1alpha1.GovernancePolicy{}, Budgets: []policy.AuthorityBudget{}}
		}
		json.NewEncoder(w).Encode(response)
	}))
	defer srv.Close()
	client := &authorityClientFake{wake: make(chan struct{}, 1)}
	s := &Syncer{URL: srv.URL, Dataplane: "node", Store: policy.NewEmptyStore(), Authority: client}
	if _, err := s.syncOnce(context.Background()); err == nil {
		t.Fatal("missing protocol accepted")
	}
	if ready, _ := s.GovernanceReady(0); ready {
		t.Fatal("invalid response became ready")
	}
	valid.Store(true)
	if _, err := s.syncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if client.applied != 1 {
		t.Fatal("authority state not applied")
	}
	if ready, _ := s.GovernanceReady(0); !ready {
		t.Fatal("valid complete bundle not ready")
	}
}
