package anthropicapi

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/inferplane/inferplane/internal/config"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/live"
	"github.com/inferplane/inferplane/internal/principal"
	"github.com/inferplane/inferplane/internal/router"
	"github.com/inferplane/inferplane/providers"
)

// TestCountTokensCrossModelFallbackRBACRecheck is C3's regression: ResolveChain
// appends a cross-model fallback's targets AFTER the pre-routing Allows check,
// and the region filter can then drop every target of the requested model and
// promote the fallback to chain[0] — whose upstream previously received the
// caller's body even though the key was never allowed that model. The
// post-routing FilterModelAllowed re-check (mirroring messages.go) must remove
// the fallback first, so the handler falls back to the LOCAL estimate (still
// 200) and no upstream counter is ever called.
func TestCountTokensCrossModelFallbackRBACRecheck(t *testing.T) {
	tcA := &tcProvider{}
	tcB := &tcProvider{}
	providers.Register("c3a-"+t.Name(), func(providers.Config) (providers.Provider, error) { return tcA, nil })
	providers.Register("c3b-"+t.Name(), func(providers.Config) (providers.Provider, error) { return tcB, nil })
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"p-a": {Type: "c3a-" + t.Name(), Region: "us"},
			"p-b": {Type: "c3b-" + t.Name(), Region: "eu"},
		},
		Models: map[string]config.ModelConfig{
			"claude-a": {Targets: []config.Target{{Provider: "p-a", Model: "up-a"}}},
			"claude-b": {Targets: []config.Target{{Provider: "p-b", Model: "up-b"}}},
		},
		ModelFallbacks: map[string]string{"claude-a": "claude-b"},
	}
	st, _, err := live.BuildState(cfg)
	if err != nil {
		t.Fatal(err)
	}
	holder := &live.Holder{}
	holder.Swap(st)
	h := NewCountTokensHandler(router.New(holder))
	// The team is region-locked to eu: claude-a's only target (us) is dropped
	// by the region filter, which — without the C3 re-check — promoted
	// claude-b's eu target to chain[0].
	h.SetTeamPolicy(func(team string) (keystore.TeamRecord, bool) {
		return keystore.TeamRecord{AllowedRegions: []string{"eu"}}, true
	})

	req := httptest.NewRequest("POST", "/v1/messages/count_tokens",
		strings.NewReader(`{"model":"claude-a","messages":[{"role":"user","content":"hi"}]}`))
	req = req.WithContext(principal.With(req.Context(),
		keystore.Principal{Team: "t", AllowedModels: []string{"claude-a"}}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "input_tokens") {
		t.Fatalf("count_tokens must stay 200 with an estimate: %d %s", rec.Code, rec.Body.String())
	}
	if tcA.called || tcB.called {
		t.Fatalf("no upstream counter may be called (primary region-dropped, fallback not allowed): a=%v b=%v", tcA.called, tcB.called)
	}
}
