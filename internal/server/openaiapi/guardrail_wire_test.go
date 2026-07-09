package openaiapi

import (
	"bytes"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inferplane/inferplane/internal/audit"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/principal"
)

func TestChatTeamPolicy_GuardrailOverrideReachesProxyRequest(t *testing.T) {
	rec := &recProvider{}
	h := NewChatHandler(recRouter(rec))
	h.SetTeamPolicy(func(team string) (keystore.TeamRecord, bool) {
		if team == "acme" {
			return keystore.TeamRecord{GuardrailID: "gr-team", GuardrailVersion: "2"}, true
		}
		return keystore.TeamRecord{}, false
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[]}`))
	ctx := principal.With(req.Context(), keystore.Principal{KeyID: "ik", Team: "acme", AllowedModels: []string{"*"}})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req.WithContext(ctx))
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

// TestChatTeamPolicy_GuardrailStampedOnAuditRecord proves the applied
// guardrail (team override) is recorded on the request_completed audit
// record for the OpenAI ingress too.
func TestChatTeamPolicy_GuardrailStampedOnAuditRecord(t *testing.T) {
	rec := &recProvider{}
	var buf bytes.Buffer
	w, err := audit.NewWriter("i", filepath.Join(t.TempDir(), "a.wal"), []audit.Sink{audit.NewWriterSink("b", &buf, true)})
	if err != nil {
		t.Fatal(err)
	}
	h := NewChatHandlerFull(recRouter(rec), w, nil)
	h.SetTeamPolicy(func(team string) (keystore.TeamRecord, bool) {
		return keystore.TeamRecord{GuardrailID: "gr-team", GuardrailVersion: "2"}, true
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[]}`))
	ctx := principal.With(req.Context(), keystore.Principal{KeyID: "ik", Team: "acme", AllowedModels: []string{"*"}})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req.WithContext(ctx))
	w.Close()
	if rr.Code != 200 {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
	if !bytes.Contains(buf.Bytes(), []byte(`"guardrail_id":"gr-team"`)) || !bytes.Contains(buf.Bytes(), []byte(`"guardrail_version":"2"`)) {
		t.Fatalf("request_completed record missing stamped guardrail id/version: %s", buf.String())
	}
}

// TestChatTeamPolicy_NoGuardrailOmitsAuditFields proves a request with no
// guardrail applied leaves guardrail_id/guardrail_version entirely absent.
func TestChatTeamPolicy_NoGuardrailOmitsAuditFields(t *testing.T) {
	rec := &recProvider{}
	var buf bytes.Buffer
	w, err := audit.NewWriter("i", filepath.Join(t.TempDir(), "a.wal"), []audit.Sink{audit.NewWriterSink("b", &buf, true)})
	if err != nil {
		t.Fatal(err)
	}
	h := NewChatHandlerFull(recRouter(rec), w, nil)
	// No SetTeamPolicy call.

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[]}`))
	ctx := principal.With(req.Context(), keystore.Principal{KeyID: "ik", Team: "acme", AllowedModels: []string{"*"}})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req.WithContext(ctx))
	w.Close()
	if rr.Code != 200 {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
	if bytes.Contains(buf.Bytes(), []byte(`"guardrail_id"`)) {
		t.Fatalf("request_completed record must omit guardrail_id when no guardrail applied: %s", buf.String())
	}
}

func TestChatTeamPolicy_NilSetterLeavesGuardrailEmpty(t *testing.T) {
	rec := &recProvider{}
	h := NewChatHandler(recRouter(rec))
	// No SetTeamPolicy call.

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[]}`))
	ctx := principal.With(req.Context(), keystore.Principal{KeyID: "ik", Team: "acme", AllowedModels: []string{"*"}})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req.WithContext(ctx))
	if rr.Code != 200 {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
	if rec.last.GuardrailID != "" || rec.last.GuardrailVersion != "" {
		t.Fatalf("expected no override with nil teamPolicy, got %+v", rec.last)
	}
}
