package config

import (
	"os"
	"path/filepath"
	"strings"
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

func TestLoadRejectsInvalidAuthHeader(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "bad.json")
	os.WriteFile(f, []byte(`{"providers":{"p":{"type":"anthropic","auth_header":"Bearer"}}}`), 0o600)
	if _, err := Load(f); err == nil {
		t.Fatal("expected rejection of invalid auth_header")
	}
}

func TestLoadAcceptsBearerAuthHeader(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "ok.json")
	os.WriteFile(f, []byte(`{"providers":{"p":{"type":"anthropic","auth_header":"bearer"}}}`), 0o600)
	cfg, err := Load(f)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Providers["p"].AuthHeader != "bearer" {
		t.Fatalf("auth_header not preserved: %+v", cfg.Providers["p"])
	}
}

func TestLoadRejectsAuthHeaderOnNonAnthropicProvider(t *testing.T) {
	// auth_header only has an effect on the anthropic provider (live.go only
	// wires it into Settings for type=="anthropic"); on bedrock/openai_compatible
	// it would validate but silently do nothing, so it must be rejected outright.
	for _, typ := range []string{"bedrock", "openai_compatible"} {
		dir := t.TempDir()
		f := filepath.Join(dir, "bad.json")
		os.WriteFile(f, []byte(`{"providers":{"p":{"type":"`+typ+`","auth_header":"bearer"}}}`), 0o600)
		if _, err := Load(f); err == nil {
			t.Fatalf("type %q: expected rejection of auth_header on a non-anthropic provider", typ)
		}
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

// --- T0: provider_store block + LoadRaw/ResolveProviders split (ADR-008) ---

// TestProviderStoreConfigParses: the optional provider_store block is parsed
// into Config.ProviderStore (nil when absent — opt-in, ADR-008 §1).
func TestProviderStoreConfigParses(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "c.json")
	os.WriteFile(f, []byte(`{"provider_store":{"type":"sqlite","path":"providers.db"}}`), 0o600)
	cfg, err := Load(f)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ProviderStore == nil {
		t.Fatal("provider_store block not parsed")
	}
	if cfg.ProviderStore.Type != "sqlite" || cfg.ProviderStore.Path != "providers.db" {
		t.Fatalf("provider_store fields wrong: %+v", cfg.ProviderStore)
	}

	// Absent → nil (opt-in default unchanged).
	f2 := filepath.Join(dir, "c2.json")
	os.WriteFile(f2, []byte(`{"providers":{}}`), 0o600)
	cfg2, err := Load(f2)
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.ProviderStore != nil {
		t.Fatalf("provider_store should be nil when absent, got %+v", cfg2.ProviderStore)
	}
}

// TestLoadRawDoesNotResolveProviderSecrets pins gate G1 (CRITICAL): LoadRaw
// parses + rejects inline secrets + validates OIDC, but does NOT resolve
// provider secret refs — so a file provider with an unset env ref does NOT
// crash boot/reload before the DB overlay can discard it.
func TestLoadRawDoesNotResolveProviderSecrets(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "c.json")
	// MISSING_REF_ENV is intentionally never set.
	os.WriteFile(f, []byte(`{"providers":{"p":{"type":"anthropic","api_key_ref":{"env":"MISSING_REF_ENV"}}}}`), 0o600)

	// LoadRaw must succeed (the unset ref is NOT resolved).
	cfg, err := LoadRaw(f)
	if err != nil {
		t.Fatalf("LoadRaw must not resolve provider secrets: %v", err)
	}
	if got := cfg.Providers["p"].APIKey; got != "" {
		t.Fatalf("LoadRaw must leave APIKey empty, got %q", got)
	}

	// ResolveProviders on the same config DOES fail (unset env).
	if err := ResolveProviders(cfg); err == nil {
		t.Fatal("ResolveProviders must fail on an unset env ref")
	}

	// And the back-compat Load (= LoadRaw + ResolveProviders) fails too.
	if _, err := Load(f); err == nil {
		t.Fatal("Load must fail on an unset env ref (back-compat)")
	}
}

// TestLoadRawRejectsInlineSecret: the §7 inline-api_key rejection lives in the
// raw parse, so it fires even without provider resolution.
func TestLoadRawRejectsInlineSecret(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "bad.json")
	os.WriteFile(f, []byte(`{"providers":{"x":{"type":"anthropic","api_key":"sk-plaintext"}}}`), 0o600)
	if _, err := LoadRaw(f); err == nil {
		t.Fatal("LoadRaw must still reject inline api_key")
	}
}

