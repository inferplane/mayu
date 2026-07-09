package openaiapi

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/principal"
)

// TestChatTeamPolicy_RegionRestrictedTeamBlocksUnlabeledTarget mirrors
// anthropicapi's region_wire_test.go: a region-restricted team is denied
// against an unlabeled-provider target (D7, ADR-020 fail-closed).
func TestChatTeamPolicy_RegionRestrictedTeamBlocksUnlabeledTarget(t *testing.T) {
	rec := &recProvider{}
	h := NewChatHandler(recRouter(rec))
	h.SetTeamPolicy(func(team string) (keystore.TeamRecord, bool) {
		return keystore.TeamRecord{AllowedRegions: []string{"eu"}}, true
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[]}`))
	ctx := principal.With(req.Context(), keystore.Principal{KeyID: "ik", Team: "restricted", AllowedModels: []string{"*"}})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req.WithContext(ctx))
	if rr.Code != 403 {
		t.Fatalf("status %d, want 403 (region_blocked): %s", rr.Code, rr.Body)
	}
	if rec.last != nil {
		t.Fatal("provider must not be called once its only target is region-filtered out")
	}
}

// TestChatTeamPolicy_NoRegionPolicyReachesUnlabeledTarget proves a team with
// no region restriction is unaffected by D7.
func TestChatTeamPolicy_NoRegionPolicyReachesUnlabeledTarget(t *testing.T) {
	rec := &recProvider{}
	h := NewChatHandler(recRouter(rec))
	h.SetTeamPolicy(func(team string) (keystore.TeamRecord, bool) {
		return keystore.TeamRecord{}, true
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[]}`))
	ctx := principal.With(req.Context(), keystore.Principal{KeyID: "ik", Team: "unrestricted", AllowedModels: []string{"*"}})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req.WithContext(ctx))
	if rr.Code != 200 {
		t.Fatalf("status %d, want 200: %s", rr.Code, rr.Body)
	}
	if rec.last == nil {
		t.Fatal("provider not called")
	}
}
