// Package config loads inferplane's M2 configuration subset. Secrets are only
// referenced (env/file/secret), never inline — an inline api_key is rejected
// at load (design doc §7).
package config

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/inferplane/inferplane/internal/adminauth"
)

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
}

type Target struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	// API selects the Bedrock call path for this target (M4):
	// invoke_model|converse|mantle. Empty means default routing.
	API string `json:"api,omitempty"`
}

type ModelConfig struct {
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

// AuditSink configures one audit output: "stdout" or "file" (with Path).
type AuditSink struct {
	Type string `json:"type"`
	Path string `json:"path,omitempty"`
}

// AuditBuffer is the disk-backed WAL location for buffer_then_block.
type AuditBuffer struct {
	Path string `json:"path"`
}

type AuditConfig struct {
	FailureMode string      `json:"failure_mode"` // buffer_then_block (default)
	Buffer      AuditBuffer `json:"buffer"`
	Sinks       []AuditSink `json:"sinks"`
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

type Config struct {
	Server    ServerConfig              `json:"server"`
	Providers map[string]ProviderConfig `json:"providers"`
	Models    map[string]ModelConfig    `json:"models"`
	KeyStore  KeyStoreConfig            `json:"key_store"`
	Audit     AuditConfig               `json:"audit"`
	Teams     map[string]TeamConfig     `json:"teams"`
	Pricing   PricingConfig             `json:"pricing"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// Reject inline secrets before structured parse: any provider object with
	// a literal "api_key" key is a config error (§7).
	var probe struct {
		Providers map[string]map[string]json.RawMessage `json:"providers"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	for name, p := range probe.Providers {
		if _, bad := p["api_key"]; bad {
			return nil, fmt.Errorf("config: provider %q has inline api_key; use api_key_ref (§7)", name)
		}
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	for name, p := range cfg.Providers {
		secret, err := resolveSecret(p.APIKeyRef)
		if err != nil {
			return nil, fmt.Errorf("config: provider %q secret: %w", name, err)
		}
		p.APIKey = secret
		cfg.Providers[name] = p
	}
	for i := range cfg.Server.AdminAuth.TokenRefs {
		ref := cfg.Server.AdminAuth.TokenRefs[i]
		tok, err := resolveSecret(&ref)
		if err != nil {
			return nil, fmt.Errorf("config: admin token: %w", err)
		}
		cfg.Server.AdminAuth.Tokens = append(cfg.Server.AdminAuth.Tokens, tok)
	}
	if err := validateOIDC(&cfg.Server.AdminAuth); err != nil {
		return nil, err
	}
	return &cfg, nil
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

func resolveSecret(ref *SecretRef) (string, error) {
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
