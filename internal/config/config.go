// Package config loads inferplane's M2 configuration subset. Secrets are only
// referenced (env/file/secret), never inline — an inline api_key is rejected
// at load (design doc §7).
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
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
type AdminAuth struct {
	TokenRefs []SecretRef `json:"token_refs,omitempty"`
	Tokens    []string    `json:"-"` // resolved at load
}

type ServerConfig struct {
	Listen      string    `json:"listen"`
	AdminListen string    `json:"admin_listen"`
	DrainGrace  string    `json:"drain_grace"`
	AdminAuth   AdminAuth `json:"admin_auth"`
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

type Config struct {
	Server    ServerConfig              `json:"server"`
	Providers map[string]ProviderConfig `json:"providers"`
	Models    map[string]ModelConfig    `json:"models"`
	KeyStore  KeyStoreConfig            `json:"key_store"`
	Audit     AuditConfig               `json:"audit"`
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
	return &cfg, nil
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
