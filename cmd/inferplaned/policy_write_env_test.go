package main

import (
	"strings"
	"testing"
)

// INFERPLANED_POLICY_WRITE_TOKEN (review/fable5 §08 B1): unset is allowed
// (writes then fail closed for static bearers); when set it must be a
// distinct, non-JWT-shaped secret from both the heartbeat and broker tokens.
func TestValidatePolicyWriteEnv(t *testing.T) {
	tests := []struct {
		name, token, broker, write string
		wantErr                    string
	}{
		{"unset is allowed", "t", "", "", ""},
		{"distinct write token", "t", "b", "w", ""},
		{"jwt-shaped write token", "t", "", "a.b.c", "JWT-shaped"},
		{"equals heartbeat token", "same", "", "same", "must differ from INFERPLANED_TOKEN"},
		{"equals broker token", "t", "same", "same", "must differ from INFERPLANED_BROKER_TOKEN"},
		{"no heartbeat token, write token set", "", "", "w", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validatePolicyWriteEnv(tc.token, tc.broker, tc.write)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("got %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("got %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}
