package config

import (
	"encoding/json"
	"strings"
	"testing"
)

const sharedConfigJSON = `{
 "key_store":{"type":"postgres","dsn_ref":{"env":"SHARED_TEST_DSN"}},
 "governance_store":{"type":"postgres"},
 "control_plane":{"url":"https://cp.example.invalid","dataplane":"node","require_sync":true,"token_ref":{"env":"SHARED_TEST_TOKEN"}}
}`

func TestSharedProfileRejectsInvalidBackendAndSecretCombinations(t *testing.T) {
	t.Setenv("SHARED_TEST_DSN", "postgres://test:test@localhost/test")
	t.Setenv("SHARED_TEST_TOKEN", "test-machine-credential")
	cases := []struct {
		name, doc string
		valid     bool
	}{
		{"valid", sharedConfigJSON, true},
		{"inline-dsn", strings.Replace(sharedConfigJSON, `"dsn_ref":{"env":"SHARED_TEST_DSN"}`, `"dsn":"private-password"`, 1), false},
		{"unknown-key", strings.Replace(sharedConfigJSON, `"type":"postgres"`, `"type":"postgress"`, 1), false},
		{"unknown-governance", strings.Replace(sharedConfigJSON, `"governance_store":{"type":"postgres"}`, `"governance_store":{"type":"redis"}`, 1), false},
		{"no-sync", strings.Replace(sharedConfigJSON, `"require_sync":true`, `"require_sync":false`, 1), false},
		{"two-authorities", strings.Replace(sharedConfigJSON, `"require_sync":true`, `"require_sync":true,"authority":{"journal_path":"/private/journal"}`, 1), false},
		{"utc-only", strings.Replace(sharedConfigJSON, `"key_store"`, `"budget_timezone":"Asia/Seoul","key_store"`, 1), false},
		{"private-journal", strings.Replace(sharedConfigJSON, `"key_store"`, `"provider_store":{"type":"sqlite","path":"/private/providers.db"},"key_store"`, 1), false},
		{"key-without-shared", strings.Replace(sharedConfigJSON, `"governance_store":{"type":"postgres"},`, "", 1), false},
		{"shared-without-key", strings.Replace(sharedConfigJSON, `"key_store":{"type":"postgres","dsn_ref":{"env":"SHARED_TEST_DSN"}}`, `"key_store":{"type":"sqlite","path":"/private/keys.db"}`, 1), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := LoadRaw(writeConfig(t, tc.doc))
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
			if err != nil && strings.Contains(err.Error(), "private-password") {
				t.Fatal("inline DSN leaked into error")
			}
			if cfg != nil {
				b, err := json.Marshal(cfg)
				if err != nil || strings.Contains(string(b), "postgres://test:test") {
					t.Fatal("resolved DSN exposed by serialization")
				}
			}
		})
	}
}

func TestKeyManagementConfigResolvesOnlyKeyStore(t *testing.T) {
	t.Setenv("KEY_ONLY_DSN", "postgres://test:fixture@localhost/example")
	doc := `{"key_store":{"type":"postgres","dsn_ref":{"env":"KEY_ONLY_DSN"}},
		"providers":{"ignored":{"api_key_ref":{"env":"ABSENT_PROVIDER"}}},
		"control_plane":{"token_ref":{"env":"ABSENT_CONTROL"}}}`
	cfg, err := LoadKeyStore(writeConfig(t, doc))
	if err != nil || cfg.DSN != "postgres://test:fixture@localhost/example" {
		t.Fatalf("key-only config lost resolved connection: type=%s err=%v", cfg.Type, err)
	}
	raw, _ := json.Marshal(cfg)
	if strings.Contains(string(raw), "fixture") {
		t.Fatal("key-only config serialized its credential")
	}
}
