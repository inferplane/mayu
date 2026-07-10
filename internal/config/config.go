// Package config loads inferplane's M2 configuration subset. Secrets are only
// referenced (env/file/secret), never inline — an inline api_key is rejected
// at load (design doc §7).
package config

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/inferplane/inferplane/internal/adminauth"
)

// envRefShape is the allowed shape of an env-var secret ref: a POSIX-ish env var
// NAME. A pasted secret (sk-…, dashes, mixed case) fails it, so a secret value
// can never be accepted/persisted as a "ref" (ADR-008 gate C1).
var envRefShape = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ValidateSecretRef checks a ref's SHAPE (not resolvability): a nil ref is valid
// (keyless provider); a non-nil ref must set exactly one of env/file — the env
// an environment-variable NAME, the file an ABSOLUTE path. This is the single
// shared guard for both the UI write path (configapi) and the file→DB seed path
// (providerstore), so a malformed/secret-shaped ref is rejected before it can be
// persisted, exported, or audited. Error messages never echo the ref value.
func ValidateSecretRef(ref *SecretRef) error {
	if ref == nil {
		return nil
	}
	switch {
	case ref.Env != "" && ref.File != "":
		return fmt.Errorf("secret ref must set either env or file, not both")
	case ref.Env != "":
		if !envRefShape.MatchString(ref.Env) {
			return fmt.Errorf("secret ref env must be an environment variable name (it is a reference, not the secret value)")
		}
	case ref.File != "":
		if !strings.HasPrefix(ref.File, "/") {
			return fmt.Errorf("secret ref file must be an absolute path (it is a reference, not the secret value)")
		}
	default:
		return fmt.Errorf("secret ref must set env or file")
	}
	return nil
}

type SecretRef struct {
	Env  string `json:"env,omitempty"`
	File string `json:"file,omitempty"`
}

type ProviderConfig struct {
	Type      string     `json:"type"`
	BaseURL   string     `json:"base_url"`
	APIKeyRef *SecretRef `json:"api_key_ref,omitempty"`
	// APIKey is the RESOLVED secret, filled at load. Tagged "-" so a config
	// file can never set it inline (defense-in-depth alongside the scan below).
	APIKey string `json:"-"`
	// Region and Auth configure the Bedrock provider (M4). Region is the AWS
	// region; Auth selects the credential mode (irsa|pod_identity|profile|
	// static|default) and, for "profile", the named shared-config profile.
	Region string `json:"region,omitempty"`
	Auth   struct {
		Mode    string `json:"mode"`
		Profile string `json:"profile,omitempty"`
	} `json:"auth,omitempty"`
	// AuthHeader selects how the anthropic provider sends its credential:
	// "x-api-key" (default, api.anthropic.com) or "bearer" (Anthropic-compatible
	// endpoints such as OpenRouter that expect Authorization: Bearer).
	AuthHeader string `json:"auth_header,omitempty"`
	// GuardrailID/GuardrailVersion configure the Bedrock provider's DEFAULT
	// Guardrail (D6, ADR-019) — applied to every invocation unless a team
	// record overrides it. Meaningful only for type "bedrock"; empty Version
	// with a non-empty ID defaults to "DRAFT" at the provider layer.
	GuardrailID      string `json:"guardrail_id,omitempty"`
	GuardrailVersion string `json:"guardrail_version,omitempty"`
}

type Target struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	// API selects the Bedrock call path for this target (M4):
	// invoke_model|converse|mantle. Empty means default routing.
	API string `json:"api,omitempty"`
}

type ModelConfig struct {
	Aliases []string `json:"aliases,omitempty"`
	Targets []Target `json:"targets"`
}

// AdminAuth guards the admin plane (§5.5). Tokens are referenced via
// SecretRef (env/file) and resolved into Tokens at load — never inline.
// OIDC (v0.2, ADR-004) promotes the admin API to IdP-group authorization;
// the static tokens remain as break-glass.
type AdminAuth struct {
	TokenRefs []SecretRef `json:"token_refs,omitempty"`
	Tokens    []string    `json:"-"` // resolved at load
	OIDC      *OIDCConfig `json:"oidc,omitempty"`
}

