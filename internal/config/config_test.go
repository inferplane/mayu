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

func TestLoadBedrockProviderFields(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "c.json")
	os.WriteFile(f, []byte(`{
	  "providers": {"bedrock-us": {"type":"bedrock","region":"us-west-2","auth":{"mode":"profile","profile":"dev"}}},
	  "models": {"claude-bedrock": {"targets":[{"provider":"bedrock-us","model":"anthropic.claude-sonnet-4-6-v1:0","api":"invoke_model"}]}}
	}`), 0o600)
	cfg, err := Load(f)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Providers["bedrock-us"].Region != "us-west-2" || cfg.Providers["bedrock-us"].Auth.Mode != "profile" || cfg.Providers["bedrock-us"].Auth.Profile != "dev" {
		t.Fatalf("bedrock fields: %+v", cfg.Providers["bedrock-us"])
	}
	if cfg.Models["claude-bedrock"].Targets[0].API != "invoke_model" {
		t.Fatalf("target api: %+v", cfg.Models["claude-bedrock"].Targets[0])
	}
}

func TestLoadTeamsAndPricing(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "c.json")
	os.WriteFile(f, []byte(`{
	  "teams": {"platform-eng": {"allowed_models":["claude-sonnet-4-6"],"rate_limit":{"requests_per_minute":300,"tokens_per_minute":2000000},"quota":{"tokens_per_day":50000000,"on_exceeded":"block"},"budget":{"usd_per_month":5000,"on_exceeded":"warn"}}},
	  "pricing": {"on_missing":"allow","overrides":{"anthropic-direct":{"claude-sonnet-4-6":{"input_per_mtok":3.0,"output_per_mtok":15.0}}}}
	}`), 0o600)
	cfg, err := Load(f)
	if err != nil {
		t.Fatal(err)
	}
	tm := cfg.Teams["platform-eng"]
	if tm.RateLimit.RequestsPerMinute != 300 || tm.Quota.TokensPerDay != 50000000 || tm.Quota.OnExceeded != "block" || tm.Budget.OnExceeded != "warn" {
		t.Fatalf("team: %+v", tm)
	}
	if cfg.Pricing.OnMissing != "allow" {
		t.Fatalf("pricing on_missing: %q", cfg.Pricing.OnMissing)
	}
}

func TestLoadKeyStoreAuditAdmin(t *testing.T) {
	t.Setenv("ADMIN_TOK", "secret-admin")
	dir := t.TempDir()
	f := filepath.Join(dir, "c.json")
	os.WriteFile(f, []byte(`{
	  "server": {"listen":":8080","admin_listen":":9090","admin_auth":{"token_refs":[{"env":"ADMIN_TOK"}]}},
	  "key_store": {"type":"sqlite","path":"/tmp/keys.db"},
	  "audit": {"failure_mode":"buffer_then_block","buffer":{"path":"/tmp/audit.wal"},"sinks":[{"type":"stdout"},{"type":"file","path":"/tmp/audit.jsonl"}]}
	}`), 0o600)
	cfg, err := Load(f)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.KeyStore.Type != "sqlite" || cfg.KeyStore.Path != "/tmp/keys.db" {
		t.Fatalf("key_store: %+v", cfg.KeyStore)
	}
	if len(cfg.Server.AdminAuth.TokenRefs) != 1 || len(cfg.Server.AdminAuth.Tokens) != 1 || cfg.Server.AdminAuth.Tokens[0] != "secret-admin" {
		t.Fatalf("admin tokens not resolved: %+v", cfg.Server.AdminAuth)
	}
	if cfg.Audit.FailureMode != "buffer_then_block" || len(cfg.Audit.Sinks) != 2 {
		t.Fatalf("audit: %+v", cfg.Audit)
	}
}
