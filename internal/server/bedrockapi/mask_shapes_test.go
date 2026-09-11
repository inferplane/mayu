package bedrockapi

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/inferplane/inferplane/internal/config"
	"github.com/inferplane/inferplane/internal/filter"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/principal"
	"github.com/inferplane/inferplane/internal/router"
	"github.com/inferplane/inferplane/providers"
)

// upperMasker is a deterministic filter: replaces "SECRET" and counts it.
type upperMasker struct{}

func (upperMasker) Name() string { return "test-mask" }
func (upperMasker) Mask(text string) (string, int) {
	n := strings.Count(text, "SECRET")
	return strings.ReplaceAll(text, "SECRET", "‹X›"), n
}

// C8: a Nova/Converse-shaped body — {"text": ...} content blocks with NO
// "type" field — must be masked, not silently skipped.
func TestMaskBodyMasksTypelessNovaTextBlocks(t *testing.T) {
	raw := []byte(`{"messages":[{"role":"user","content":[{"text":"my SECRET here"}]}]}`)
	masked, n, err := maskBody(raw, upperMasker{})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("redactions = %d, want 1 (the typeless text block must be walked)", n)
	}
	if !strings.Contains(string(masked), "‹X›") || strings.Contains(string(masked), "SECRET") {
		t.Fatalf("typeless text block not masked: %s", masked)
	}
}

// Anthropic-typed blocks keep their exact prior semantics: text masked,
// tool_use/thinking untouched.
func TestMaskBodyTypedBlocksUnchangedSemantics(t *testing.T) {
	raw := []byte(`{"messages":[{"role":"user","content":[` +
		`{"type":"text","text":"a SECRET"},` +
		`{"type":"tool_use","id":"t1","name":"f","input":{"q":"SECRET stays"}}]}]}`)
	masked, n, err := maskBody(raw, upperMasker{})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("redactions = %d, want 1 (only the text block)", n)
	}
	if !strings.Contains(string(masked), `SECRET stays`) {
		t.Fatalf("tool_use content must stay untouched: %s", masked)
	}
}

// C8: a body with no "messages" array at all (Titan inputText / Llama prompt)
// must ERROR — each caller then takes its own refusal path — never be
// returned unmasked as a silent no-op.
func TestMaskBodyRefusesMessagesLessShapes(t *testing.T) {
	for _, body := range []string{
		`{"inputText":"my SECRET"}`,
		`{"prompt":"my SECRET"}`,
	} {
		if _, _, err := maskBody([]byte(body), upperMasker{}); err == nil {
			t.Errorf("maskBody(%s) = nil error, want a refusal", body)
		}
	}
}

// End-to-end on the count path: a masked team's Titan-shaped CountTokens body
// falls back to the LOCAL estimate (still 200) and the upstream counter is
// never called with the unmaskable body.
func TestCountTokensMaskedTeamUnmaskableShapeNoUpstream(t *testing.T) {
	tc := &tcProvider{}
	provs := map[string]providers.Provider{"p": tc}
	models := map[string]config.ModelConfig{
		"titan-x": {Targets: []config.Target{{Provider: "p", Model: "up"}}},
	}
	h := holderFor(provs, models)
	handler := NewCountTokensHandler(router.New(h), h)
	handler.SetMasking(&filter.Masking{Filter: upperMasker{}, Global: true})

	req := countTokensReq("titan-x", countTokensBody(t, `{"inputText":"my SECRET"}`))
	req = req.WithContext(principal.With(req.Context(), keystore.Principal{Team: "t", AllowedModels: []string{"*"}}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert200InputTokens(t, rec)
	if tc.called {
		t.Fatal("upstream counter must never see an unmaskable body for a masked team")
	}
}