// OIDCConfig connects the Identity layer (§5.1): the gateway validates
// externally-acquired ID tokens against the issuer's JWKS and owns only the
// groups→team mapping rules. Issuer must be an absolute https URL (MITM-JWKS
// / SSRF-by-config guard); client_id is the mandatory expected audience —
// leaving it optional is the classic cross-app token-reuse hole.
type OIDCConfig struct {
	Issuer        string         `json:"issuer"`
	ClientID      string         `json:"client_id"`
	GroupsClaim   string         `json:"groups_claim,omitempty"` // default "groups"; top-level claim, no traversal
	AdminGroups   []string       `json:"admin_groups,omitempty"`
	GroupMappings []GroupMapping `json:"group_mappings,omitempty"`
}

// GroupMapping maps one IdP group to gateway teams ("*" = explicit wildcard).
type GroupMapping struct {
	Group string   `json:"group"`
	Teams []string `json:"teams"`
}

// TLSConfig optionally terminates TLS on the data plane (non-K8s single binary,
// design §2.3). Both files must be set, or neither. K8s deployments terminate
// TLS at the ingress/mesh and leave this empty.
type TLSConfig struct {
	CertFile string `json:"cert_file"`
	KeyFile  string `json:"key_file"`
}

type ServerConfig struct {
	Listen      string    `json:"listen"`
	AdminListen string    `json:"admin_listen"`
	DrainGrace  string    `json:"drain_grace"`
	AdminAuth   AdminAuth `json:"admin_auth"`
	TLS         TLSConfig `json:"tls"`
}

// KeyStoreConfig selects the virtual-key backend. M3 ships "sqlite";
// "postgres" is the HA path (v0.2).
type KeyStoreConfig struct {
	Type string `json:"type"`
	Path string `json:"path"`
}

// ProviderStoreConfig optionally enables the DB-authoritative provider/model
// topology store (ADR-008, Stage 2). Absent (nil) → providers/models come from
// this file and UI writes return 405 (ADR-005, unchanged). Present → the DB is
// authoritative for the reloadable topology; "sqlite" ships, "postgres" is the
// HA path. Same shape as KeyStoreConfig for consistency.
type ProviderStoreConfig struct {
	Type string `json:"type"`
	Path string `json:"path"`
}

// AuditSink configures one audit output: "stdout" or "file" (with Path).
type AuditSink struct {
	Type string `json:"type"`
	Path string `json:"path,omitempty"`
}

// AuditBuffer is the disk-backed WAL location for buffer_then_block.
type AuditBuffer struct {
	Path string `json:"path"`
}

// AnchorConfig enables opt-in S3 Object Lock audit anchoring (ADR-012). Absent
// (nil) → no anchoring (no-op). Interval is a Go duration (default 5m); bucket
// required; RetainDays>0 sets per-object COMPLIANCE retention.
type AnchorConfig struct {
	Type       string `json:"type"` // "s3"
	Bucket     string `json:"bucket"`
	Prefix     string `json:"prefix,omitempty"`
	Region     string `json:"region,omitempty"`
	Endpoint   string `json:"endpoint,omitempty"`
	Interval   string `json:"interval,omitempty"`
	RetainDays int    `json:"retain_days,omitempty"`
}

type AuditConfig struct {
	FailureMode string         `json:"failure_mode"` // buffer_then_block (default)
	Buffer      AuditBuffer    `json:"buffer"`
	Sinks       []AuditSink    `json:"sinks"`
	Anchor      *AnchorConfig  `json:"anchor,omitempty"`
	LogBodies   *BodyLogConfig `json:"log_bodies,omitempty"`
}

