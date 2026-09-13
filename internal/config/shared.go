package config

import (
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"os"
	"strings"

	"github.com/inferplane/inferplane/internal/adminauth"
)

func (c *Config) SharedGovernance() bool {
	return c != nil && c.GovernanceStore != nil && c.GovernanceStore.Type == "postgres"
}

func validateSharedStores(c *Config) error {
	k := &c.KeyStore
	if err := validateKeyStore(k); err != nil {
		return err
	}
	return validateSharedProfile(c)
}

func validateKeyStore(k *KeyStoreConfig) error {
	switch k.Type {
	case "", "sqlite":
		if k.DSNRef != nil {
			return fmt.Errorf("config: SQLite key_store cannot set dsn_ref")
		}
	case "postgres":
		if k.Path != "" || k.DSNRef == nil {
			return fmt.Errorf("config: Postgres key_store requires dsn_ref and no path")
		}
		if err := ValidateSecretRef(k.DSNRef); err != nil {
			return fmt.Errorf("config: key_store.dsn_ref: %w", err)
		}
		dsn, err := ResolveSecretRef(k.DSNRef)
		if err != nil || strings.TrimSpace(dsn) == "" {
			return fmt.Errorf("config: key_store.dsn_ref cannot be resolved")
		}
		k.DSN = dsn
	default:
		return fmt.Errorf("config: unknown key_store type")
	}
	return nil
}

func validateSharedProfile(c *Config) error {
	if c.GovernanceStore != nil && c.GovernanceStore.Type != "postgres" {
		return fmt.Errorf("config: governance_store type must be postgres")
	}
	if (c.KeyStore.Type == "postgres") != c.SharedGovernance() {
		return fmt.Errorf("config: Postgres key_store and governance_store must be enabled together")
	}
	if !c.SharedGovernance() {
		return nil
	}
	if c.BudgetTimezone != "" && c.BudgetTimezone != "UTC" {
		return fmt.Errorf("config: shared governance requires budget_timezone UTC")
	}
	cp := c.ControlPlane
	if cp == nil || !cp.RequireSync || cp.TokenRef == nil || strings.TrimSpace(cp.Token) == "" ||
		adminauth.IsOIDCBearerShape(cp.Token) {
		return fmt.Errorf("config: shared governance requires control_plane with require_sync and non-JWT machine token")
	}
	if cp.Authority != nil {
		return fmt.Errorf("config: shared governance and node-local control_plane.authority are mutually exclusive")
	}
	u, err := url.Parse(cp.URL)
	if err != nil || u.User != nil || (u.Scheme != "https" && !isLoopbackHost(u.Hostname())) {
		return fmt.Errorf("config: shared governance requires HTTPS or loopback control_plane")
	}
	if c.ProviderStore != nil {
		return fmt.Errorf("config: shared governance requires file-managed topology; per-replica provider_store is unsupported")
	}
	for _, team := range c.Teams {
		if team.Quota.TokensPerMonth != 0 {
			return fmt.Errorf("config: use GovernancePolicy.tokenQuota CalendarMonth for shared monthly token quotas")
		}
		if team.RateLimit.RequestsPerMinute < 0 || team.RateLimit.TokensPerMinute < 0 || team.Quota.TokensPerDay < 0 {
			return fmt.Errorf("config: shared rate/token limits must be nonnegative")
		}
		for _, usd := range []float64{team.Budget.USDPerDay, team.Budget.USDPerMonth} {
			if usd < 0 || math.IsInf(usd, 0) || math.IsNaN(usd) || usd >= float64(math.MaxInt64)/1_000_000 {
				return fmt.Errorf("config: shared monetary limit is out of range")
			}
		}
	}
	return nil
}

// LoadKeyStore reads only key-backend settings for offline management commands;
// it does not require unrelated provider/admin/control-plane credentials.
func LoadKeyStore(path string) (KeyStoreConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return KeyStoreConfig{}, err
	}
	var root struct {
		KeyStore json.RawMessage `json:"key_store"`
	}
	if err := json.Unmarshal(raw, &root); err != nil {
		return KeyStoreConfig{}, fmt.Errorf("config: invalid key store document")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(root.KeyStore, &fields); err != nil {
		return KeyStoreConfig{}, fmt.Errorf("config: key_store is required")
	}
	if _, bad := fields["dsn"]; bad {
		return KeyStoreConfig{}, fmt.Errorf("config: use key_store.dsn_ref instead of inline dsn")
	}
	var cfg KeyStoreConfig
	if err := json.Unmarshal(root.KeyStore, &cfg); err != nil {
		return cfg, fmt.Errorf("config: invalid key_store")
	}
	if err := validateKeyStore(&cfg); err != nil {
		return KeyStoreConfig{}, err
	}
	return cfg, nil
}
