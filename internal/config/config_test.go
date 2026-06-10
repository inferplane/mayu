package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadResolvesSecretRef(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-test-xyz")
	cfg, err := Load(filepath.Join("..", "..", "testdata", "m2-config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	p, ok := cfg.Providers["anthropic-direct"]
	if !ok {
		t.Fatal("provider missing")
	}
	if p.APIKey != "sk-test-xyz" {
		t.Fatalf("secret ref not resolved: %q", p.APIKey)
	}
	if cfg.Models["claude-sonnet-4-6"].Targets[0].Provider != "anthropic-direct" {
		t.Fatalf("model mapping wrong: %+v", cfg.Models)
	}
}

func TestLoadRejectsInlineSecret(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "bad.json")
	os.WriteFile(f, []byte(`{"providers":{"x":{"type":"anthropic","api_key":"sk-plaintext"}}}`), 0o600)
	if _, err := Load(f); err == nil {
		t.Fatal("expected rejection of inline api_key")
	}
}
