// Package config loads inferplane's M2 configuration subset. Secrets are only
// referenced (env/file/secret), never inline — an inline api_key is rejected
// at load (design doc §7).
package config

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

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
	// ContextWindow is the model's total context limit in TOKENS
	// (input + output), operator-declared. 0 (unset) = unknown: no gateway
	// pre-flight and no exposure. When set, the gateway (a) advertises it in
	// GET /v1/models (both wire shapes) so context-aware clients can size
	// their windows instead of assuming a default, and (b) fast-fails an
	// obviously oversized generation request with a CLEAR 400 naming the
	// limit — the upstream's own rejection reaches the client as an opaque
	// scrubbed ValidationException (providers/bedrock/errors.go never echoes
	// upstream text). Declared per public model name; alias lookups resolve
	// to it via router.ContextWindow.
	ContextWindow int64 `json:"context_window,omitempty"`
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
	Issuer        string          `json:"issuer"`
	ClientID      string          `json:"client_id"`
	GroupsClaim   string          `json:"groups_claim,omitempty"` // default "groups"; top-level claim, no traversal
	AdminGroups   []string        `json:"admin_groups,omitempty"`
	GroupMappings []GroupMapping  `json:"group_mappings,omitempty"`
	LoginOrigins  []string        `json:"login_origins,omitempty"`
	CLILogin      *CLILoginConfig `json:"cli_login,omitempty"`
}

// CLILoginConfig opts in to `mayu login` (ADR-028): a data-plane
// endpoint that trades a verified ID token for a short-lived gateway virtual
// key, so a developer never copies a long-lived ik_... key by hand. ClientID
// MUST differ from OIDCConfig.ClientID — the console SPA's public client is
// secretless and registering a CLI loopback redirect on it would let any
// local process complete a code flow with the console's audience (P2 gate).
// KeyTTL bounds how long the minted key lives; the CLI cannot request a
// longer one — a client-supplied TTL would make "short-lived" a false claim.
type CLILoginConfig struct {
	Enabled  bool   `json:"enabled"`
	ClientID string `json:"client_id"`
	KeyTTL   string `json:"key_ttl,omitempty"` // duration string, default "8h", clamped [15m, 24h]
}

// KeyTTLDuration parses KeyTTL, defaulting to 8h when unset. validateOIDC
// already normalizes and range-checks KeyTTL at load time; this parse is
// cheap and kept independently correct rather than trusting that mutation.
func (c *CLILoginConfig) KeyTTLDuration() time.Duration {
	if c == nil || c.KeyTTL == "" {
		return 8 * time.Hour
	}
	d, err := time.ParseDuration(c.KeyTTL)
	if err != nil {
		return 8 * time.Hour
	}
	return d
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
	// MaxRequestBytes bounds every data-plane request body (C9 — before it,
	// every ingress did an unbounded io.ReadAll and one oversized body could
	// OOM the gateway). 0 (unset) defaults to 64 MiB at load; negative is a
	// config error. Distinct from audit.log_bodies.max_body_bytes
	// (BodyLogConfig.MaxBodyBytes), which caps a per-record CAPTURED body,
	// not the ingress read.
	MaxRequestBytes int64 `json:"max_request_bytes,omitempty"`
}

// defaultMaxRequestBytes is server.max_request_bytes' unset default. 64 MiB
// comfortably covers the largest legitimate agent payloads (1M-token contexts,
// base64 images/PDFs in messages) while still bounding a hostile body.
const defaultMaxRequestBytes = 64 << 20

// validateServer normalizes ServerConfig.MaxRequestBytes: negative is a
// config error, zero (unset) defaults to defaultMaxRequestBytes, mutated in
// place so every reader of cfg.Server.MaxRequestBytes sees the resolved
// value (same posture as validateBodyLog's MaxBodyBytes).
func validateServer(s *ServerConfig) error {
	if s.MaxRequestBytes < 0 {
		return fmt.Errorf("config: server.max_request_bytes must be >= 0")
	}
	if s.MaxRequestBytes == 0 {
		s.MaxRequestBytes = defaultMaxRequestBytes
	}
	return nil
}

// KeyStoreConfig selects the virtual-key backend. Only "sqlite" exists — Type
// is parsed but currently IGNORED (gateway.go always calls OpenSQLite); a
// Postgres backend is design-only (ADR-013), not implemented. Setting
// "postgres" today silently yields SQLite with no error.
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

