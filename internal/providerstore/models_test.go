package providerstore

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestModelSetGetOrdered(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	targets := []Target{
		{Provider: "anthropic-prod", Model: "claude-sonnet-4-6", API: ""},
		{Provider: "bedrock-us", Model: "anthropic.claude-sonnet-4-6-v1:0", API: "converse"},
	}
	if err := s.SetModel(ctx, "claude", ModelRoute{Targets: targets}); err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	got, err := s.ListModels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got["claude"].Targets, targets) {
		t.Fatalf("model targets not round-tripped in order:\n got %+v\nwant %+v", got["claude"].Targets, targets)
	}
}

func TestModelSetReplacesAll(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	_ = s.SetModel(ctx, "m", ModelRoute{Targets: []Target{{Provider: "a", Model: "1"}, {Provider: "b", Model: "2"}, {Provider: "c", Model: "3"}}})
	// Replace with a shorter chain — the extra positions must be gone.
	_ = s.SetModel(ctx, "m", ModelRoute{Targets: []Target{{Provider: "x", Model: "9"}}})
	got, _ := s.ListModels(ctx)
	if len(got["m"].Targets) != 1 || got["m"].Targets[0].Provider != "x" {
		t.Fatalf("SetModel did not replace-all: %+v", got["m"])
	}
}

func TestModelDelete(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	_ = s.SetModel(ctx, "m", ModelRoute{Targets: []Target{{Provider: "a", Model: "1"}}})
	if err := s.DeleteModel(ctx, "m"); err != nil {
		t.Fatalf("DeleteModel: %v", err)
	}
	got, _ := s.ListModels(ctx)
	if _, ok := got["m"]; ok {
		t.Fatal("model still present after delete")
	}
	if err := s.DeleteModel(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteModel(missing) = %v, want ErrNotFound", err)
	}
}

// TestModelAliasesRoundTrip (ADR-021 follow-up): a model's aliases are stored
// and read back alongside its targets.
func TestModelAliasesRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	route := ModelRoute{
		Aliases: []string{"apac.anthropic.claude-sonnet-4-6", "global.claude-sonnet-4-6"},
		Targets: []Target{{Provider: "p", Model: "claude-sonnet-4-6"}},
	}
	if err := s.SetModel(ctx, "claude-sonnet-4-6", route); err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	got, err := s.ListModels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got["claude-sonnet-4-6"].Aliases, route.Aliases) {
		t.Fatalf("aliases not round-tripped: got %+v want %+v", got["claude-sonnet-4-6"].Aliases, route.Aliases)
	}
}

// TestModelSetReplacesAliases: SetModel replace-all also clears stale aliases,
// not just targets — a shrunk alias list must not leave orphaned rows.
func TestModelSetReplacesAliases(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	_ = s.SetModel(ctx, "m", ModelRoute{
		Aliases: []string{"old-alias-1", "old-alias-2"},
		Targets: []Target{{Provider: "p", Model: "x"}},
	})
	_ = s.SetModel(ctx, "m", ModelRoute{
		Aliases: []string{"new-alias"},
		Targets: []Target{{Provider: "p", Model: "x"}},
	})
	got, _ := s.ListModels(ctx)
	if !reflect.DeepEqual(got["m"].Aliases, []string{"new-alias"}) {
		t.Fatalf("stale aliases not replaced: %+v", got["m"].Aliases)
	}
}

// TestModelDeleteRemovesAliases: deleting a model route also removes its
// aliases — an alias must not survive its model.
func TestModelDeleteRemovesAliases(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	_ = s.SetModel(ctx, "m", ModelRoute{
		Aliases: []string{"alias-1"},
		Targets: []Target{{Provider: "p", Model: "x"}},
	})
	if err := s.DeleteModel(ctx, "m"); err != nil {
		t.Fatalf("DeleteModel: %v", err)
	}
	// Re-declaring the alias under a different model must succeed — it must
	// not still be considered "in use" by the deleted model.
	if err := s.SetModel(ctx, "n", ModelRoute{
		Aliases: []string{"alias-1"},
		Targets: []Target{{Provider: "p", Model: "y"}},
	}); err != nil {
		t.Fatalf("alias reuse after delete: %v", err)
	}
}

