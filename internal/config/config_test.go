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

func TestLoadFileSecretTrimsTrailingNewline(t *testing.T) {
	dir := t.TempDir()
	secretFile := filepath.Join(dir, "key")
	os.WriteFile(secretFile, []byte("sk-from-file\n"), 0o600) // K8s/echo leave a trailing \n
	cfgFile := filepath.Join(dir, "cfg.json")
	os.WriteFile(cfgFile, []byte(`{"providers":{"p":{"type":"anthropic","api_key_ref":{"file":"`+secretFile+`"}}}}`), 0o600)
	cfg, err := Load(cfgFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Providers["p"].APIKey; got != "sk-from-file" {
		t.Fatalf("file secret not trimmed: %q", got)
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