// BudgetConfig is a team's spend ceiling. USDPerMonth/USDPerDay are human USD
// floats in config, converted to µUSD at use; each is 0 = not limited on that
// dimension, and both can be set at once (a team capped at $50/day AND
// $1000/month). OnExceeded (block|warn) governs BOTH windows — there is
// deliberately one knob, not two, because the policy-document channel that can
// express "day soft, month hard" is a later phase.
type BudgetConfig struct {
	USDPerMonth float64 `json:"usd_per_month"` // converted to µUSD at use
	USDPerDay   float64 `json:"usd_per_day"`   // converted to µUSD at use
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

type VirtualKeyConfig struct {
	Team              string            `json:"team"`
	KeyRef            *SecretRef        `json:"key_ref"`
	AllowedModels     []string          `json:"allowed_models"`
	RPM               int64             `json:"rpm,omitempty"`
	TPM               int64             `json:"tpm,omitempty"`
	BudgetUSDPerMonth float64           `json:"budget_usd_per_month,omitempty"`
	BudgetUSDPerDay   float64           `json:"budget_usd_per_day,omitempty"`
	Owner             string            `json:"owner,omitempty"`
	Metadata          map[string]string `json:"metadata,omitempty"`
	Key               string            `json:"-"`
}

// RateConfig holds per-MTok rates as human USD floats in config, converted to
// µUSD-per-MTok int64 at load.
type RateConfig struct {
	InputPerMTok        float64 `json:"input_per_mtok"`
	OutputPerMTok       float64 `json:"output_per_mtok"`
	CacheReadPerMTok    float64 `json:"cache_read_per_mtok"`
	CacheWrite5mPerMTok float64 `json:"cache_write_5m_per_mtok"`
	CacheWrite1hPerMTok float64 `json:"cache_write_1h_per_mtok"`
	// Free declares a genuinely zero-cost model. It is the ONLY way to write a
	// 0/0 override: without it, both rates being zero is a load error, because
	// 0 is what an unfinished placeholder looks like and an unfinished
	// placeholder must not bill as "free" (see validatePricing).
	Free bool `json:"free,omitempty"`
}

// PricingConfig configures cost computation: on_missing policy (allow|block)
// and per-(provider,model) rate overrides. Version labels the rate table so a
// chargeback dispute can be pinned to the rates that priced it — it lands in
// every audit record's cost.pricing_version (ADR-030; the field used to be the
// hardcoded string "bundled" even for fully-overridden tables).
type PricingConfig struct {
	OnMissing string                           `json:"on_missing"` // allow|block
	Version   string                           `json:"version"`    // free-form label, e.g. "2026-07-bedrock"; default "unversioned"
	Overrides map[string]map[string]RateConfig `json:"overrides"`  // provider → model → rate
}

// validatePricing rejects an unrecognized on_missing value, and a rate override
// that declares no price at all.
//
// The on_missing check: before it, a typo like "blcok" — or "BLOCK" — silently
// fell back to allow, so an operator who believed unpriced traffic was refused
// was in fact serving it free (ADR-030).
//
// The 0/0 check closes the same class of hole one level down. An override of
// `{"input_per_mtok": 0, "output_per_mtok": 0}` is the natural way to write a
// fill-in-the-blank placeholder, and it used to be accepted as a REAL rate: it
// satisfied HasRate, passed `mayu pricing check`, booted under
// `on_missing: "block"`, passed the runtime guard, and settled at 0 uUSD with
// missing=FALSE — which is precisely how the audit record encodes a genuinely
// free model. So an unfinished placeholder was indistinguishable from a
// deliberate zero. 0 means unpriced; free needs `"free": true`.
//
// A LOAD ERROR rather than a warning, matching the two nearest precedents (an
// unrecognized on_missing value, and an unknown budget_timezone): a money
// control that is silently wrong is worse than a refused boot.
//
// Only BOTH rates being zero is refused. A single-sided zero is unusual but not
// provably wrong — a provider could bill output only — so it loads.
func validatePricing(p PricingConfig) error {
	switch p.OnMissing {
	case "", "allow", "block":
	default:
		return fmt.Errorf("config: pricing.on_missing must be \"allow\" or \"block\", got %q", p.OnMissing)
	}
	// Sorted so a config with several offenders always names the same one
	// first: a load error that moves between boots is not reproducible.
	for _, provider := range sortedKeys(p.Overrides) {
		models := p.Overrides[provider]
		for _, model := range sortedKeys(models) {
			rc := models[model]
			if rc.InputPerMTok == 0 && rc.OutputPerMTok == 0 && !rc.Free {
				return fmt.Errorf("config: pricing.overrides[%q][%q]: 0 means unpriced, not free; fill in real rates or set \"free\": true", provider, model)
			}
		}
	}
	return nil
}

// sortedKeys returns m's keys in ascending order, for deterministic validation
// error messages over Go's randomized map iteration.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// validateBudgetTimezone resolves budget_timezone into cfg.BudgetLoc, failing
// the load on an unknown zone. "Local" is refused on purpose: it resolves to
// whatever the host's TZ happens to be, so the same config would put the daily
// money boundary at a different instant in a developer's shell than in a
// distroless container (where TZ is unset and Local is UTC) — a silent,
// environment-dependent money control. Name the zone explicitly instead.
func validateBudgetTimezone(cfg *Config) error {
	name := strings.TrimSpace(cfg.BudgetTimezone)
	if name == "" || name == "UTC" {
		cfg.BudgetLoc = time.UTC
		return nil
	}
	if name == "Local" {
		return fmt.Errorf("config: budget_timezone must be an explicit IANA zone name (e.g. \"Asia/Seoul\"), not \"Local\" — Local depends on the host TZ and would move the daily budget boundary between environments")
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return fmt.Errorf("config: budget_timezone %q: %w", name, err)
	}
	cfg.BudgetLoc = loc
	return nil
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
	Server              ServerConfig               `json:"server"`
	Providers           map[string]ProviderConfig  `json:"providers"`
	Models              map[string]ModelConfig     `json:"models"`
	KeyStore            KeyStoreConfig             `json:"key_store"`
	ProviderStore       *ProviderStoreConfig       `json:"provider_store,omitempty"`
	Audit               AuditConfig                `json:"audit"`
	Teams               map[string]TeamConfig      `json:"teams"`
	Pricing             PricingConfig              `json:"pricing"`
	Plugins             []PluginConfig             `json:"plugins,omitempty"`
	OTel                *OTelConfig                `json:"otel,omitempty"`
	Probe               ProbeConfig                `json:"probe,omitempty"`
	Analytics           AnalyticsConfig            `json:"analytics,omitempty"`
	BudgetAlerts        *BudgetAlertsConfig        `json:"budget_alerts,omitempty"`
	ProviderHealthCheck *ProviderHealthCheckConfig `json:"provider_health_check,omitempty"`
	VirtualKeys         []VirtualKeyConfig         `json:"virtual_keys,omitempty"`
	// ModelFallbacks maps a requested (possibly unconfigured) model name to a
	// configured model name/alias to serve in its place — e.g.
	// {"claude-opus-5": "claude-opus-4-8"} keeps a hardcoded client version
	// working before the operator adds the new model. One hop only, same
	// posture as model aliases: a target must not itself be a map key.
	ModelFallbacks map[string]string `json:"model_fallbacks,omitempty"`
	// ModelFallbackFamily enables the default same-family fallback heuristic
	// (an unconfigured "claude-opus-5" falls back to the highest configured
	// "claude-opus-*" version below it) for models with no explicit entry in
	// ModelFallbacks. Nil (absent) means enabled; explicit false disables it.
	ModelFallbackFamily *bool `json:"model_fallback_family,omitempty"`
	// Policies lists local GovernancePolicy document paths (files or
	// directories of *.yaml/*.yml/*.json — the api/v1alpha1 schema, ADR-032/
	// ADR-033). Loaded at boot (boot fails on an invalid document) and
	// watched for changes at runtime (save → applied within seconds; a bad
	// edit keeps the previous set serving). Standalone mode's local policy
	// channel — the same documents inferplaned will distribute. Mutually
	// exclusive with ControlPlane: one policy source at a time.
	Policies []string `json:"policies,omitempty"`
	// ControlPlane connects this data plane to inferplaned (ADR-034):
	// policies, budget leases, and rejection reporting flow over one
	// heartbeat. Mutually exclusive with Policies.
	ControlPlane *ControlPlaneConfig `json:"control_plane,omitempty"`
	// BudgetTimezone is the IANA zone name the CALENDAR-DAY budget window
	// anchors its midnight to (e.g. "Asia/Seoul"). Empty = "UTC", which is
	// byte-for-byte the behavior every budget window had before this key
	// existed. Validated with time.LoadLocation at load: an unknown zone is a
	// LOAD ERROR, not a warning, because a money control silently anchored to
	// the wrong midnight is worse than a refused boot (same logic as
	// pricing.on_missing's typo rejection). The binaries import
	// _ "time/tzdata", so a named zone resolves inside distroless/static too.
	BudgetTimezone string `json:"budget_timezone,omitempty"`
	// BudgetLoc is the RESOLVED location, filled at load. Tagged "-" so a
	// config file can never set it directly — same posture as
	// ProviderConfig.APIKey and AdminAuth.Tokens.
	BudgetLoc *time.Location `json:"-"`
}

// ControlPlaneConfig is the data plane's inferplaned connection (ADR-034).
type ControlPlaneConfig struct {
	// URL is inferplaned's base URL (e.g. "https://inferplaned.infra:7601").
	URL string `json:"url"`
	// TokenRef resolves the shared bearer token — referenced, never inline
	// (§7), same posture as every other secret. Optional only for loopback
	// control planes.
	TokenRef *SecretRef `json:"token_ref,omitempty"`
	// Token is the resolved secret; never serialized.
	Token string `json:"-"`
	// BrokerTokenRef resolves the DEDICATED credential-broker bearer token
	// (ADR-040 decision 1) — referenced, never inline (§7). Distinct from
	// TokenRef on purpose: the heartbeat token sits in env on every node and
	// grants policy reads, so if the SAME token also minted portable AWS
	// credentials, compromising any one node's env would yield Bedrock access
	// WITHOUT mayu — re-opening the exact bypass brokering exists to close.
	// Required by a bedrock provider with auth.mode "broker".
	BrokerTokenRef *SecretRef `json:"broker_token_ref,omitempty"`
	// BrokerToken is the resolved broker secret; never serialized.
	BrokerToken string `json:"-"`
	// Dataplane is this proxy's stable instance id; defaults to the
	// hostname plus a boot-time suffix when empty.
	Dataplane string `json:"dataplane,omitempty"`
	// RequireSync flips this data plane from fail-open to fail-closed on
	// control-plane reachability (review/fable5 §08 B2/B3). Default false
	// keeps today's posture: boot serves ungoverned until the first
	// heartbeat succeeds, and a lost control plane keeps the last-applied
	// policy set forever. With true, governed data-plane requests are refused
	// with 503 (and /readyz reports not-ready) until the first successful
	// sync — and, when MaxPolicyAge is also set, again whenever the last
	// successful sync is older than that. count_tokens is never gated (the
	// never-non-200 invariant). A node operator can still firewall the
	// control plane, but then gets NO service instead of ungoverned service.
	RequireSync bool `json:"require_sync,omitempty"`
	// MaxPolicyAge is a Go duration ("10m"); the last-applied policy set is
	// treated as expired once the last successful sync is older than this.
	// Only meaningful with RequireSync (rejected without it — a silently
	// ignored knob is the failure mode this repo refuses). Empty = policies
	// never expire (leases still do, as before).
	MaxPolicyAge string `json:"max_policy_age,omitempty"`
	// MaxPolicyAgeDuration is the parsed MaxPolicyAge; never serialized.
	MaxPolicyAgeDuration time.Duration `json:"-"`
}

// FallbackFamilyEnabled reports whether the family fallback heuristic is on
// (default: yes).
func (c *Config) FallbackFamilyEnabled() bool {
	return c.ModelFallbackFamily == nil || *c.ModelFallbackFamily
}

// BudgetLocation returns the resolved budget timezone, UTC when unset — the
// same nil-means-UTC default budget.Window carries, so a config that never
// mentions budget_timezone produces the pre-existing behavior exactly.
func (c *Config) BudgetLocation() *time.Location {
	if c.BudgetLoc == nil {
		return time.UTC
	}
	return c.BudgetLoc
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

// ProviderHealthCheckConfig enables periodic background health probing of
// registered providers (ADR-014 deferred item). Absent (nil) -> probing off,
// the v1 on-demand-only default (POST /admin/providers/test is unaffected).
type ProviderHealthCheckConfig struct {
	Interval string `json:"interval,omitempty"` // Go duration; no default -- required when the block is present
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
		BudgetAlerts map[string]json.RawMessage   `json:"budget_alerts"`
		VirtualKeys  []map[string]json.RawMessage `json:"virtual_keys"`
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
	for i, vk := range probe.VirtualKeys {
		if _, bad := vk["key"]; bad {
			return nil, fmt.Errorf("config: virtual_keys[%d] has inline key; use key_ref (§7)", i)
		}
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
	if err := validateServer(&cfg.Server); err != nil {
		return nil, err
	}
	if err := validatePricing(cfg.Pricing); err != nil {
		return nil, err
	}
	if err := validateBudgetTimezone(&cfg); err != nil {
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
	if err := validateProviderHealthCheck(cfg.ProviderHealthCheck); err != nil {
		return nil, err
	}
	if err := validateModelAliases(cfg.Models); err != nil {
		return nil, err
	}
	if err := validateModelFallbacks(cfg.Models, cfg.ModelFallbacks); err != nil {
		return nil, err
	}
	if err := validateVirtualKeys(cfg.VirtualKeys); err != nil {
		return nil, err
	}
	for i, p := range cfg.Policies {
		if strings.TrimSpace(p) == "" {
			return nil, fmt.Errorf("config: policies[%d] is empty", i)
		}
	}
	if err := validateControlPlane(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// validateControlPlane checks the ADR-034 connection block: https/http URL,
// referenced (never inline) token, and mutual exclusion with local policy
// files — two authoritative policy sources would need merge semantics
// nobody has defined, so it is rejected outright.
func validateControlPlane(cfg *Config) error {
	cp := cfg.ControlPlane
	if cp == nil {
		return nil
	}
	if len(cfg.Policies) > 0 {
		return fmt.Errorf("config: control_plane and policies are mutually exclusive — the control plane is the policy source once connected")
	}
	u, err := url.Parse(cp.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("config: control_plane.url must be an absolute http(s) URL")
	}
	if cp.MaxPolicyAge != "" {
		if !cp.RequireSync {
			return fmt.Errorf("config: control_plane.max_policy_age requires control_plane.require_sync: true — without the sync requirement a policy expiry has no fail-closed action to take")
		}
		d, err := time.ParseDuration(cp.MaxPolicyAge)
		if err != nil || d <= 0 {
			return fmt.Errorf("config: control_plane.max_policy_age %q must be a positive Go duration (e.g. \"10m\")", cp.MaxPolicyAge)
		}
		cp.MaxPolicyAgeDuration = d
	}
	if cp.TokenRef != nil {
		tok, err := ResolveSecretRef(cp.TokenRef)
		if err != nil {
			return fmt.Errorf("config: control_plane.token_ref: %w", err)
		}
		cp.Token = tok
	}
	if cp.BrokerTokenRef != nil {
		tok, err := ResolveSecretRef(cp.BrokerTokenRef)
		if err != nil {
			return fmt.Errorf("config: control_plane.broker_token_ref: %w", err)
		}
		cp.BrokerToken = tok
	}
	// The ADR-028 distinct-client-id rule, applied to tokens (ADR-040
	// decision 1): a heartbeat-token compromise must stop at policy reads and
	// must NOT also yield credential brokering. Checked unconditionally
	// whenever both refs resolve, not only in broker mode — an operator who
	// points both refs at one secret has already lost the separation the
	// design depends on.
	if cp.BrokerToken != "" && cp.BrokerToken == cp.Token {
		return fmt.Errorf("config: control_plane.broker_token_ref must not resolve to the same value as control_plane.token_ref — a heartbeat-token compromise must not also yield credential brokering (ADR-040)")
	}
	return nil
}

// ValidateModelAliases checks that no model's alias collides with another
// model's name or with another model's alias (one hop only), and validates
// the other per-model scalar fields (context_window >= 0) in the same walk.
// It is the shared guard for both the file-config path (LoadRaw) and the
// providerstore UI-write path (configapi.ParseModelWrite), mirroring
// ValidateSecretRef's role for provider refs.
func ValidateModelAliases(models map[string]ModelConfig) error {
	return validateModelAliases(models)
}

func validateModelAliases(models map[string]ModelConfig) error {
	seen := make(map[string]string)
	for model, mc := range models {
		if mc.ContextWindow < 0 {
			return fmt.Errorf("config: model %q context_window must be >= 0 (tokens; 0 = unknown)", model)
		}
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

// ValidateModelFallbacks is the shared guard for both the file-config path
// (LoadRaw) and the providerstore UI-write path, mirroring
// ValidateModelAliases's role for model aliases.
func ValidateModelFallbacks(models map[string]ModelConfig, fallbacks map[string]string) error {
	return validateModelFallbacks(models, fallbacks)
}

func validateModelFallbacks(models map[string]ModelConfig, fallbacks map[string]string) error {
	known := make(map[string]bool, len(models))
	for name, mc := range models {
		known[name] = true
		for _, alias := range mc.Aliases {
			known[alias] = true
		}
	}
	for requested, served := range fallbacks {
		if served == requested {
			return fmt.Errorf("config: model_fallbacks %q maps to itself", requested)
		}
		if !known[served] {
			return fmt.Errorf("config: model_fallbacks %q targets unconfigured model %q", requested, served)
		}
		if _, chained := fallbacks[served]; chained {
			return fmt.Errorf("config: model_fallbacks %q targets %q, which is itself a fallback key (one hop only)", requested, served)
		}
	}
	return nil
}

func validateVirtualKeys(vks []VirtualKeyConfig) error {
	seenPlaintexts := make(map[string]int)
	for i := range vks {
		if vks[i].Team == "" {
			return fmt.Errorf("config: virtual_keys[%d].team is required", i)
		}
		if vks[i].KeyRef == nil {
			return fmt.Errorf("config: virtual_keys[%d].key_ref is required", i)
		}
		// A negative limit must be rejected, not silently treated as
		// "unlimited" — governance.go only enforces limits > 0, so a
		// negative value here would grant unlimited RPM/TPM/budget instead
		// of erroring, the opposite of a typo'd negative operator intent
		// (same non-negative guard the admin API applies in adminapi/keys.go).
		if vks[i].RPM < 0 {
			return fmt.Errorf("config: virtual_keys[%d].rpm must be >= 0", i)
		}
		if vks[i].TPM < 0 {
			return fmt.Errorf("config: virtual_keys[%d].tpm must be >= 0", i)
		}
		if vks[i].BudgetUSDPerMonth < 0 {
			return fmt.Errorf("config: virtual_keys[%d].budget_usd_per_month must be >= 0", i)
		}
		if vks[i].BudgetUSDPerDay < 0 {
			return fmt.Errorf("config: virtual_keys[%d].budget_usd_per_day must be >= 0", i)
		}
		if err := ValidateSecretRef(vks[i].KeyRef); err != nil {
			return fmt.Errorf("config: virtual_keys[%d].key_ref: %w", i, err)
		}
		plaintext, err := ResolveSecretRef(vks[i].KeyRef)
		if err != nil {
			return fmt.Errorf("config: virtual_keys[%d]: %w", i, err)
		}
		if len(plaintext) < 16 {
			return fmt.Errorf("config: virtual_keys[%d].key_ref resolves to a value under the 16 character minimum", i)
		}
		if len(vks[i].AllowedModels) == 0 {
			return fmt.Errorf("config: virtual_keys[%d].allowed_models is required (use [\"*\"] for all)", i)
		}
		for j, model := range vks[i].AllowedModels {
			if model == "" || strings.Contains(model, ",") || containsControl(model) {
				return fmt.Errorf("config: virtual_keys[%d].allowed_models[%d] is invalid", i, j)
			}
		}
		if prev, ok := seenPlaintexts[plaintext]; ok {
			return fmt.Errorf("config: virtual_keys[%d] resolves to the same plaintext as virtual_keys[%d]", i, prev)
		}
		seenPlaintexts[plaintext] = i
		vks[i].Key = plaintext
	}
	return nil
}

func containsControl(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
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

// validateProviderHealthCheck checks the opt-in provider_health_check block
// (ADR-014 deferred item). nil block (probing off) is valid, but a present
// block must carry a non-empty interval -- an empty string would otherwise
// parse to a zero time.Duration and panic the ticker the worker starts with.
func validateProviderHealthCheck(phc *ProviderHealthCheckConfig) error {
	if phc == nil {
		return nil
	}
	if phc.Interval == "" {
		return fmt.Errorf("config: provider_health_check.interval is required when provider_health_check is present")
	}
	return validateDurationString("provider_health_check.interval", phc.Interval, time.Millisecond)
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
		// auth.mode is only wired into provider Settings for type "bedrock"
		// (internal/live/live.go), so validate the closed set there. Before
		// ADR-040 EVERY unrecognized value fell silently through to the AWS
		// default credential chain, so a typo ("brokre") would have deployed
		// the bypassable local-IAM posture brokering exists to remove —
		// fail-closed invariant #2. Breaking change for typo'd configs only.
		if p.Type == "bedrock" {
			switch p.Auth.Mode {
			case "", "default", "irsa", "pod_identity", "profile", "static", "broker":
			default:
				return fmt.Errorf("config: provider %q auth.mode %q is not a known mode (allowed: default, irsa, pod_identity, profile, static, broker; empty means default)", name, p.Auth.Mode)
			}
			if p.Auth.Mode == "broker" {
				if err := validateBrokerMode(name, cfg.ControlPlane); err != nil {
					return err
				}
			}
		}
		cfg.Providers[name] = p
	}
	return nil
}

// validateBrokerMode checks the preconditions a bedrock provider's
// auth.mode "broker" has on the control-plane block (ADR-040 fail-closed
// invariants #1/#3). It runs from ResolveProviders, i.e. AFTER
// validateControlPlane has resolved the two tokens, so it can rely on
// cp.BrokerToken being populated.
func validateBrokerMode(provider string, cp *ControlPlaneConfig) error {
	if cp == nil {
		return fmt.Errorf("config: provider %q uses auth.mode \"broker\" but no control_plane block is configured — the credential fetcher would have no URL or token, and broker mode must never fall back to the node's own AWS identity (ADR-040)", provider)
	}
	if cp.BrokerTokenRef == nil {
		return fmt.Errorf("config: provider %q uses auth.mode \"broker\" but control_plane.broker_token_ref is not set — credential brokering requires a bearer token distinct from control_plane.token_ref (ADR-040)", provider)
	}
	u, err := url.Parse(cp.URL)
	if err != nil {
		// validateControlPlane already rejected an unparseable URL; belt and
		// braces so this never reads a zero-value scheme as loopback.
		return fmt.Errorf("config: provider %q uses auth.mode \"broker\" but control_plane.url is not a valid URL", provider)
	}
	if u.Scheme != "https" && !isLoopbackHost(u.Hostname()) {
		return fmt.Errorf("config: provider %q uses auth.mode \"broker\" but control_plane.url %q is plain http to a non-loopback host — an STS triplet on the wire in plaintext is a credential grant to the network path; use https (or a loopback control plane) (ADR-040)", provider, cp.URL)
	}
	return nil
}

// isLoopbackHost reports whether host is a loopback literal. "localhost" is
// trusted without resolution, matching cmd/inferplaned's isLoopback and
// cmd/mayu/gateway.go's isLoopbackHost.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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
	seenLoginOrigins := map[string]bool{}
	for i, origin := range o.LoginOrigins {
		u, err := url.Parse(origin)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
			return fmt.Errorf("config: oidc.login_origins[%d]: must be an absolute https URL without query/fragment/userinfo, got %q", i, origin)
		}
		if u.Path != "" && u.Path != "/" {
			return fmt.Errorf("config: oidc.login_origins[%d]: must not contain a path, got %q", i, origin)
		}
		if seenLoginOrigins[origin] {
			return fmt.Errorf("config: oidc.login_origins[%d]: duplicate origin %q", i, origin)
		}
		seenLoginOrigins[origin] = true
	}
	if o.GroupsClaim == "" {
		o.GroupsClaim = "groups"
	}
	if cl := o.CLILogin; cl != nil {
		if cl.ClientID == "" {
			return fmt.Errorf("config: oidc.cli_login.client_id is required")
		}
		if cl.ClientID == o.ClientID {
			return fmt.Errorf("config: oidc.cli_login.client_id must differ from oidc.client_id — the console's public client must not accept a CLI loopback redirect")
		}
		if cl.KeyTTL == "" {
			cl.KeyTTL = "8h"
		}
		ttl, err := time.ParseDuration(cl.KeyTTL)
		if err != nil {
			return fmt.Errorf("config: oidc.cli_login.key_ttl: %w", err)
		}
		if ttl < 15*time.Minute || ttl > 24*time.Hour {
			return fmt.Errorf("config: oidc.cli_login.key_ttl must be between 15m and 24h, got %s", cl.KeyTTL)
		}
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
