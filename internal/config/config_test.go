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

func TestLoadServerTLS(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "c.json")
	os.WriteFile(f, []byte(`{"server":{"listen":":8080","admin_listen":":9090","tls":{"cert_file":"/etc/tls/cert.pem","key_file":"/etc/tls/key.pem"}}}`), 0o600)
	cfg, err := Load(f)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.TLS.CertFile != "/etc/tls/cert.pem" || cfg.Server.TLS.KeyFile != "/etc/tls/key.pem" {
		t.Fatalf("tls: %+v", cfg.Server.TLS)
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

// --- OIDC admin auth (plan 2026-06-12, tasks 1) ---

// writeOIDCConfig writes a minimal config with the given oidc block (raw JSON,
// empty string = omit) and admin token env ref, returning its path.
func writeOIDCConfig(t *testing.T, oidcJSON string) string {
	t.Helper()
	t.Setenv("TEST_ADMIN_TOKEN", testAdminTokenValue)
	oidcField := ""
	if oidcJSON != "" {
		oidcField = `, "oidc": ` + oidcJSON
	}
	cfg := `{
	  "server": {
	    "listen": ":8080", "admin_listen": ":9090",
	    "admin_auth": { "token_refs": [ { "env": "TEST_ADMIN_TOKEN" } ]` + oidcField + ` }
	  },
	  "key_store": { "type": "sqlite", "path": "/tmp/k.db" },
	  "audit": { "buffer": { "path": "/tmp/a.wal" }, "sinks": [ { "type": "stdout" } ] }
	}`
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

var testAdminTokenValue = "opaque-static-token"

func TestOIDCConfigLoads(t *testing.T) {
	cfg, err := Load(writeOIDCConfig(t, `{
	  "issuer": "https://idp.example.com/realms/dev",
	  "client_id": "inferplane-admin",
	  "admin_groups": ["platform-admins"],
	  "group_mappings": [
	    {"group": "team-alpha", "teams": ["alpha"]},
	    {"group": "*", "teams": ["sandbox"]}
	  ]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	o := cfg.Server.AdminAuth.OIDC
	if o == nil {
		t.Fatal("oidc block not parsed")
	}
	if o.Issuer != "https://idp.example.com/realms/dev" || o.ClientID != "inferplane-admin" {
		t.Fatalf("issuer/client_id: %+v", o)
	}
	if o.GroupsClaim != "groups" {
		t.Fatalf("groups_claim default = %q, want groups", o.GroupsClaim)
	}
	if len(o.GroupMappings) != 2 || o.GroupMappings[0].Teams[0] != "alpha" {
		t.Fatalf("group_mappings: %+v", o.GroupMappings)
	}
}

func TestOIDCConfigAbsentIsNil(t *testing.T) {
	cfg, err := Load(writeOIDCConfig(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.AdminAuth.OIDC != nil {
		t.Fatal("oidc must be nil when absent")
	}
}

func TestOIDCConfigRejectsMissingClientID(t *testing.T) {
	for name, block := range map[string]string{
		"empty client_id": `{"issuer": "https://idp.example.com", "client_id": ""}`,
		"empty issuer":    `{"issuer": "", "client_id": "x"}`,
	} {
		if _, err := Load(writeOIDCConfig(t, block)); err == nil {
			t.Fatalf("%s: want load error", name)
		}
	}
}

func TestOIDCConfigRejectsNonHTTPSIssuer(t *testing.T) {
	for name, issuer := range map[string]string{
		"http":       "http://idp.example.com",
		"query":      "https://idp.example.com/?x=1",
		"fragment":   "https://idp.example.com/#frag",
		"userinfo":   "https://user:pw@idp.example.com",
		"not a url":  "idp.example.com",
		"empty host": "https://",
	} {
		block := `{"issuer": "` + issuer + `", "client_id": "x"}`
		if _, err := Load(writeOIDCConfig(t, block)); err == nil {
			t.Fatalf("%s (%s): want load error", name, issuer)
		}
	}
}

func TestOIDCConfigRejectsDuplicateGroupKeys(t *testing.T) {
	block := `{"issuer": "https://idp.example.com", "client_id": "x",
	  "group_mappings": [
	    {"group": "team-a", "teams": ["a"]},
	    {"group": "team-a", "teams": ["b"]}
	  ]}`
	if _, err := Load(writeOIDCConfig(t, block)); err == nil {
		t.Fatal("duplicate group keys: want load error")
	}
}

// TestOIDCConfigRejectsJWTShapedStaticToken pins the break-glass invariant
// (P2 gate, triple-confirmed): a static admin token that the shared shape
// predicate would route to the OIDC path can never be configured alongside an
// oidc block — it would be unverifiable and lock operators out during an IdP
// outage.
func TestOIDCConfigRejectsJWTShapedStaticToken(t *testing.T) {
	old := testAdminTokenValue
	testAdminTokenValue = "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ4In0.c2ln" // JWT-shaped
	defer func() { testAdminTokenValue = old }()

	block := `{"issuer": "https://idp.example.com", "client_id": "x"}`
	if _, err := Load(writeOIDCConfig(t, block)); err == nil {
		t.Fatal("JWT-shaped static token with oidc enabled: want load error")
	}

	// Without the oidc block the same token loads fine (back-compat).
	if _, err := Load(writeOIDCConfig(t, "")); err != nil {
		t.Fatalf("JWT-shaped token without oidc must load: %v", err)
	}
}