// BodyLogConfig enables opt-in request/response body capture (D4, ADR-018).
// Presence of this block (non-nil) enables capture — bodies live in a
// separate mutable/deletable/encrypted store, never the audit chain. Type
// selects the backend: "sqlite" (default, single-instance) or "postgres"
// (HA, requires DSNRef). KeyRef is always required and resolves to 64 hex
// chars (a 32-byte AES-256 master key) — never inline.
type BodyLogConfig struct {
	Type         string     `json:"type,omitempty"` // "sqlite" (default) | "postgres"
	Path         string     `json:"path,omitempty"` // sqlite path; default derived (bodies.db beside the file audit sink)
	DSNRef       *SecretRef `json:"dsn_ref,omitempty"`
	DSN          string     `json:"-"` // resolved at load (postgres only)
	KeyRef       *SecretRef `json:"key_ref"`
	Key          string     `json:"-"`                        // resolved 64-hex-char master key
	TTL          string     `json:"ttl,omitempty"`            // Go duration; default "168h" (7 days)
	MaxBytes     int64      `json:"max_bytes,omitempty"`      // total store size cap; default 1 GiB
	MaxBodyBytes int64      `json:"max_body_bytes,omitempty"` // per-record cap; default 1 MiB
}

// RateLimitConfig is a team's instance-local token-bucket gate (§5.3): RPM and
// TPM pre-block thresholds.
type RateLimitConfig struct {
	RequestsPerMinute int64 `json:"requests_per_minute"`
	TokensPerMinute   int64 `json:"tokens_per_minute"`
}

// QuotaConfig is a team's daily/monthly token window (two-phase optimistic
// check + post-debit). OnExceeded selects block|warn.
type QuotaConfig struct {
	TokensPerDay   int64  `json:"tokens_per_day"`
	TokensPerMonth int64  `json:"tokens_per_month"`
	OnExceeded     string `json:"on_exceeded"` // block|warn
}

// BudgetConfig is a team's monthly spend ceiling. USDPerMonth is a human USD
// float in config, converted to µUSD at use.
type BudgetConfig struct {
	USDPerMonth float64 `json:"usd_per_month"` // converted to µUSD at use
	OnExceeded  string  `json:"on_exceeded"`
}

type TeamConfig struct {
	AllowedModels []string        `json:"allowed_models"`
	RateLimit     RateLimitConfig `json:"rate_limit"`
	Quota         QuotaConfig     `json:"quota"`
	Budget        BudgetConfig    `json:"budget"`
	// AllowedRegions restricts this team to providers labeled with one of
	// these regions (D7, ADR-020). Empty = unrestricted.
	AllowedRegions []string `json:"allowed_regions,omitempty"`
}

// RateConfig holds per-MTok rates as human USD floats in config, converted to
// µUSD-per-MTok int64 at load.
type RateConfig struct {
	InputPerMTok        float64 `json:"input_per_mtok"`
	OutputPerMTok       float64 `json:"output_per_mtok"`
	CacheReadPerMTok    float64 `json:"cache_read_per_mtok"`
	CacheWrite5mPerMTok float64 `json:"cache_write_5m_per_mtok"`
	CacheWrite1hPerMTok float64 `json:"cache_write_1h_per_mtok"`
}

// PricingConfig configures cost computation: on_missing policy (allow|block)
// and per-(provider,model) rate overrides.
type PricingConfig struct {
	OnMissing string                           `json:"on_missing"` // allow|block
	Overrides map[string]map[string]RateConfig `json:"overrides"`  // provider → model → rate
}

// PluginConfig enables a request-transform filter plugin (the spec's filter
// chain ⑥, ADR-009). Name must match a registered filter (e.g. "pii-mask").
// Teams scopes it to those teams; an empty Teams means GLOBAL (all teams). The
// filter name is resolved against the registry at assembly (boot); an unknown
// name is rejected there.
type PluginConfig struct {
	Name  string   `json:"name"`
	Teams []string `json:"teams,omitempty"`
}

// OTelConfig enables opt-in OpenTelemetry tracing (ADR-011). Absent (nil) →
// no tracer installed (no-op, zero overhead). SampleRatio is a NULLABLE pointer:
// nil → 1.0 (sample all), explicit 0.0 → sample none (the two are
// distinguishable); validated to [0,1]. Protocol is "http" (default) or "grpc".
type OTelConfig struct {
	Endpoint    string   `json:"endpoint"`
	Protocol    string   `json:"protocol,omitempty"`
	Insecure    bool     `json:"insecure,omitempty"`
	SampleRatio *float64 `json:"sample_ratio,omitempty"`
	ServiceName string   `json:"service_name,omitempty"`
}

