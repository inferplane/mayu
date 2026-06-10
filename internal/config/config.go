// Package config loads inferplane's M2 configuration subset. Secrets are only
// referenced (env/file/secret), never inline — an inline api_key is rejected
// at load (design doc §7).
package config

import (
	"encoding/json"
	"fmt"
	"os"
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
}

type Target struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

type ModelConfig struct {
	Targets []Target `json:"targets"`
}

type ServerConfig struct {
	Listen      string `json:"listen"`
	AdminListen string `json:"admin_listen"`
	DrainGrace  string `json:"drain_grace"`
}

type Config struct {
	Server    ServerConfig              `json:"server"`
	Providers map[string]ProviderConfig `json:"providers"`
	Models    map[string]ModelConfig    `json:"models"`
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
		return string(b), nil
	default:
		return "", fmt.Errorf("empty secret ref")
	}
}
