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
	if err := s.SetModel(ctx, "claude", targets); err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	got, err := s.ListModels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got["claude"], targets) {
		t.Fatalf("model targets not round-tripped in order:\n got %+v\nwant %+v", got["claude"], targets)
	}
}

func TestModelSetReplacesAll(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	_ = s.SetModel(ctx, "m", []Target{{Provider: "a", Model: "1"}, {Provider: "b", Model: "2"}, {Provider: "c", Model: "3"}})
	// Replace with a shorter chain — the extra positions must be gone.
	_ = s.SetModel(ctx, "m", []Target{{Provider: "x", Model: "9"}})
	got, _ := s.ListModels(ctx)
	if len(got["m"]) != 1 || got["m"][0].Provider != "x" {
		t.Fatalf("SetModel did not replace-all: %+v", got["m"])
	}
}

func TestModelDelete(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	_ = s.SetModel(ctx, "m", []Target{{Provider: "a", Model: "1"}})
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
	models := map[string][]Target{"m": {{Provider: "p", Model: "claude"}}}
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
	models := map[string][]Target{"m": {{Provider: "p", Model: "claude", API: "converse"}}}
	if _, err := s.Seed(ctx, provs, models); err != nil {
		t.Fatal(err)
	}
	pl, _ := s.ListProviders(ctx)
	ml, _ := s.ListModels(ctx)
	if len(pl) != 1 || len(ml) != 1 {
		t.Fatalf("seed not atomic: providers=%d models=%d", len(pl), len(ml))
	}
	if ml["m"][0].API != "converse" {
		t.Fatalf("seeded model api lost: %+v", ml["m"])
	}
}
