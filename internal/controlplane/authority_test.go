package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/inferplane/inferplane/internal/policy"
)

type authorityBackendFake struct{ calls int }

func (f *authorityBackendFake) Sync(context.Context, string, policy.AuthorityRequest) (policy.SyncResponse, error) {
	f.calls++
	return policy.SyncResponse{Generation: "durable", Authority: &policy.AuthorityResponse{Protocol: policy.AuthorityProtocol, ServerTime: time.Now(), Budgets: []policy.AuthorityBudget{}}}, nil
}

func TestDurableSyncCannotFallBackToLegacyGrants(t *testing.T) {
	s, err := NewServer("machine", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	backend := &authorityBackendFake{}
	s.SetBudgetAuthority(backend)
	for _, tc := range []struct {
		body, token string
		status      int
	}{
		{`{"dataplane":"node"}`, "machine", 409},
		{`{"dataplane":"node","authority":{"protocol":"wrong","instance":"boot"}}`, "machine", 409},
		{`{"dataplane":"node","authority":{"protocol":"escrow-v1","instance":"boot"}}`, "machine", 200},
		{`{"dataplane":"node","authority":{"protocol":"escrow-v1","instance":"boot"}}`, "", 401},
	} {
		req := httptest.NewRequest("POST", "/v1alpha1/sync", strings.NewReader(tc.body))
		if tc.token != "" {
			req.Header.Set("Authorization", "Bearer "+tc.token)
		}
		rec := httptest.NewRecorder()
		s.handleSync(rec, req)
		if rec.Code != tc.status {
			t.Fatalf("status=%d want=%d body=%s", rec.Code, tc.status, rec.Body.String())
		}
		if rec.Code == 200 {
			var resp policy.SyncResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || len(resp.Leases) != 0 || resp.Authority == nil {
				t.Fatal("durable response fell back")
			}
		}
	}
	if backend.calls != 1 {
		t.Fatalf("invalid requests reached authority backend: %d", backend.calls)
	}
}

func TestLegacyServerRejectsDurableProtocol(t *testing.T) {
	s, err := NewServer("machine", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.handleSync(rec, httptest.NewRequest(http.MethodPost, "/v1alpha1/sync", strings.NewReader(`{"dataplane":"node","authority":{"protocol":"escrow-v1","instance":"boot"}}`)))
	if rec.Code != 409 {
		t.Fatalf("durable client silently accepted legacy server: %d", rec.Code)
	}
}
