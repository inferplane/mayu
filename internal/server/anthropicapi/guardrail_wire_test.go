package anthropicapi

import (
	"net/http/httptest"
	"testing"

	"github.com/inferplane/inferplane/internal/keystore"
)

// TestMessagesTeamPolicy_GuardrailOverrideReachesProxyRequest proves the D6
// per-team override, resolved via SetTeamPolicy, is stamped onto the
// ProxyRequest every provider (bedrock in production) reads.
func TestMessagesTeamPolicy_GuardrailOverrideReachesProxyRequest(t *testing.T) {
	rec := &recProvider{}
	h := NewMessagesHandler(recRouter(rec))
	h.SetTeamPolicy(func(team string) (keystore.TeamRecord, bool) {
		if team == "acme" {
			return keystore.TeamRecord{GuardrailID: "gr-team", GuardrailVersion: "2"}, true
		}
		return keystore.TeamRecord{}, false
	})

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, maskedReq("acme", `{"model":"m","messages":[]}`))
	if rr.Code != 200 {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
	if rec.last == nil {
		t.Fatal("provider not called")
	}
	if rec.last.GuardrailID != "gr-team" || rec.last.GuardrailVersion != "2" {
		t.Fatalf("GuardrailID/Version not threaded: %+v", rec.last)
	}
}

// TestMessagesTeamPolicy_NoRecordLeavesGuardrailEmpty proves a team with no
// record (or SetTeamPolicy never called) leaves the provider's own default
// guardrail untouched (empty override fields).
func TestMessagesTeamPolicy_NoRecordLeavesGuardrailEmpty(t *testing.T) {
	rec := &recProvider{}
	h := NewMessagesHandler(recRouter(rec))
	h.SetTeamPolicy(func(team string) (keystore.TeamRecord, bool) { return keystore.TeamRecord{}, false })

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, maskedReq("no-record-team", `{"model":"m","messages":[]}`))
	if rr.Code != 200 {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
	if rec.last.GuardrailID != "" || rec.last.GuardrailVersion != "" {
		t.Fatalf("expected no override, got %+v", rec.last)
	}
}

// TestMessagesTeamPolicy_NilSetterLeavesGuardrailEmpty proves the
// zero-overhead default: SetTeamPolicy never called at all.
func TestMessagesTeamPolicy_NilSetterLeavesGuardrailEmpty(t *testing.T) {
	rec := &recProvider{}
	h := NewMessagesHandler(recRouter(rec))
	// No SetTeamPolicy call.

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, maskedReq("any", `{"model":"m","messages":[]}`))
	if rr.Code != 200 {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
	if rec.last.GuardrailID != "" || rec.last.GuardrailVersion != "" {
		t.Fatalf("expected no override with nil teamPolicy, got %+v", rec.last)
	}
}
