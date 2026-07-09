package anthropicapi

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/inferplane/inferplane/internal/config"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/router"
	"github.com/inferplane/inferplane/providers"
)

// tcProvider embeds recProvider (Name/Models/Complete/Stream, defined in
// mask_wire_test.go) and additionally implements TokenCounter, so a test can
// prove whether the real upstream CountTokens call happened (vs. the local
// estimator fallback).
type tcProvider struct {
	recProvider
	called bool
}

func (tc *tcProvider) CountTokens(ctx context.Context, req *providers.ProxyRequest) (int64, error) {
	tc.called = true
	return 7, nil
}

func tokenCounterRouter(tc *tcProvider) *router.Router {
	provs := map[string]providers.Provider{"p": tc}
	models := map[string]config.ModelConfig{
		"claude-sonnet-4-6": {Targets: []config.Target{{Provider: "p", Model: "claude-sonnet-4-6"}}},
	}
	return router.New(holderFor(provs, models))
}

// TestCountTokens_RegionRestrictedTeamNeverCallsUpstreamCounter proves D7's
// count_tokens guard: a region-restricted team, whose only target carries no
// region label, falls back to the local estimator rather than ever invoking
// the real upstream TokenCounter — count_tokens must not leak content to an
// out-of-region (or, here, unprovable-region) provider.
func TestCountTokens_RegionRestrictedTeamNeverCallsUpstreamCounter(t *testing.T) {
	tc := &tcProvider{}
	h := NewCountTokensHandler(tokenCounterRouter(tc))
	h.SetTeamPolicy(func(team string) (keystore.TeamRecord, bool) {
		return keystore.TeamRecord{AllowedRegions: []string{"eu"}}, true
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, countReq("restricted", `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}`))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "input_tokens") {
		t.Fatalf("count_tokens must always return 200: %d %s", rec.Code, rec.Body.String())
	}
	if tc.called {
		t.Fatal("upstream CountTokens must not be called for a region-filtered-out target")
	}
}

// TestCountTokens_NoRegionPolicyCallsUpstreamCounter proves an unrestricted
// team is unaffected — the real upstream counter is still used when present.
func TestCountTokens_NoRegionPolicyCallsUpstreamCounter(t *testing.T) {
	tc := &tcProvider{}
	h := NewCountTokensHandler(tokenCounterRouter(tc))
	h.SetTeamPolicy(func(team string) (keystore.TeamRecord, bool) { return keystore.TeamRecord{}, true })
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, countReq("unrestricted", `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}`))
	if rec.Code != 200 {
		t.Fatalf("status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if !tc.called {
		t.Fatal("upstream CountTokens should have been called for an unrestricted team")
	}
}
