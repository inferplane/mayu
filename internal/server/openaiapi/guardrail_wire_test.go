package openaiapi

import (
	"net/http/httptest"
	"strings"
	"testing"

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
