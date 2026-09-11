package main

import "testing"

func TestDurableBudgetModeRequiresPolicyDatabaseAndMachineToken(t *testing.T) {
	for _, tc := range []struct {
		mode, dsn, token string
		good             bool
	}{
		{"", "", "", true}, {"false", "", "", true}, {"true", "postgres://example", "machine", true},
		{"maybe", "postgres://example", "machine", false}, {"true", "", "machine", false}, {"true", "postgres://example", "", false},
	} {
		on, err := durableBudgetMode(tc.mode, tc.dsn, tc.token)
		if (err == nil) != tc.good || (err == nil && on != (tc.mode == "true")) {
			t.Fatalf("%+v: enabled=%v error=%v", tc, on, err)
		}
	}
}
