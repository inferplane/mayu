package configapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/inferplane/inferplane/internal/config"
)

// TestViewFromNeverLeaksSecrets is the centerpiece: even when the config's
// resolved APIKey is populated (it is, after config.Load), the topology view
// must expose only the ref NAME (env var / file path), never the value.
func TestViewFromNeverLeaksSecrets(t *testing.T) {
	providers := map[string]config.ProviderConfig{
		"anthropic-direct": {
			Type:      "anthropic",
			BaseURL:   "https://api.anthropic.com",
			APIKeyRef: &config.SecretRef{Env: "ANTHROPIC_API_KEY"},
			APIKey:    "sk-ant-SUPER-SECRET-VALUE", // resolved at load — must NOT appear
		},
		"vllm": {
			Type:      "openai_compatible",
			BaseURL:   "http://vllm.internal:8000/v1",
			APIKeyRef: &config.SecretRef{File: "/secrets/vllm-key"},
			APIKey:    "file-secret-value-xyz",
		},
		"ollama": {Type: "openai_compatible", BaseURL: "http://localhost:11434/v1"}, // keyless
	}
	bedrock := config.ProviderConfig{Type: "bedrock", Region: "us-west-2"}
	bedrock.Auth.Mode = "irsa"
	providers["bedrock-us"] = bedrock
	models := map[string]config.ModelConfig{
		"claude-sonnet-4-6": {Targets: []config.Target{
			{Provider: "anthropic-direct", Model: "claude-sonnet-4-6"},
			{Provider: "bedrock-us", Model: "anthropic.claude-sonnet-4-6-v1:0", API: "invoke_model"},
		}},
	}

	v := ViewFrom(providers, models)
	body, _ := json.Marshal(v)
	s := string(body)
	for _, secret := range []string{"sk-ant-SUPER-SECRET-VALUE", "file-secret-value-xyz"} {
		if strings.Contains(s, secret) {
			t.Fatalf("view leaks secret value %q:\n%s", secret, s)
		}
	}
	// Ref NAMES are safe and operationally essential — they MUST be present.
	for _, want := range []string{"ANTHROPIC_API_KEY", "/secrets/vllm-key", "irsa"} {
		if !strings.Contains(s, want) {
			t.Fatalf("view missing safe ref %q:\n%s", want, s)
		}
	}
}

func TestViewAuthStrings(t *testing.T) {
	bedrock := config.ProviderConfig{Type: "bedrock"}
	bedrock.Auth.Mode = "pod_identity"
	providers := map[string]config.ProviderConfig{
		"a": {Type: "anthropic", APIKeyRef: &config.SecretRef{Env: "K"}},
		"f": {Type: "openai_compatible", APIKeyRef: &config.SecretRef{File: "/p"}},
		"n": {Type: "openai_compatible"},
		"b": bedrock,
	}
	v := ViewFrom(providers, nil)
	got := map[string]string{}
	for _, p := range v.Providers {
		got[p.Name] = p.Auth
	}
	want := map[string]string{
		"a": "api key · env:K",
		"f": "api key · file:/p",
		"n": "none (keyless)",
		"b": "IAM · pod_identity",
	}
	for name, w := range want {
		if got[name] != w {
			t.Fatalf("provider %q auth = %q, want %q", name, got[name], w)
		}
	}
}

func TestViewProvidersAndModelsSorted(t *testing.T) {
	// Deterministic order (maps are unordered) so the console renders stably.
	providers := map[string]config.ProviderConfig{"zeta": {Type: "anthropic"}, "alpha": {Type: "bedrock"}}
	models := map[string]config.ModelConfig{"m-z": {}, "m-a": {Targets: []config.Target{{Provider: "alpha", Model: "x"}}}}
	v := ViewFrom(providers, models)
	if v.Providers[0].Name != "alpha" || v.Providers[1].Name != "zeta" {
		t.Fatalf("providers not sorted: %+v", v.Providers)
	}
	if v.Models[0].Name != "m-a" || v.Models[1].Name != "m-z" {
		t.Fatalf("models not sorted: %+v", v.Models)
	}
}

func TestHandlerServesViewGETOnly(t *testing.T) {
	v := ViewFrom(map[string]config.ProviderConfig{"a": {Type: "anthropic", BaseURL: "https://x"}}, nil)
	h := Handler(func() View { return v })

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/admin/config", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "https://x") {
		t.Fatalf("GET = %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type = %q", ct)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/admin/config", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST = %d, want 405 (read-only — stage 2 is a separate ADR)", rec.Code)
	}
}

// TestHandlerReadsLiveView: the handler derives the view per request from the
// provider func, so a hot reload (changing what the func returns) is reflected
// without rebuilding the handler.
func TestHandlerReadsLiveView(t *testing.T) {
	current := ViewFrom(map[string]config.ProviderConfig{"a": {Type: "anthropic", BaseURL: "https://one"}}, nil)
	h := Handler(func() View { return current })

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/admin/config", nil))
	if !strings.Contains(rec.Body.String(), "https://one") {
		t.Fatalf("first: %s", rec.Body.String())
	}
	// Simulate a reload changing the topology.
	current = ViewFrom(map[string]config.ProviderConfig{"a": {Type: "anthropic", BaseURL: "https://two"}}, nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/admin/config", nil))
	if !strings.Contains(rec.Body.String(), "https://two") || strings.Contains(rec.Body.String(), "https://one") {
		t.Fatalf("handler did not reflect the live view: %s", rec.Body.String())
	}
}
