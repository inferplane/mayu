package live

import (
	"encoding/json"
	"testing"

	"github.com/inferplane/inferplane/internal/config"
)

func TestRoutingMetadataFrozen(t *testing.T) {
	var models map[string]config.ModelConfig
	_ = json.Unmarshal([]byte(`{"m":{"context_window":1234,"capabilities":["tools"],"aliases":["a"]}}`), &models)
	st := NewState(nil, models, nil, nil)
	// JSON tests the externally consumed metadata without requiring new fields to compile at RED.
	assert := func() {
		t.Helper()
		mc, _ := st.Route("m")
		b, _ := json.Marshal(mc)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		caps, _ := m["capabilities"].([]any)
		if len(caps) != 1 || caps[0] != "tools" || m["context_window"] != float64(1234) {
			t.Fatalf("frozen metadata lost: %s", b)
		}
	}
	assert()
}

func TestRoutingMetadataBoundaryAndIsolation(t *testing.T) {
	models := map[string]config.ModelConfig{"m": {ContextWindow: 1234, Capabilities: []string{"tools"}}}
	st := NewState(nil, models, nil, nil)
	models["m"].Capabilities[0] = "vision"
	st.Models()["m"].Capabilities[0] = "reasoning"
	mc, _ := st.Route("m")
	mc.Capabilities[0] = "structured_output"
	mc, _ = st.Route("m")
	if mc.Capabilities[0] != "tools" {
		t.Fatal("caller mutated frozen capabilities")
	}
	st.providerConfigs = map[string]config.ProviderConfig{"internal": {DataBoundary: "internal"}, "external": {DataBoundary: "external"}, "empty": {}, "bad": {DataBoundary: "local"}}
	for name, want := range map[string]string{"internal": "internal", "external": "external", "empty": "unknown", "bad": "unknown", "missing": "unknown"} {
		if got := st.DataBoundary(name); got != want {
			t.Errorf("%s boundary=%s want=%s", name, got, want)
		}
	}
}

func TestRoutingMetadataBuildStateCopies(t *testing.T) {
	cfg := sampleConfig()
	pc := cfg.Providers["anthropic-direct"]
	pc.DataBoundary = "internal"
	pc.APIKeyRef = &config.SecretRef{Env: "UPSTREAM_KEY"}
	cfg.Providers["anthropic-direct"] = pc
	mc := cfg.Models["claude"]
	mc.Capabilities = []string{"tools"}
	mc.ContextWindow = 8192
	cfg.Models["claude"] = mc
	st, _, err := BuildState(cfg)
	if err != nil {
		t.Fatal(err)
	}
	pc.DataBoundary = "external"
	cfg.Providers["anthropic-direct"] = pc
	cfg.Providers["anthropic-direct"].APIKeyRef.Env = "MUTATED"
	cfg.Models["claude"].Capabilities[0] = "vision"
	got := st.ProviderConfigs()
	got["anthropic-direct"].APIKeyRef.Env = "ALSO_MUTATED"
	if st.DataBoundary("anthropic-direct") != "internal" || st.ProviderConfigs()["anthropic-direct"].APIKeyRef.Env != "UPSTREAM_KEY" {
		t.Fatal("provider metadata not frozen")
	}
	route, _ := st.Route("claude")
	if route.Capabilities[0] != "tools" || route.ContextWindow != 8192 {
		t.Fatal("model metadata not frozen")
	}
	cfg.Providers["anthropic-direct"] = config.ProviderConfig{DataBoundary: "local"}
	if _, _, err := BuildState(cfg); err == nil {
		t.Fatal("invalid boundary accepted")
	}
}
