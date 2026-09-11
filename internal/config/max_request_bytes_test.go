package config

import (
	"strings"
	"testing"
	"time"
)

// server.max_request_bytes (C9): negative rejected, zero defaults to 64 MiB,
// a positive value passes through unchanged — validateBodyLog's posture.
func TestValidateServerMaxRequestBytes(t *testing.T) {
	cases := []struct {
		name    string
		in      int64
		want    int64
		wantErr bool
	}{
		{"negative rejected", -1, 0, true},
		{"zero defaults", 0, 64 << 20, false},
		{"positive passes through", 1 << 20, 1 << 20, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := ServerConfig{MaxRequestBytes: tc.in}
			err := validateServer(&s)
			if tc.wantErr {
				if err == nil {
					t.Fatal("want an error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if s.MaxRequestBytes != tc.want {
				t.Fatalf("MaxRequestBytes = %d, want %d", s.MaxRequestBytes, tc.want)
			}
		})
	}
}

// models.<name>.context_window: negative rejected by the shared per-model
// validation walk (ValidateModelAliases), zero/positive pass through.
func TestValidateModelContextWindow(t *testing.T) {
	bad := map[string]ModelConfig{"m": {Targets: []Target{{Provider: "p", Model: "u"}}, ContextWindow: -1}}
	if err := ValidateModelAliases(bad); err == nil {
		t.Fatal("negative context_window must be rejected")
	}
	ok := map[string]ModelConfig{"m": {Targets: []Target{{Provider: "p", Model: "u"}}, ContextWindow: 872000}}
	if err := ValidateModelAliases(ok); err != nil {
		t.Fatalf("positive context_window rejected: %v", err)
	}
}

// control_plane.require_sync / max_policy_age (review/fable5 §08 B2/B3):
// max_policy_age needs require_sync (a silently ignored knob is refused),
// must parse as a positive duration, and lands in MaxPolicyAgeDuration.
func TestValidateControlPlaneRequireSync(t *testing.T) {
	mk := func(rs bool, age string) *Config {
		return &Config{ControlPlane: &ControlPlaneConfig{URL: "https://cp.example:7601", RequireSync: rs, MaxPolicyAge: age}}
	}
	if err := validateControlPlane(mk(false, "10m")); err == nil || !strings.Contains(err.Error(), "requires control_plane.require_sync") {
		t.Fatalf("max_policy_age without require_sync must be rejected, got %v", err)
	}
	if err := validateControlPlane(mk(true, "soon")); err == nil {
		t.Fatal("unparseable max_policy_age must be rejected")
	}
	if err := validateControlPlane(mk(true, "-5m")); err == nil {
		t.Fatal("non-positive max_policy_age must be rejected")
	}
	cfg := mk(true, "10m")
	if err := validateControlPlane(cfg); err != nil {
		t.Fatalf("valid require_sync + max_policy_age rejected: %v", err)
	}
	if cfg.ControlPlane.MaxPolicyAgeDuration != 10*time.Minute {
		t.Fatalf("MaxPolicyAgeDuration = %v, want 10m", cfg.ControlPlane.MaxPolicyAgeDuration)
	}
	if err := validateControlPlane(mk(true, "")); err != nil {
		t.Fatalf("require_sync alone must be valid: %v", err)
	}
}