// TestResolveSecretRefExported: the exported resolver is the single code path
// for env/file refs (DB-overlaid providers reuse it).
func TestResolveSecretRefExported(t *testing.T) {
	t.Setenv("T0_REF_ENV", "sk-t0-val")
	got, err := ResolveSecretRef(&SecretRef{Env: "T0_REF_ENV"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "sk-t0-val" {
		t.Fatalf("ResolveSecretRef = %q", got)
	}
}

// --- T4: plugins block (ADR-009) ---

func TestPluginsBlockParses(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "c.json")
	os.WriteFile(f, []byte(`{"plugins":[{"name":"pii-mask","teams":["alpha","beta"]},{"name":"other"}]}`), 0o600)
	cfg, err := Load(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Plugins) != 2 {
		t.Fatalf("want 2 plugins, got %+v", cfg.Plugins)
	}
	if cfg.Plugins[0].Name != "pii-mask" || len(cfg.Plugins[0].Teams) != 2 {
		t.Fatalf("plugin[0] wrong: %+v", cfg.Plugins[0])
	}
	// empty Teams = global
	if cfg.Plugins[1].Name != "other" || len(cfg.Plugins[1].Teams) != 0 {
		t.Fatalf("plugin[1] (global) wrong: %+v", cfg.Plugins[1])
	}
}

func TestPluginsAbsentNil(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "c.json")
	os.WriteFile(f, []byte(`{"providers":{}}`), 0o600)
	cfg, err := Load(f)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Plugins != nil {
		t.Fatalf("plugins should be nil when absent, got %+v", cfg.Plugins)
	}
}

// --- T1: otel block (ADR-011) ---