// TestSeedOnceDurableMarker pins the round-2 CRITICAL: seeding is gated by a
// durable marker, NOT a row count — so deleting every provider does NOT
// resurrect the file topology on the next seed attempt.
func TestSeedOnceDurableMarker(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	seeded, err := s.Seeded(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if seeded {
		t.Fatal("fresh store must report Seeded=false")
	}

	provs := []ProviderRow{{Name: "p", Type: "anthropic", APIKeyRefEnv: "K"}}
	models := map[string]ModelRoute{"m": {Targets: []Target{{Provider: "p", Model: "claude"}}}}
	did, err := s.Seed(ctx, provs, models)
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}
	if !did {
		t.Fatal("first Seed must report it seeded")
	}
	if ok, _ := s.Seeded(ctx); !ok {
		t.Fatal("Seeded must be true after Seed")
	}
	if list, _ := s.ListProviders(ctx); len(list) != 1 {
		t.Fatalf("Seed did not import providers: %d", len(list))
	}

	// A second Seed is a no-op (already seeded).
	did2, err := s.Seed(ctx, []ProviderRow{{Name: "q", Type: "anthropic"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if did2 {
		t.Fatal("second Seed must NOT re-seed")
	}

	// Operator deletes every provider — Seeded stays true (no resurrection).
	_ = s.DeleteProvider(ctx, "p")
	if list, _ := s.ListProviders(ctx); len(list) != 0 {
		t.Fatalf("expected 0 providers, got %d", len(list))
	}
	if ok, _ := s.Seeded(ctx); !ok {
		t.Fatal("Seeded must remain true after all providers deleted (no resurrection)")
	}
	did3, _ := s.Seed(ctx, provs, models)
	if did3 {
		t.Fatal("Seed after delete-all must NOT resurrect the file topology")
	}
	if list, _ := s.ListProviders(ctx); len(list) != 0 {
		t.Fatalf("resurrection: %d providers reappeared", len(list))
	}
}

// TestSeedAtomic: a Seed writes providers AND model_targets together; both are
// present after one Seed call (single transaction, all-or-nothing).
func TestSeedAtomic(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	provs := []ProviderRow{{Name: "p", Type: "anthropic"}}
	models := map[string]ModelRoute{"m": {Targets: []Target{{Provider: "p", Model: "claude", API: "converse"}}}}
	if _, err := s.Seed(ctx, provs, models); err != nil {
		t.Fatal(err)
	}
	pl, _ := s.ListProviders(ctx)
	ml, _ := s.ListModels(ctx)
	if len(pl) != 1 || len(ml) != 1 {
		t.Fatalf("seed not atomic: providers=%d models=%d", len(pl), len(ml))
	}
	if ml["m"].Targets[0].API != "converse" {
		t.Fatalf("seeded model api lost: %+v", ml["m"])
	}
}

// TestSeedPreservesAliases: Seed also imports a model's aliases, not just its
// targets.
func TestSeedPreservesAliases(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	provs := []ProviderRow{{Name: "p", Type: "anthropic"}}
	models := map[string]ModelRoute{
		"m": {Aliases: []string{"m-alias"}, Targets: []Target{{Provider: "p", Model: "claude"}}},
	}
	if _, err := s.Seed(ctx, provs, models); err != nil {
		t.Fatal(err)
	}
	ml, _ := s.ListModels(ctx)
	if !reflect.DeepEqual(ml["m"].Aliases, []string{"m-alias"}) {
		t.Fatalf("seed lost aliases: %+v", ml["m"])
	}
}
