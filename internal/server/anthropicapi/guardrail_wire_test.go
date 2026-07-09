package anthropicapi

import (
	"bytes"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/inferplane/inferplane/internal/audit"
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

// TestMessagesTeamPolicy_GuardrailStampedOnAuditRecord proves the applied
// guardrail (team override) is recorded on the request_completed audit
// record, so the tamper-evident log can later prove which policy governed
// this request even though a team's guardrail_id is mutable.
func TestMessagesTeamPolicy_GuardrailStampedOnAuditRecord(t *testing.T) {
	rec := &recProvider{}
	var buf bytes.Buffer
	w, err := audit.NewWriter("i", filepath.Join(t.TempDir(), "a.wal"), []audit.Sink{audit.NewWriterSink("b", &buf, true)})
	if err != nil {
		t.Fatal(err)
	}
	h := NewMessagesHandlerFull(recRouter(rec), w, nil)
	h.SetTeamPolicy(func(team string) (keystore.TeamRecord, bool) {
		return keystore.TeamRecord{GuardrailID: "gr-team", GuardrailVersion: "2"}, true
	})

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, maskedReq("acme", `{"model":"m","messages":[]}`))
	w.Close()
	if rr.Code != 200 {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
	if !bytes.Contains(buf.Bytes(), []byte(`"guardrail_id":"gr-team"`)) || !bytes.Contains(buf.Bytes(), []byte(`"guardrail_version":"2"`)) {
		t.Fatalf("request_completed record missing stamped guardrail id/version: %s", buf.String())
	}
}

// TestMessagesTeamPolicy_NoGuardrailOmitsAuditFields proves a request with no
// guardrail applied leaves guardrail_id/guardrail_version entirely absent
// from the audit record (nil, never a pointer-to-"").
func TestMessagesTeamPolicy_NoGuardrailOmitsAuditFields(t *testing.T) {
	rec := &recProvider{}
	var buf bytes.Buffer
	w, err := audit.NewWriter("i", filepath.Join(t.TempDir(), "a.wal"), []audit.Sink{audit.NewWriterSink("b", &buf, true)})
	if err != nil {
		t.Fatal(err)
	}
	h := NewMessagesHandlerFull(recRouter(rec), w, nil)
	// No SetTeamPolicy call — no guardrail override.

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, maskedReq("acme", `{"model":"m","messages":[]}`))
	w.Close()
	if rr.Code != 200 {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
	if bytes.Contains(buf.Bytes(), []byte(`"guardrail_id"`)) {
		t.Fatalf("request_completed record must omit guardrail_id when no guardrail applied: %s", buf.String())
	}
}

// TestMessagesTeamPolicy_OkFalseIgnoresRecord proves teamPolicy's ok=false is
// honored (matching count_tokens.go's existing behavior) even when the
// returned TeamRecord is non-zero: neither the guardrail override nor the
// region filter must apply. Today's production teamPolicy never returns this
// shape, but a discarded ok (`teamRec, _ = h.teamPolicy(...)`) would wrongly
// apply both — here demonstrated by the region filter falsely rejecting the
// request (the fake router's target carries no region label, so an
// (incorrectly) enforced allowed_regions=["eu"] fail-closed-blocks it).
func TestMessagesTeamPolicy_OkFalseIgnoresRecord(t *testing.T) {
	rec := &recProvider{}
	h := NewMessagesHandler(recRouter(rec))
	h.SetTeamPolicy(func(team string) (keystore.TeamRecord, bool) {
		return keystore.TeamRecord{GuardrailID: "gr-x", AllowedRegions: []string{"eu"}}, false
	})

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, maskedReq("acme", `{"model":"m","messages":[]}`))
	if rr.Code != 200 {
		t.Fatalf("status %d: %s (ok=false must be honored — no region filter, no guardrail)", rr.Code, rr.Body)
	}
	if rec.last == nil {
		t.Fatal("provider not called")
	}
	if rec.last.GuardrailID != "" {
		t.Fatalf("expected no guardrail override when ok=false, got %+v", rec.last)
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
