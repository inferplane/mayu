package tier

import (
	"encoding/json"
	"maps"
	"slices"
	"sync"
	"testing"

	"github.com/inferplane/inferplane/internal/policy"
)

type subjectLookup interface {
	GetForSubject(team, user string) map[string]string
	ConstraintsForSubject(team, user string) map[string]string
}

func scopedTable(t *testing.T, body string) (*Table, subjectLookup, []policy.ActiveTier) {
	t.Helper()
	var active []policy.ActiveTier
	if err := json.Unmarshal([]byte(body), &active); err != nil {
		t.Fatal(err)
	}
	tb := NewTable()
	tb.Set(active)
	lookup, ok := any(tb).(subjectLookup)
	if !ok {
		t.Fatal("tier table does not preserve team/user subject selectors")
	}
	return tb, lookup, active
}

func TestTableSubjectScopesAreIsolated(t *testing.T) {
	tb, lookup, active := scopedTable(t, `[
		{"team":"alpha","policy":"team","thresholdPercent":50,"substitute":{"team-model":"team-target"}},
		{"user":"alice","policy":"user","thresholdPercent":50,"substitute":{"user-model":"user-target"}},
		{"team":"alpha","user":"alice","policy":"both","thresholdPercent":50,"substitute":{"both-model":"both-target"}},
		{"team":"beta","user":"bob","policy":"other","thresholdPercent":50,"substitute":{"other-model":"other-target"}}
	]`)
	for _, tc := range []struct {
		team, user string
		want       map[string]string
	}{
		{"alpha", "alice", map[string]string{"team-model": "team-target", "user-model": "user-target", "both-model": "both-target"}},
		{"alpha", "bob", map[string]string{"team-model": "team-target"}},
		{"beta", "alice", map[string]string{"user-model": "user-target"}},
		{"beta", "bob", map[string]string{"other-model": "other-target"}},
		{"gamma", "bob", nil},
		{"alpha", "", map[string]string{"team-model": "team-target"}},
		{"", "alice", map[string]string{"user-model": "user-target"}},
		{"", "", nil},
	} {
		if got := lookup.GetForSubject(tc.team, tc.user); !maps.Equal(got, tc.want) {
			t.Errorf("(%q,%q): got %v, want %v", tc.team, tc.user, got, tc.want)
		}
	}
	if got := tb.Get("alpha"); !maps.Equal(got, map[string]string{"team-model": "team-target"}) {
		t.Fatalf("legacy team lookup inherited a user's tier: %v", got)
	}
	active[0].Substitute["team-model"] = "changed-input"
	copy := lookup.GetForSubject("alpha", "alice")
	copy["team-model"] = "changed-output"
	if got := lookup.GetForSubject("alpha", "alice")["team-model"]; got != "team-target" {
		t.Fatalf("caller mutated subject snapshot: %q", got)
	}
	tb.Set(nil)
	if lookup.GetForSubject("alpha", "alice") != nil {
		t.Fatal("replacement retained old subject tiers")
	}
}

func TestTableSubjectWinnersUsePressureThenPolicyAndRule(t *testing.T) {
	tb, lookup, active := scopedTable(t, `[
		{"team":"alpha","policy":"z-team","rule":"r","thresholdPercent":90,"substitute":{"pressure":"team","tie":"team"}},
		{"user":"alice","policy":"a-user","rule":"z","thresholdPercent":90,"substitute":{"tie":"user-z"}},
		{"user":"alice","policy":"a-user","rule":"a","thresholdPercent":90,"substitute":{"tie":"user-a"}},
		{"team":"alpha","user":"alice","policy":"scoped","rule":"r","thresholdPercent":80,"substitute":{"pressure":"narrower"}}
	]`)
	for i := 0; i < 2; i++ {
		tb.Set(active)
		if got := lookup.GetForSubject("alpha", "alice"); !maps.Equal(got, map[string]string{
			"pressure": "team", "tie": "user-a",
		}) {
			t.Fatalf("order %d changed cross-scope winners: %v", i, got)
		}
		slices.Reverse(active)
	}
}

func TestTableSubjectStrictConflictsCannotBeLoosened(t *testing.T) {
	tb, lookup, active := scopedTable(t, `[
		{"team":"alpha","policy":"team","thresholdPercent":50,"enforceTargets":true,"substitute":{"a":"b","same":"cheap"}},
		{"user":"alice","policy":"user","thresholdPercent":60,"enforceTargets":true,"substitute":{"a":"c","same":"cheap"}},
		{"team":"alpha","user":"alice","policy":"both","thresholdPercent":70,"enforceTargets":true,"substitute":{"a":"b"}},
		{"team":"alpha","user":"alice","policy":"legacy","thresholdPercent":99,"substitute":{"a":"escape","same":"premium"}}
	]`)
	for i := 0; i < 2; i++ {
		tb.Set(active)
		for _, tc := range []struct {
			team, user string
			want       map[string]string
		}{
			{"alpha", "alice", map[string]string{"a": "", "same": "cheap"}},
			{"alpha", "bob", map[string]string{"a": "b", "same": "cheap"}},
			{"beta", "alice", map[string]string{"a": "c", "same": "cheap"}},
			{"beta", "bob", nil},
		} {
			if got := lookup.ConstraintsForSubject(tc.team, tc.user); !maps.Equal(got, tc.want) {
				t.Errorf("order %d (%q,%q): got %v, want %v", i, tc.team, tc.user, got, tc.want)
			}
		}
		slices.Reverse(active)
	}
	if lookup.GetForSubject("alpha", "alice")["a"] != "escape" {
		t.Fatal("legacy winner was mixed into strict constraint evaluation")
	}
	copy := lookup.ConstraintsForSubject("alpha", "alice")
	copy["a"] = "escape"
	if got := lookup.ConstraintsForSubject("alpha", "alice")["a"]; got != "" {
		t.Fatal("caller mutated the strict conflict")
	}
	if tb.Constraints("alpha")["a"] != "b" {
		t.Fatal("legacy team constraint lookup inherited a user's constraint")
	}
	tb.Set(nil)
	if lookup.ConstraintsForSubject("alpha", "alice") != nil {
		t.Fatal("replacement retained old subject constraints")
	}
}

func TestTableSubjectSnapshotsRemainAtomicDuringReplacement(t *testing.T) {
	tb := NewTable()
	makeSnapshot := func(target string) []policy.ActiveTier {
		return []policy.ActiveTier{
			{Team: "alpha", Policy: "team", EnforceTargets: true, Substitute: map[string]string{"team-model": target}},
			{User: "alice", Policy: "user", EnforceTargets: true, Substitute: map[string]string{"user-model": target}},
		}
	}
	first, second := makeSnapshot("first"), makeSnapshot("second")
	tb.Set(first)
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			tb.Set(first)
			tb.Set(second)
		}
	}()
	for _, lookup := range []func(string, string) map[string]string{tb.GetForSubject, tb.ConstraintsForSubject} {
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				got := lookup("alpha", "alice")
				if len(got) != 2 || got["team-model"] != got["user-model"] {
					t.Errorf("subject lookup mixed two snapshots: %v", got)
					return
				}
			}
		}()
	}
	wg.Wait()
}
