package anthropicapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/inferplane/inferplane/internal/filter"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/principal"
)

func countReq(team, body string) *http.Request {
	req := httptest.NewRequest("POST", "/v1/messages/count_tokens", strings.NewReader(body))
	return req.WithContext(principal.With(req.Context(),
		keystore.Principal{Team: team, AllowedModels: []string{"*"}}))
}

// count_tokens must NEVER return non-200, masked or not (ADR-009 / §mandate).
func TestCountTokensMaskedTeam200(t *testing.T) {
	h := NewCountTokensHandler(testRouter())
	h.SetMasking(&filter.Masking{Filter: stubMasker{}, Global: true})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, countReq("t", `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"PII here"}]}`))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "input_tokens") {
		t.Fatalf("masked count = %d body %s", rec.Code, rec.Body.String())
	}
}

// A masker error must STILL return 200 (local estimate; never 500, never leak).
func TestCountTokensMaskerErrorStill200(t *testing.T) {
	h := NewCountTokensHandler(testRouter())
	h.SetMasking(&filter.Masking{Filter: stubMasker{}, Global: true})
	rec := httptest.NewRecorder()
	// messages is not an array → maskBody errors → local estimate fallback.
	h.ServeHTTP(rec, countReq("t", `{"model":"claude-sonnet-4-6","messages":"not-an-array"}`))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "input_tokens") {
		t.Fatalf("masker-error count = %d body %s, want 200", rec.Code, rec.Body.String())
	}
}
