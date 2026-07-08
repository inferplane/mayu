package anthropicapi

import (
	"net/http/httptest"
	"testing"

	"github.com/inferplane/inferplane/internal/keystore"
)

// TestMessagesTeamPolicy_RegionRestrictedTeamBlocksUnlabeledTarget proves the
// D7 fail-closed rule: a team with an AllowedRegions policy is denied against
// a target whose provider carries NO region label — an unlabeled provider
// cannot prove residency, so it is never reachable for a restricted team, even
// though it would otherwise be the only (and therefore normally chosen) target.
func TestMessagesTeamPolicy_RegionRestrictedTeamBlocksUnlabeledTarget(t *testing.T) {
	rec := &recProvider{}
	h := NewMessagesHandler(recRouter(rec))
	h.SetTeamPolicy(func(team string) (keystore.TeamRecord, bool) {
		return keystore.TeamRecord{AllowedRegions: []string{"eu"}}, true
	})

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, maskedReq("restricted", `{"model":"m","messages":[]}`))
	if rr.Code != 403 {
		t.Fatalf("status %d, want 403 (region_blocked): %s", rr.Code, rr.Body)
	}
	if rec.last != nil {
		t.Fatal("provider must not be called once its only target is region-filtered out")
	}
}

// TestMessagesTeamPolicy_NoRegionPolicyReachesUnlabeledTarget proves a team
// with NO region restriction (the overwhelming default) is completely
// unaffected by D7 — same passthrough behavior as before this feature.
func TestMessagesTeamPolicy_NoRegionPolicyReachesUnlabeledTarget(t *testing.T) {
	rec := &recProvider{}
	h := NewMessagesHandler(recRouter(rec))
	h.SetTeamPolicy(func(team string) (keystore.TeamRecord, bool) {
		return keystore.TeamRecord{}, true // record exists but sets no region policy
	})

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, maskedReq("unrestricted", `{"model":"m","messages":[]}`))
	if rr.Code != 200 {
		t.Fatalf("status %d, want 200: %s", rr.Code, rr.Body)
	}
	if rec.last == nil {
		t.Fatal("provider not called")
	}
}
