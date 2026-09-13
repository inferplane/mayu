package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDurableAuthorityRequiresPersistentUTCReadyProfile(t *testing.T) {
	t.Setenv("CP_BUDGET_TEST_TOKEN", "local-test-machine-token")
	for _, tc := range []struct {
		path, zone string
		ready      bool
		want       bool
	}{
		{"/private/authority.db", "UTC", true, true},
		{"", "UTC", true, false},
		{":memory:", "UTC", true, false},
		{"/private/authority.db", "Asia/Seoul", true, false},
		{"/private/authority.db", "UTC", false, false},
	} {
		c := &Config{BudgetTimezone: tc.zone, ControlPlane: &ControlPlaneConfig{
			URL: "https://cp.example", RequireSync: tc.ready, Dataplane: "stable-node",
			TokenRef:  &SecretRef{Env: "CP_BUDGET_TEST_TOKEN"},
			Authority: &AuthorityConfig{JournalPath: tc.path},
		}}
		err := validateControlPlane(c)
		if (err == nil) != tc.want {
			t.Fatalf("profile=%+v error=%v", tc, err)
		}
	}
}

func TestAuthorityCapabilitiesRequireEncryptedRemoteTransport(t *testing.T) {
	t.Setenv("CP_BUDGET_TEST_TOKEN", "local-test-machine-token")
	c := &Config{ControlPlane: &ControlPlaneConfig{URL: "http://remote.example", Dataplane: "node", RequireSync: true,
		TokenRef: &SecretRef{Env: "CP_BUDGET_TEST_TOKEN"}, Authority: &AuthorityConfig{JournalPath: "/private/journal.db"}}}
	if err := validateControlPlane(c); err == nil {
		t.Fatal("grant capabilities may traverse remote plaintext HTTP")
	}
}

func TestAuthorityRejectsEmptyOrJWTMachineCredential(t *testing.T) {
	for _, token := range []string{" \n", "eyJhbGciOiJIUzI1NiJ9.e30.signature"} {
		t.Run(token, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "token")
			if err := os.WriteFile(path, []byte(token), 0600); err != nil {
				t.Fatal(err)
			}
			c := &Config{ControlPlane: &ControlPlaneConfig{URL: "https://cp.example", Dataplane: "node", RequireSync: true,
				TokenRef: &SecretRef{File: path}, Authority: &AuthorityConfig{JournalPath: "/private/journal.db"}}}
			if err := validateControlPlane(c); err == nil {
				t.Fatal("unusable machine credential accepted")
			}
		})
	}
}
