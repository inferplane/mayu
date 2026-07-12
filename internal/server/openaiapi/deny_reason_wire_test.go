package openaiapi

import (
	"bytes"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/inferplane/inferplane/internal/audit"
	"github.com/inferplane/inferplane/internal/budget"
	"github.com/inferplane/inferplane/internal/governance"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/limiter"
	"github.com/inferplane/inferplane/internal/principal"
)

// Mirrors anthropicapi's deny_reason_wire_test.go: pins the ADR-020 deferred
// item on the OpenAI-compatible ingress too — OutcomeRef.Error now carries a
// closed audit.DenyReason code for allow-list, quota, budget, and region
// denies, not just region_blocked.

func TestDenyReasonAudited_ModelNotAllowed(t *testing.T) {
	var buf bytes.Buffer
	w, err := audit.NewWriter("i", filepath.Join(t.TempDir(), "a.wal"), []audit.Sink{audit.NewWriterSink("b", &buf, true)})
	if err != nil {
		t.Fatal(err)
	}
	h := NewChatHandlerFull(testRouter(), w, nil)
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-x","messages":[{"role":"user","content":"hi"}]}`))
	ctx := principal.With(req.Context(), keystore.Principal{KeyID: "ik", Team: "t", AllowedModels: []string{"qwen-coder"}})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req.WithContext(ctx))
	w.Close()
	if rec.Code != 403 {
		t.Fatalf("want 403, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(buf.String(), `"error":"model_not_allowed"`) {
		t.Fatalf("audit record must carry the model_not_allowed deny code: %s", buf.String())
	}
}

func TestDenyReasonAudited_TeamQuotaExceeded(t *testing.T) {
	var buf bytes.Buffer
	w, err := audit.NewWriter("i", filepath.Join(t.TempDir(), "a.wal"), []audit.Sink{audit.NewWriterSink("b", &buf, true)})
	if err != nil {
		t.Fatal(err)
	}
	lim := limiter.NewMemory()
	teams := map[string]governance.TeamPolicy{"platform-eng": {TokensPerDay: 1000, QuotaExceeded: "block"}}
	gov := governance.NewGovernor(teams, lim, budget.NewMemory(), nil)
	lim.DebitQuota("quota:platform-eng", 1000, 24*time.Hour)

	h := NewChatHandlerFull(testRouter(), w, gov)
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-x","messages":[{"role":"user","content":"hi"}]}`))
	ctx := principal.With(req.Context(), keystore.Principal{KeyID: "ik", Team: "platform-eng", AllowedModels: []string{"*"}})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req.WithContext(ctx))
	w.Close()
	if rec.Code != 429 {
		t.Fatalf("want 429, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(buf.String(), `"error":"team_quota_exceeded"`) {
		t.Fatalf("audit record must carry the team_quota_exceeded deny code: %s", buf.String())
	}
}

func TestDenyReasonAudited_KeyBudgetExceeded(t *testing.T) {
	var buf bytes.Buffer
	w, err := audit.NewWriter("i", filepath.Join(t.TempDir(), "a.wal"), []audit.Sink{audit.NewWriterSink("b", &buf, true)})
	if err != nil {
		t.Fatal(err)
	}
	bud := budget.NewMemory()
	gov := governance.NewGovernor(nil, limiter.NewMemory(), bud, nil) // team ungoverned
	bud.Debit("budget:key:ik_over", 1_500_000, 30*24*time.Hour)       // over the key's 1M cap

	h := NewChatHandlerFull(testRouter(), w, gov)
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-x","messages":[{"role":"user","content":"hi"}]}`))
	ctx := principal.With(req.Context(), keystore.Principal{
		KeyID: "ik_over", Team: "platform-eng", AllowedModels: []string{"*"},
		KeyOptions: keystore.KeyOptions{BudgetUSDMicros: 1_000_000},
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req.WithContext(ctx))
	w.Close()
	if rec.Code != 402 {
		t.Fatalf("want 402, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(buf.String(), `"error":"key_budget_exceeded"`) {
		t.Fatalf("audit record must carry the key_budget_exceeded deny code: %s", buf.String())
	}
}

func TestDenyReasonAudited_RegionBlocked(t *testing.T) {
	var buf bytes.Buffer
	w, err := audit.NewWriter("i", filepath.Join(t.TempDir(), "a.wal"), []audit.Sink{audit.NewWriterSink("b", &buf, true)})
	if err != nil {
		t.Fatal(err)
	}
	rec := &recProvider{}
	h := NewChatHandlerFull(recRouter(rec), w, nil)
	h.SetTeamPolicy(func(team string) (keystore.TeamRecord, bool) {
		return keystore.TeamRecord{AllowedRegions: []string{"eu"}}, true
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[]}`))
	ctx := principal.With(req.Context(), keystore.Principal{KeyID: "ik", Team: "restricted", AllowedModels: []string{"*"}})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req.WithContext(ctx))
	w.Close()
	if rr.Code != 403 {
		t.Fatalf("want 403, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(buf.String(), `"error":"region_blocked"`) {
		t.Fatalf("audit record must carry the region_blocked deny code: %s", buf.String())
	}
}
