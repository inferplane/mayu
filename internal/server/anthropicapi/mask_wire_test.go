package anthropicapi

import (
	"context"
	"encoding/json"
	"iter"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/inferplane/inferplane/internal/config"
	"github.com/inferplane/inferplane/internal/filter"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/principal"
	"github.com/inferplane/inferplane/internal/router"
	"github.com/inferplane/inferplane/pkg/schema"
	"github.com/inferplane/inferplane/providers"
)

// recProvider records the last ProxyRequest it received, so a test can assert
// WHAT was forwarded upstream (masked or not).
type recProvider struct {
	last *providers.ProxyRequest
}

func (p *recProvider) Name() string               { return "rec" }
func (p *recProvider) Models() []schema.ModelInfo { return nil }
func (p *recProvider) Complete(_ context.Context, req *providers.ProxyRequest) (*providers.ProxyResponse, error) {
	p.last = req
	return &providers.ProxyResponse{StatusCode: 200, RawBody: []byte(`{"id":"x","type":"message","role":"assistant","model":"m","content":[]}`)}, nil
}
func (p *recProvider) Stream(context.Context, *providers.ProxyRequest) (iter.Seq2[*providers.StreamEvent, error], error) {
	return nil, nil
}

func recRouter(p providers.Provider) *router.Router {
	provs := map[string]providers.Provider{"rec": p}
	models := map[string]config.ModelConfig{"m": {Targets: []config.Target{{Provider: "rec", Model: "up"}}}}
	return router.New(holderFor(provs, models))
}

// stubMasker is defined in mask_test.go (masks "PII" → "X").

func maskedReq(team, body string) *http.Request {
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	return req.WithContext(principal.With(req.Context(),
		keystore.Principal{Team: team, AllowedModels: []string{"*"}}))
}

func TestMessagesMasksUpstreamBody(t *testing.T) {
	rec := &recProvider{}
	h := NewMessagesHandler(recRouter(rec))
	h.SetMasking(&filter.Masking{Filter: stubMasker{}, Global: true})

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, maskedReq("any", `{"model":"m","messages":[{"role":"user","content":"call PII now"}]}`))
	if rr.Code != 200 {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
	if rec.last == nil {
		t.Fatal("provider not called")
	}
	// BOTH the forwarded RawBody AND the parsed request must be masked (the
	// openai_compatible provider converts from Parsed — gate C1).
	if strings.Contains(string(rec.last.RawBody), "PII") {
		t.Fatalf("RawBody forwarded unmasked: %s", rec.last.RawBody)
	}
	if !strings.Contains(string(rec.last.RawBody), "X") {
		t.Fatalf("RawBody not masked: %s", rec.last.RawBody)
	}
	// Inspect Parsed via its JSON form (the openai_compatible provider converts
	// from Parsed — gate C1): it must be masked too.
	pj, _ := json.Marshal(rec.last.Parsed)
	if strings.Contains(string(pj), "PII") {
		t.Fatalf("Parsed forwarded unmasked: %s", pj)
	}
	if !strings.Contains(string(pj), "X") {
		t.Fatalf("Parsed not masked (openai_compatible path would leak): %s", pj)
	}
}

func TestMessagesUnmaskedTeamVerbatim(t *testing.T) {
	rec := &recProvider{}
	h := NewMessagesHandler(recRouter(rec))
	// masking scoped to team "secure" only
	h.SetMasking(&filter.Masking{Filter: stubMasker{}, Teams: map[string]bool{"secure": true}})

	body := `{"model":"m","messages":[{"role":"user","content":"PII stays"}]}`
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, maskedReq("other-team", body))
	if rr.Code != 200 {
		t.Fatalf("status %d", rr.Code)
	}
	// unmasked team → body forwarded BYTE-FOR-BYTE (verbatim fast path)
	if string(rec.last.RawBody) != body {
		t.Fatalf("unmasked team body not verbatim:\n got %s\nwant %s", rec.last.RawBody, body)
	}
}

// failMasker errors via a body the masker cannot handle — but stubMasker can't
// fail, so we force the fail-closed path with an invalid-JSON body (maskBody
// returns an error). The handler must reject 400 and NOT call the provider.
func TestMessagesMaskerErrorFailsClosed(t *testing.T) {
	rec := &recProvider{}
	h := NewMessagesHandler(recRouter(rec))
	h.SetMasking(&filter.Masking{Filter: stubMasker{}, Global: true})

	// Valid enough to parse for routing (has model) but maskBody re-unmarshal of
	// messages fails: messages is not an array.
	body := `{"model":"m","messages":"not-an-array"}`
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, maskedReq("any", body))
	if rr.Code != 400 {
		t.Fatalf("masker error status = %d, want 400 (fail closed): %s", rr.Code, rr.Body)
	}
	if rec.last != nil {
		t.Fatal("fail-closed must NOT forward to the provider")
	}
}