type Config struct {
	Server        ServerConfig              `json:"server"`
	Providers     map[string]ProviderConfig `json:"providers"`
	Models        map[string]ModelConfig    `json:"models"`
	KeyStore      KeyStoreConfig            `json:"key_store"`
	ProviderStore *ProviderStoreConfig      `json:"provider_store,omitempty"`
	Audit         AuditConfig               `json:"audit"`
	Teams         map[string]TeamConfig     `json:"teams"`
	Pricing       PricingConfig             `json:"pricing"`
	Plugins       []PluginConfig            `json:"plugins,omitempty"`
	OTel          *OTelConfig               `json:"otel,omitempty"`
	Probe         ProbeConfig               `json:"probe,omitempty"`
	Analytics     AnalyticsConfig           `json:"analytics,omitempty"`
	BudgetAlerts  *BudgetAlertsConfig       `json:"budget_alerts,omitempty"`
}

// BudgetAlertsConfig enables webhook budget alerts (D5b, ADR-017): a team's
// monthly-budget utilization crossing a threshold POSTs a JSON payload to
// WebhookURLRef. The URL is referenced, never inline — Slack incoming-webhook
// and SNS HTTPS-subscription URLs routinely embed a capability token, the
// same trust level as an API key (§7). Absent (nil) → alerting off.
type BudgetAlertsConfig struct {
	WebhookURLRef *SecretRef `json:"webhook_url_ref"`
	WebhookURL    string     `json:"-"`                    // resolved at load
	Thresholds    []float64  `json:"thresholds,omitempty"` // ratios in (0,+inf); default [0.8, 1.0]
	Timeout       string     `json:"timeout,omitempty"`    // Go duration; default "5s"
}

// AnalyticsConfig configures the derived analytics index (design spec §4 / D1).
// The index is default-on when a file audit sink exists (a deployment that
// already persists audit gets usage analytics out of the box); Disabled turns
// it off, and Path overrides the derived location.
type AnalyticsConfig struct {
	Path     string          `json:"path,omitempty"`
	Disabled bool            `json:"disabled,omitempty"`
	ModeB    *AnalyticsModeB `json:"mode_b,omitempty"`
}

// AnalyticsModeB configures the shared Postgres analytics store (ADR-015).
// DSN is the resolved secret and is never accepted from, or written to, JSON.
type AnalyticsModeB struct {
	AggregatedAuditDir string     `json:"aggregated_audit_dir"`
	DSN                string     `json:"-"`
	DSNRef             *SecretRef `json:"dsn_ref"`
	PollInterval       string     `json:"poll_interval"`
	LeaseTTL           string     `json:"lease_ttl"`
}

// ResolveAnalytics decides whether the analytics index is enabled and at which
// path. Rules (review-corrected): Disabled wins → off. An explicit Path always
// enables (live ingestion via the audit Sink needs no file sink). Otherwise the
// path is derived from the first file audit sink's directory; with no file sink
// and no explicit path the index is off (nothing to derive or replay).
func ResolveAnalytics(c *Config) (path string, enabled bool) {
	if c.Analytics.Disabled {
		return "", false
	}
	if c.Analytics.Path != "" {
		return c.Analytics.Path, true
	}
	for _, s := range c.Audit.Sinks {
		if s.Type == "file" && s.Path != "" {
			return filepath.Join(filepath.Dir(s.Path), "analytics.db"), true
		}
	}
	return "", false
}

// ProbeConfig configures the admin connection-test probe (ADR-014 D2).
// AllowedHosts, when non-empty, restricts probe targets to those hostnames; an
// empty list permits any host (the cloud metadata endpoint is always blocked).
type ProbeConfig struct {
	AllowedHosts []string `json:"allowed_hosts,omitempty"`
}

