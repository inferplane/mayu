package adminauth

import (
	"reflect"
	"strings"
	"testing"
)

func TestIsOIDCBearerShape(t *testing.T) {
	long := strings.Repeat("a", 9*1024)
	cases := []struct {
		name   string
		bearer string
		want   bool
	}{
		{"plain JWT shape", "a.b.c", true},
		{"realistic JWT", "eyJhbGciOiJSUzI1NiIsImtpZCI6IjEifQ.eyJzdWIiOiJ1MSJ9.c2ln", true},
		{"base64url charset with - and _", "a-b._c-d.e_f", true},
		{"two segments", "a.b", false},
		{"four segments", "a.b.c.d", false},
		{"five segments (JWE)", "a.b.c.d.e", false},
		{"empty middle segment", "a..b", false},
		{"leading dot", ".a.b", false},
		{"trailing dot", "a.b.", false},
		{"padded segment", "a=.b.c", false},
		{"whitespace inside", "a.b c.d", false},
		{"non-base64url char", "a.!.c", false},
		{"plus is base64 not base64url", "a.b+c.d", false},
		{"empty string", "", false},
		{"no dots", "opaquestatictoken", false},
		{"over size cap", long + "." + long + "." + long, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsOIDCBearerShape(tc.bearer); got != tc.want {
				t.Fatalf("IsOIDCBearerShape(%q) = %v, want %v", tc.bearer, got, tc.want)
			}
		})
	}
}

func TestResolve(t *testing.T) {
	cfg := MappingConfig{
		AdminGroups: []string{"platform-admins"},
		GroupMappings: []GroupMapping{
			{Group: "team-alpha", Teams: []string{"alpha"}},
			{Group: "team-beta", Teams: []string{"beta", "beta-eu"}},
			{Group: "*", Teams: []string{"sandbox"}},
		},
	}
	cases := []struct {
		name      string
		groups    []string
		wantTeams []string
		wantAdmin bool
		wantOK    bool
	}{
		{"exact match", []string{"team-alpha"}, []string{"alpha", "sandbox"}, false, true},
		{"multi-group union", []string{"team-alpha", "team-beta"}, []string{"alpha", "beta", "beta-eu", "sandbox"}, false, true},
		{"wildcard only", []string{"unmapped-group"}, []string{"sandbox"}, false, true},
		{"admin group", []string{"platform-admins"}, nil, true, true},
		{"admin plus member", []string{"platform-admins", "team-alpha"}, nil, true, true},
		{"empty groups claim", nil, nil, false, false},
		{"dedup teams", []string{"team-beta", "team-beta"}, []string{"beta", "beta-eu", "sandbox"}, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			teams, isAdmin, ok := Resolve(tc.groups, cfg)
			if ok != tc.wantOK || isAdmin != tc.wantAdmin {
				t.Fatalf("Resolve(%v) ok=%v admin=%v, want ok=%v admin=%v", tc.groups, ok, isAdmin, tc.wantOK, tc.wantAdmin)
			}
			if !tc.wantAdmin && tc.wantOK && !reflect.DeepEqual(teams, tc.wantTeams) {
				t.Fatalf("Resolve(%v) teams=%v, want %v", tc.groups, teams, tc.wantTeams)
			}
		})
	}
}

// TestResolveNoMatchNoWildcard pins the banned behavior: with no wildcard
// mapping configured, an unmapped group set must NOT fall back to any team.
func TestResolveNoMatchNoWildcard(t *testing.T) {
	cfg := MappingConfig{GroupMappings: []GroupMapping{{Group: "team-alpha", Teams: []string{"alpha"}}}}
	teams, isAdmin, ok := Resolve([]string{"strangers"}, cfg)
	if ok || isAdmin || teams != nil {
		t.Fatalf("no-match must be (nil,false,false); got (%v,%v,%v) — silent privilege grant is banned", teams, isAdmin, ok)
	}
}
