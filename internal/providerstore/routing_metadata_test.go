package providerstore

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/inferplane/inferplane/internal/config"
)

func TestRoutingMetadataMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE providers (name TEXT PRIMARY KEY,type TEXT NOT NULL,base_url TEXT NOT NULL DEFAULT '',region TEXT NOT NULL DEFAULT '',auth_mode TEXT NOT NULL DEFAULT '',auth_profile TEXT NOT NULL DEFAULT '',api_key_ref_env TEXT NOT NULL DEFAULT '',api_key_ref_file TEXT NOT NULL DEFAULT '');
 CREATE TABLE model_targets (model TEXT,position INTEGER,provider TEXT,model_id TEXT,api TEXT,PRIMARY KEY(model,position));
 INSERT INTO providers (name,type) VALUES ('p','openai');
 INSERT INTO model_targets VALUES ('m',0,'p','upstream','');`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	st, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	p, err := st.GetProvider(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	if p.DataBoundary != "" {
		t.Fatalf("legacy trust changed: %+v", p)
	}
	models, err := st.ListModels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if m := models["m"]; m.ContextWindow != 0 || len(m.Capabilities) != 0 || len(m.Targets) != 1 {
		t.Fatalf("legacy model changed: %+v", m)
	}
}

func TestRoutingMetadataTransactionAndOwnership(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	old := ModelRoute{ContextWindow: 8192, Capabilities: []string{"tools"}, Aliases: []string{"a"}, Targets: []Target{{Provider: "p", Model: "one"}, {Provider: "p", Model: "two"}}}
	if err := st.SetModel(ctx, "m", old); err != nil {
		t.Fatal(err)
	}
	if err := st.SetModel(ctx, "other", ModelRoute{Aliases: []string{"taken"}, Targets: []Target{{Provider: "p", Model: "x"}}}); err != nil {
		t.Fatal(err)
	}
	bad := ModelRoute{ContextWindow: 32768, Capabilities: []string{"vision"}, Aliases: []string{"taken"}, Targets: []Target{{Provider: "p", Model: "replacement"}}}
	if err := st.SetModel(ctx, "m", bad); err == nil {
		t.Fatal("alias collision accepted")
	}
	got, err := st.ListModels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got["m"], old) {
		t.Fatalf("partial transaction: %+v", got["m"])
	}
	got["m"].Capabilities[0] = "vision"
	fresh, err := st.ListModels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	eff := OverlayFrom(&config.Config{}, nil, fresh)
	fresh["m"].Capabilities[0] = "vision"
	if eff.Models["m"].Capabilities[0] != "tools" {
		t.Fatal("overlay aliases input")
	}
	if err := st.SetModel(ctx, "m", ModelRoute{Targets: old.Targets}); err != nil {
		t.Fatal(err)
	}
	fresh, err = st.ListModels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if fresh["m"].ContextWindow != 0 || len(fresh["m"].Capabilities) != 0 {
		t.Fatal("replace failed to clear metadata")
	}
	if err := st.DeleteModel(ctx, "m"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := st.db.QueryRow(`SELECT count(*) FROM model_metadata WHERE model='m'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("metadata orphan: %d %v", n, err)
	}
}

func TestRoutingMetadataStoreValidation(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	if err := st.UpsertProvider(ctx, ProviderRow{Name: "p", Type: "openai", DataBoundary: "local"}); err == nil {
		t.Error("persisted unknown boundary")
	}
	for _, route := range []ModelRoute{{ContextWindow: -1}, {Capabilities: []string{"audio"}}} {
		if err := st.SetModel(ctx, "m", route); err == nil {
			t.Errorf("persisted invalid metadata: %+v", route)
		}
	}
	if _, err := st.Seed(ctx, []ProviderRow{{Name: "seed", Type: "openai", DataBoundary: "local"}}, nil); err == nil {
		t.Error("seed persisted unknown boundary")
	}
	seeded, err := st.Seeded(ctx)
	if err != nil || seeded {
		t.Fatalf("invalid seed committed: %v %v", seeded, err)
	}
}

func TestRoutingMetadataProviderReplaceDeleteAndReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store.db")
	st, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertProvider(ctx, ProviderRow{Name: "p", Type: "openai", DataBoundary: "internal"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetModel(ctx, "m", ModelRoute{ContextWindow: 4096, Capabilities: []string{"reasoning"}, Targets: []Target{{Provider: "p", Model: "m"}}}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	p, err := st.GetProvider(ctx, "p")
	if err != nil || p.DataBoundary != "internal" {
		t.Fatalf("boundary not durable: %+v %v", p, err)
	}
	models, err := st.ListModels(ctx)
	if err != nil || models["m"].ContextWindow != 4096 || len(models["m"].Capabilities) != 1 || models["m"].Capabilities[0] != "reasoning" {
		t.Fatalf("model metadata not durable: %+v %v", models, err)
	}
	p.DataBoundary = ""
	if err := st.UpsertProvider(ctx, p); err != nil {
		t.Fatal(err)
	}
	p, err = st.GetProvider(ctx, "p")
	if err != nil || p.DataBoundary != "" {
		t.Fatal("omission retained old trust", p, err)
	}
	if err := st.DeleteProvider(ctx, "p"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetProvider(ctx, "p"); err != ErrNotFound {
		t.Fatal("deleted provider still exists", err)
	}
}
