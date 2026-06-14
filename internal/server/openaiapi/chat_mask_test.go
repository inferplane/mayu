package openaiapi

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/inferplane/inferplane/internal/filter"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/principal"
)

type stubMasker struct{}

func (stubMasker) Name() string                { return "stub" }
func (stubMasker) Mask(t string) (string, int) { return t, 0 }

// TestChatRejectsMaskedTeam pins the ADR-009 round-2 CRITICAL: a masked team
// must NOT bypass PII masking via the OpenAI ingress — it is rejected (400),
// never forwarded, until OpenAI-ingress masking ships.
func TestChatRejectsMaskedTeam(t *testing.T) {
	h := NewChatHandler(testRouter())
	h.SetMasking(&filter.Masking{Filter: stubMasker{}, Teams: map[string]bool{"secure": true}})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-x","messages":[{"role":"user","content":"hi"}]}`))
	ctx := principal.With(req.Context(), keystore.Principal{Team: "secure", AllowedModels: []string{"*"}})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req.WithContext(ctx))
	if rec.Code != 400 {
		t.Fatalf("masked team on OpenAI ingress = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "/v1/messages") {
		t.Fatalf("rejection should point to /v1/messages: %s", rec.Body.String())
	}
}

// An UNMASKED team is unaffected on the OpenAI ingress.
func TestChatUnmaskedTeamUnaffected(t *testing.T) {
	h := NewChatHandler(testRouter())
	h.SetMasking(&filter.Masking{Filter: stubMasker{}, Teams: map[string]bool{"secure": true}})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-x","messages":[{"role":"user","content":"hi"}]}`))
	ctx := principal.With(req.Context(), keystore.Principal{Team: "other", AllowedModels: []string{"*"}})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req.WithContext(ctx))
	if rec.Code != 200 {
		t.Fatalf("unmasked team = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}
