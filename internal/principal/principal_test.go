package principal

import (
	"context"
	"testing"

	"github.com/inferplane/inferplane/internal/keystore"
)

func TestWithAdminAndAdminFrom(t *testing.T) {
	id := AdminIdentity{Subject: "sub-1", Teams: []string{"alpha"}, IsAdmin: false, AuthMethod: "oidc"}
	ctx := WithAdmin(context.Background(), id)
	got, ok := AdminFrom(ctx)
	if !ok || got.Subject != "sub-1" || got.AuthMethod != "oidc" || len(got.Teams) != 1 {
		t.Fatalf("AdminFrom = %+v, %v", got, ok)
	}
}

func TestAdminFromAbsent(t *testing.T) {
	if _, ok := AdminFrom(context.Background()); ok {
		t.Fatal("AdminFrom on empty ctx must be ok=false")
	}
}

// TestAdminAndDataPlaneKeysAreIndependent pins the separate-context-key
// design: the two planes' identities never shadow each other.
func TestAdminAndDataPlaneKeysAreIndependent(t *testing.T) {
	ctx := With(context.Background(), keystore.Principal{KeyID: "ik_x", Team: "demo"})
	ctx = WithAdmin(ctx, AdminIdentity{Subject: "break-glass", IsAdmin: true, AuthMethod: "break_glass"})

	p, ok := From(ctx)
	if !ok || p.KeyID != "ik_x" {
		t.Fatalf("data-plane principal shadowed: %+v, %v", p, ok)
	}
	a, ok := AdminFrom(ctx)
	if !ok || a.Subject != "break-glass" || !a.IsAdmin {
		t.Fatalf("admin identity shadowed: %+v, %v", a, ok)
	}
}

// TestAdminIdentityEntitled covers the team-entitlement helper used by the
// key handlers (fail-closed: empty identity is entitled to nothing).
func TestAdminIdentityEntitled(t *testing.T) {
	member := AdminIdentity{Subject: "u", Teams: []string{"alpha", "beta"}}
	admin := AdminIdentity{Subject: "root", IsAdmin: true}
	var zero AdminIdentity

	cases := []struct {
		name string
		id   AdminIdentity
		team string
		want bool
	}{
		{"member own team", member, "alpha", true},
		{"member other team", member, "gamma", false},
		{"admin any team", admin, "anything", true},
		{"zero identity nothing", zero, "alpha", false},
		{"member empty team", member, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.id.Entitled(tc.team); got != tc.want {
				t.Fatalf("Entitled(%q) = %v, want %v", tc.team, got, tc.want)
			}
		})
	}
}