// Load parses the config and resolves every secret ref — the back-compat entry
// point (= LoadRaw + ResolveProviders). Used when no provider store is enabled:
// file providers are authoritative, so their secrets must resolve at boot.
func Load(path string) (*Config, error) {
	cfg, err := LoadRaw(path)
	if err != nil {
		return nil, err
	}
	if err := ResolveProviders(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// LoadRaw parses the config, rejects inline secrets (§7), resolves admin tokens,
// and validates the OIDC block — but does NOT resolve provider secret refs
// (ADR-008 gate G1). When a provider store is authoritative, file providers may
// be stale/ignored, so resolving their refs at boot would crash the gateway
// before the DB overlay could discard them; the assembly resolves only the
// effective (DB-overlaid) providers via ResolveProviders.
func LoadRaw(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// Reject inline secrets before structured parse: any provider object with
	// a literal "api_key" key is a config error (§7).
	var probe struct {
		Providers map[string]map[string]json.RawMessage `json:"providers"`
		Analytics struct {
			ModeB map[string]json.RawMessage `json:"mode_b"`
		} `json:"analytics"`
		Audit struct {
			LogBodies map[string]json.RawMessage `json:"log_bodies"`
		} `json:"audit"`
		BudgetAlerts map[string]json.RawMessage `json:"budget_alerts"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	for name, p := range probe.Providers {
		if _, bad := p["api_key"]; bad {
			return nil, fmt.Errorf("config: provider %q has inline api_key; use api_key_ref (§7)", name)
		}
	}
	if _, bad := probe.Analytics.ModeB["dsn"]; bad {
		return nil, fmt.Errorf("config: analytics.mode_b has inline dsn; use dsn_ref (§7)")
	}
	if _, bad := probe.Audit.LogBodies["key"]; bad {
		return nil, fmt.Errorf("config: audit.log_bodies has inline key; use key_ref (§7)")
	}
	if _, bad := probe.Audit.LogBodies["dsn"]; bad {
		return nil, fmt.Errorf("config: audit.log_bodies has inline dsn; use dsn_ref (§7)")
	}
	if _, bad := probe.BudgetAlerts["webhook_url"]; bad {
		return nil, fmt.Errorf("config: budget_alerts has inline webhook_url; use webhook_url_ref (§7)")
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	for i := range cfg.Server.AdminAuth.TokenRefs {
		ref := cfg.Server.AdminAuth.TokenRefs[i]
		tok, err := ResolveSecretRef(&ref)
		if err != nil {
			return nil, fmt.Errorf("config: admin token: %w", err)
		}
		cfg.Server.AdminAuth.Tokens = append(cfg.Server.AdminAuth.Tokens, tok)
	}
	if err := validateOIDC(&cfg.Server.AdminAuth); err != nil {
		return nil, err
	}
	if err := validateOTel(cfg.OTel); err != nil {
		return nil, err
	}
	if err := validateAnchor(cfg.Audit.Anchor); err != nil {
		return nil, err
	}
	if err := validateAnalyticsModeB(cfg.Analytics.ModeB); err != nil {
		return nil, err
	}
	if err := validateBodyLog(cfg.Audit.LogBodies); err != nil {
		return nil, err
	}
	if err := validateBudgetAlerts(cfg.BudgetAlerts); err != nil {
		return nil, err
	}
	if err := validateModelAliases(cfg.Models); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func validateModelAliases(models map[string]ModelConfig) error {
	seen := make(map[string]string)
	for model, mc := range models {
		for _, alias := range mc.Aliases {
			if _, ok := models[alias]; ok {
				return fmt.Errorf("config: model %q alias %q collides with existing model name", model, alias)
			}
			if prev, ok := seen[alias]; ok {
				return fmt.Errorf("config: model alias %q declared by both %q and %q", alias, prev, model)
			}
			seen[alias] = model
		}
	}
	return nil
}

// validateBodyLog checks the opt-in audit.log_bodies block (D4, ADR-018).
// key_ref is always required and resolves to a 64-hex-char (32-byte) master
// key — the error never echoes the resolved value. postgres additionally
// requires dsn_ref, resolved the same way as analytics.mode_b's DSN (never
// inline). nil block (body logging off) is valid.
func validateBodyLog(bl *BodyLogConfig) error {
	if bl == nil {
		return nil
	}
	if bl.Type != "" && bl.Type != "sqlite" && bl.Type != "postgres" {
		return fmt.Errorf("config: audit.log_bodies.type must be \"sqlite\" or \"postgres\", got %q", bl.Type)
	}
	if bl.KeyRef == nil {
		return fmt.Errorf("config: audit.log_bodies.key_ref is required")
	}
	if err := ValidateSecretRef(bl.KeyRef); err != nil {
		return fmt.Errorf("config: audit.log_bodies.key_ref: %w", err)
	}
	key, err := ResolveSecretRef(bl.KeyRef)
	if err != nil {
		return fmt.Errorf("config: audit.log_bodies.key_ref: %w", err)
	}
	if raw, hexErr := hex.DecodeString(key); hexErr != nil || len(raw) != 32 {
		return fmt.Errorf("config: audit.log_bodies.key_ref must resolve to 64 hex characters (a 32-byte AES-256 key)")
	}
	bl.Key = key
	if bl.Type == "postgres" {
		if bl.DSNRef == nil {
			return fmt.Errorf("config: audit.log_bodies.dsn_ref is required when type is \"postgres\"")
		}
		if err := ValidateSecretRef(bl.DSNRef); err != nil {
			return fmt.Errorf("config: audit.log_bodies.dsn_ref: %w", err)
		}
		dsn, err := ResolveSecretRef(bl.DSNRef)
		if err != nil {
			return fmt.Errorf("config: audit.log_bodies.dsn_ref: %w", err)
		}
		bl.DSN = dsn
	}
	if bl.TTL == "" {
		bl.TTL = "168h"
	}
	if err := validateDurationString("audit.log_bodies.ttl", bl.TTL, time.Minute); err != nil {
		return err
	}
	if bl.MaxBytes < 0 {
		return fmt.Errorf("config: audit.log_bodies.max_bytes must be >= 0")
	}
	if bl.MaxBytes == 0 {
		bl.MaxBytes = 1 << 30 // 1 GiB
	}
	if bl.MaxBodyBytes < 0 {
		return fmt.Errorf("config: audit.log_bodies.max_body_bytes must be >= 0")
	}
	if bl.MaxBodyBytes == 0 {
		bl.MaxBodyBytes = 1 << 20 // 1 MiB
	}
	return nil
}

// ResolveBodyPath derives the default SQLite body-store path (bodies.db
// beside the first file audit sink), mirroring ResolveAnalytics. An explicit
// Path always wins; with no file sink and no explicit path, it falls back to
// "bodies.db" in the working directory (still opt-in — log_bodies must be
// configured at all to reach this).
func ResolveBodyPath(c *Config) string {
	if c.Audit.LogBodies != nil && c.Audit.LogBodies.Path != "" {
		return c.Audit.LogBodies.Path
	}
	for _, s := range c.Audit.Sinks {
		if s.Type == "file" && s.Path != "" {
			return filepath.Join(filepath.Dir(s.Path), "bodies.db")
		}
	}
	return "bodies.db"
}

// validateBudgetAlerts checks the opt-in budget-alert block (D5b, ADR-017).
// The webhook URL is always referenced and resolved here, never accepted
// inline; it must resolve to an absolute http(s) URL. Thresholds default to
// [0.8, 1.0] when unset; each must be > 0. nil block (alerting off) is valid.
func validateBudgetAlerts(ba *BudgetAlertsConfig) error {
	if ba == nil {
		return nil
	}
	if ba.WebhookURLRef == nil {
		return fmt.Errorf("config: budget_alerts.webhook_url_ref is required")
	}
	if err := ValidateSecretRef(ba.WebhookURLRef); err != nil {
		return fmt.Errorf("config: budget_alerts.webhook_url_ref: %w", err)
	}
	webhookURL, err := ResolveSecretRef(ba.WebhookURLRef)
	if err != nil {
		return fmt.Errorf("config: budget_alerts.webhook_url_ref: %w", err)
	}
	u, err := url.Parse(webhookURL)
	if err != nil || !u.IsAbs() || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("config: budget_alerts webhook URL must be an absolute http(s) URL")
	}
	ba.WebhookURL = webhookURL
	if len(ba.Thresholds) == 0 {
		ba.Thresholds = []float64{0.8, 1.0}
	}
	for _, t := range ba.Thresholds {
		if t <= 0 {
			return fmt.Errorf("config: budget_alerts.thresholds entries must be > 0, got %v", t)
		}
	}
	if err := validateDurationString("budget_alerts.timeout", ba.Timeout, time.Millisecond); err != nil {
		return err
	}
	return nil
}

// validateAnalyticsModeB checks the opt-in shared analytics store block. The
// Postgres DSN is always referenced and resolved here, never accepted inline.
func validateAnalyticsModeB(mb *AnalyticsModeB) error {
	if mb == nil {
		return nil
	}
	if mb.AggregatedAuditDir == "" {
		return fmt.Errorf("config: analytics.mode_b.aggregated_audit_dir is required")
	}
	if err := ValidateSecretRef(mb.DSNRef); err != nil {
		return fmt.Errorf("config: analytics.mode_b.dsn_ref: %w", err)
	}
	dsn, err := ResolveSecretRef(mb.DSNRef)
	if err != nil {
		return fmt.Errorf("config: analytics.mode_b.dsn_ref: %w", err)
	}
	mb.DSN = dsn
	// A sub-second TTL/poll truncates to 0 whole seconds once it reaches the
	// Postgres interval math (pgstore converts via int64(d.Seconds())), which
	// would make a lease expire the instant it's created — reject rather than
	// silently accept an unusable value.
	if err := validateDurationString("analytics.mode_b.poll_interval", mb.PollInterval, time.Second); err != nil {
		return err
	}
	if err := validateDurationString("analytics.mode_b.lease_ttl", mb.LeaseTTL, time.Second); err != nil {
		return err
	}
	return nil
}

func validateDurationString(name, value string, min time.Duration) error {
	if value == "" {
		return nil
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return fmt.Errorf("config: %s %q: %w", name, value, err)
	}
	if d < min {
		return fmt.Errorf("config: %s must be >= %s, got %q", name, min, value)
	}
	return nil
}

// validateAnchor checks the opt-in audit-anchor block (ADR-012): type must be
// "s3", bucket required, and interval (if set) must parse as a Go duration. nil
// block (anchoring off) is valid.
func validateAnchor(a *AnchorConfig) error {
	if a == nil {
		return nil
	}
	if a.Type != "s3" {
		return fmt.Errorf("config: audit.anchor.type must be \"s3\", got %q", a.Type)
	}
	if a.Bucket == "" {
		return fmt.Errorf("config: audit.anchor.bucket is required")
	}
	if a.Interval != "" {
		d, err := time.ParseDuration(a.Interval)
		if err != nil {
			return fmt.Errorf("config: audit.anchor.interval %q: %w", a.Interval, err)
		}
		if d <= 0 {
			return fmt.Errorf("config: audit.anchor.interval must be > 0, got %q", a.Interval)
		}
	}
	if a.RetainDays < 0 {
		return fmt.Errorf("config: audit.anchor.retain_days must be >= 0")
	}
	return nil
}

// validateOTel checks the opt-in tracing block (ADR-011): endpoint is required,
// protocol must be http/grpc (empty = http), and sample_ratio (when set) must be
// in [0,1]. nil block (tracing off) is valid.
func validateOTel(o *OTelConfig) error {
	if o == nil {
		return nil
	}
	if o.Endpoint == "" {
		return fmt.Errorf("config: otel.endpoint is required when the otel block is present")
	}
	switch o.Protocol {
	case "", "http", "grpc":
	default:
		return fmt.Errorf("config: otel.protocol must be \"http\" or \"grpc\", got %q", o.Protocol)
	}
	if o.SampleRatio != nil && (*o.SampleRatio < 0 || *o.SampleRatio > 1) {
		return fmt.Errorf("config: otel.sample_ratio must be in [0,1], got %v", *o.SampleRatio)
	}
	return nil
}

// ResolveProviders resolves every provider's secret ref into ProviderConfig.APIKey,
// in place. It is the ONLY provider-secret resolution path — both the back-compat
// Load and the DB-overlay assembly (ADR-008) call it, so inline-rejection and
// env/file rules stay in one place. An unresolvable ref (unset env / unreadable
// file) is an error.
func ResolveProviders(cfg *Config) error {
	for name, p := range cfg.Providers {
		secret, err := ResolveSecretRef(p.APIKeyRef)
		if err != nil {
			return fmt.Errorf("config: provider %q secret: %w", name, err)
		}
		p.APIKey = secret
		if p.AuthHeader != "" {
			// auth_header only has an effect on the anthropic provider
			// (live.go only injects it into Settings for type=="anthropic");
			// on any other type it would validate but silently do nothing —
			// reject it outright instead of letting an operator believe it
			// took effect.
			if p.Type != "anthropic" {
				return fmt.Errorf("config: provider %q auth_header is only meaningful for type \"anthropic\", got type %q", name, p.Type)
			}
			if p.AuthHeader != "x-api-key" && p.AuthHeader != "bearer" {
				return fmt.Errorf("config: provider %q auth_header must be \"x-api-key\" or \"bearer\", got %q", name, p.AuthHeader)
			}
		}
		if p.GuardrailVersion != "" && p.GuardrailID == "" {
			return fmt.Errorf("config: provider %q guardrail_version set without guardrail_id", name)
		}
		if p.GuardrailVersion != "" && p.GuardrailVersion != "DRAFT" {
			n, err := strconv.Atoi(p.GuardrailVersion)
			if err != nil || n < 1 || strconv.Itoa(n) != p.GuardrailVersion {
				return fmt.Errorf("config: provider %q guardrail_version must be \"\", \"DRAFT\", or a positive integer with no leading zero/sign, got %q", name, p.GuardrailVersion)
			}
		}
		if p.GuardrailID != "" && p.Type != "bedrock" {
			// Same reasoning as auth_header above: only live.go's bedrock
			// Settings block reads these — on any other type they would
			// validate but silently do nothing.
			return fmt.Errorf("config: provider %q guardrail_id is only meaningful for type \"bedrock\", got type %q", name, p.Type)
		}
		cfg.Providers[name] = p
	}
	return nil
}

// validateOIDC enforces the ADR-004 load-time rules when the oidc block is
// present: mandatory issuer (absolute https, no query/fragment/userinfo) and
// client_id, unique group keys, default groups_claim, and — the break-glass
// invariant — no static admin token may be JWT-shaped, because AdminAuth
// routes every JWT-shaped bearer to the OIDC path and a shaped static token
// would lock operators out during an IdP outage. The shape check is
// adminauth.IsOIDCBearerShape, the SAME function the middleware routes with.
func validateOIDC(aa *AdminAuth) error {
	o := aa.OIDC
	if o == nil {
		return nil
	}
	if o.ClientID == "" {
		return fmt.Errorf("config: oidc.client_id is required (it is the expected token audience)")
	}
	u, err := url.Parse(o.Issuer)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return fmt.Errorf("config: oidc.issuer must be an absolute https URL without query/fragment/userinfo, got %q", o.Issuer)
	}
	if o.GroupsClaim == "" {
		o.GroupsClaim = "groups"
	}
	seen := map[string]bool{}
	for _, m := range o.GroupMappings {
		if seen[m.Group] {
			return fmt.Errorf("config: oidc.group_mappings has duplicate group %q", m.Group)
		}
		seen[m.Group] = true
	}
	for i, tok := range aa.Tokens {
		if adminauth.IsOIDCBearerShape(tok) {
			return fmt.Errorf("config: admin token_refs[%d] resolves to a JWT-shaped value; with oidc enabled it would be routed to the OIDC path and break the break-glass invariant — use an opaque token", i)
		}
	}
	return nil
}

// ResolveSecretRef resolves an env/file secret ref to its value (exported so the
// DB-overlay path resolves DB-sourced provider refs through the same code).
func ResolveSecretRef(ref *SecretRef) (string, error) {
	if ref == nil {
		return "", nil
	}
	switch {
	case ref.Env != "":
		v := os.Getenv(ref.Env)
		if v == "" {
			return "", fmt.Errorf("env %s is empty", ref.Env)
		}
		return v, nil
	case ref.File != "":
		b, err := os.ReadFile(ref.File)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	default:
		return "", fmt.Errorf("empty secret ref")
	}
}
