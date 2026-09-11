package bedrockapi

import (
	"net/http/httptest"
	"testing"

	"github.com/inferplane/inferplane/internal/config"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/live"
	"github.com/inferplane/inferplane/internal/principal"
	"github.com/inferplane/inferplane/internal/router"
	"github.com/inferplane/inferplane/providers"
)

// TestCountTokensCrossModelFallbackRBACRecheck is C3's regression on the
// bedrock ingress: same scenario as anthropicapi's twin — the region filter
// drops the requested model's only target and, without the post-routing
// FilterModelAllowed re-check, promotes a cross-model fallback the key was
// never allowed to chain[0], whose upstream then receives the caller's body.
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
	handler := NewCountTokensHandler(router.New(holder), holder)
	handler.SetTeamPolicy(func(team string) (keystore.TeamRecord, bool) {
		return keystore.TeamRecord{AllowedRegions: []string{"eu"}}, true
	})

	req := countTokensReq("claude-a", countTokensBody(t, `{"messages":[{"role":"user","content":"hi"}]}`))
	req = req.WithContext(principal.With(req.Context(),
		keystore.Principal{Team: "t", AllowedModels: []string{"claude-a"}}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := assert200InputTokens(t, rec); got == 777 {
		t.Fatal("response leaked an upstream count for a model the key is not allowed")
	}
	if tcA.called || tcB.called {
		t.Fatalf("no upstream counter may be called (primary region-dropped, fallback not allowed): a=%v b=%v", tcA.called, tcB.called)
	}
}