func TestOTelBlockParsesAndValidates(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(body), 0o600)
		return p
	}
	// valid
	cfg, err := Load(write("ok.json", `{"otel":{"endpoint":"c:4318","protocol":"http","sample_ratio":0.25}}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OTel == nil || cfg.OTel.Endpoint != "c:4318" || cfg.OTel.SampleRatio == nil || *cfg.OTel.SampleRatio != 0.25 {
		t.Fatalf("otel parse wrong: %+v", cfg.OTel)
	}
	// explicit 0.0 preserved (≠ unset)
	cfg0, _ := Load(write("z.json", `{"otel":{"endpoint":"c","sample_ratio":0.0}}`))
	if cfg0.OTel.SampleRatio == nil || *cfg0.OTel.SampleRatio != 0 {
		t.Fatalf("explicit 0.0 not preserved: %+v", cfg0.OTel)
	}
	// unset ratio → nil pointer (assembly defaults to 1.0)
	cfgN, _ := Load(write("n.json", `{"otel":{"endpoint":"c"}}`))
	if cfgN.OTel.SampleRatio != nil {
		t.Fatalf("unset ratio should be nil, got %v", *cfgN.OTel.SampleRatio)
	}
	// absent → nil
	cfgA, _ := Load(write("a.json", `{"providers":{}}`))
	if cfgA.OTel != nil {
		t.Fatalf("otel should be nil when absent")
	}
	// endpoint missing → error
	if _, err := Load(write("noep.json", `{"otel":{"protocol":"http"}}`)); err == nil {
		t.Fatal("otel without endpoint must be rejected")
	}
	// bad protocol → error
	if _, err := Load(write("badproto.json", `{"otel":{"endpoint":"c","protocol":"thrift"}}`)); err == nil {
		t.Fatal("bad otel.protocol must be rejected")
	}
	// ratio out of range → error
	if _, err := Load(write("badratio.json", `{"otel":{"endpoint":"c","sample_ratio":2.0}}`)); err == nil {
		t.Fatal("sample_ratio>1 must be rejected")
	}
}

// --- T4: audit.anchor block (ADR-012) ---

func TestAnchorBlockParsesAndValidates(t *testing.T) {
	dir := t.TempDir()
	write := func(n, b string) string { p := filepath.Join(dir, n); os.WriteFile(p, []byte(b), 0o600); return p }
	cfg, err := Load(write("ok.json", `{"audit":{"buffer":{"path":"/x"},"sinks":[{"type":"stdout"}],"anchor":{"type":"s3","bucket":"b","prefix":"a","interval":"1m","retain_days":7}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Audit.Anchor == nil || cfg.Audit.Anchor.Bucket != "b" || cfg.Audit.Anchor.RetainDays != 7 {
		t.Fatalf("anchor parse wrong: %+v", cfg.Audit.Anchor)
	}
	// absent → nil
	cfgA, _ := Load(write("a.json", `{"audit":{"buffer":{"path":"/x"},"sinks":[{"type":"stdout"}]}}`))
	if cfgA.Audit.Anchor != nil {
		t.Fatal("anchor should be nil when absent")
	}
	// bucket missing → error
	if _, err := Load(write("nob.json", `{"audit":{"anchor":{"type":"s3"}}}`)); err == nil {
		t.Fatal("anchor without bucket must be rejected")
	}
	// bad type → error
	if _, err := Load(write("badt.json", `{"audit":{"anchor":{"type":"gcs","bucket":"b"}}}`)); err == nil {
		t.Fatal("non-s3 anchor type must be rejected")
	}
	// bad interval → error
	if _, err := Load(write("badi.json", `{"audit":{"anchor":{"type":"s3","bucket":"b","interval":"5 minutes"}}}`)); err == nil {
		t.Fatal("bad interval must be rejected")
	}
}

// --- Phase 1b Task 3: analytics.mode_b block (ADR-015 §5) ---

func TestAnalyticsModeBParsesAndValidates(t *testing.T) {
	dir := t.TempDir()
	write := func(n, b string) string { p := filepath.Join(dir, n); os.WriteFile(p, []byte(b), 0o600); return p }
	t.Setenv("INFERPLANE_ANALYTICS_PG_DSN", "postgres://u:p@host/db")

	cfg, err := Load(write("ok.json", `{"analytics":{"mode_b":{
		"dsn_ref":{"env":"INFERPLANE_ANALYTICS_PG_DSN"},
		"aggregated_audit_dir":"/mnt/shared/audit-aggregate",
		"poll_interval":"5s",
		"lease_ttl":"15s"
	}}}`))
	if err != nil {
		t.Fatal(err)
	}
	mb := cfg.Analytics.ModeB
	if mb == nil {
		t.Fatal("analytics.mode_b not parsed")
	}
	if mb.AggregatedAuditDir != "/mnt/shared/audit-aggregate" {
		t.Fatalf("aggregated_audit_dir = %q", mb.AggregatedAuditDir)
	}
	if mb.DSN != "postgres://u:p@host/db" {
		t.Fatalf("dsn_ref not resolved: %q", mb.DSN)
	}

	// absent → nil, existing Mode A/off behavior untouched.
	cfgA, err := Load(write("a.json", `{"audit":{"buffer":{"path":"/x"},"sinks":[{"type":"stdout"}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfgA.Analytics.ModeB != nil {
		t.Fatal("analytics.mode_b should be nil when absent")
	}
	path, enabled := ResolveAnalytics(cfgA)
	if enabled || path != "" {
		t.Fatalf("ResolveAnalytics with no analytics config = (%q,%v), want off", path, enabled)
	}
}

func TestAnalyticsModeBRejectsInlineDSN(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "bad.json")
	os.WriteFile(f, []byte(`{"analytics":{"mode_b":{"dsn":"postgres://u:p@host/db","aggregated_audit_dir":"/x"}}}`), 0o600)
	_, err := Load(f)
	if err == nil {
		t.Fatal("inline dsn (not dsn_ref) must be rejected")
	}
	if strings.Contains(err.Error(), "postgres://u:p") {
		t.Fatalf("error must never echo the inline value: %v", err)
	}
}

func TestAnalyticsModeBRejectsMalshapedDSNRef(t *testing.T) {
	dir := t.TempDir()
	secret := "postgres://real-secret-value"
	f := filepath.Join(dir, "bad.json")
	os.WriteFile(f, []byte(`{"analytics":{"mode_b":{"dsn_ref":{"env":"`+secret+`"},"aggregated_audit_dir":"/x"}}}`), 0o600)
	_, err := Load(f)
	if err == nil {
		t.Fatal("a secret-shaped env ref (not a valid env var name) must be rejected")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error must never echo the ref value: %v", err)
	}
}
