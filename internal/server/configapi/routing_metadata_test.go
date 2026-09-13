package configapi

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/inferplane/inferplane/internal/config"
	"github.com/inferplane/inferplane/internal/providerstore"
)

func TestRoutingMetadataRoundTrip(t *testing.T) {
	ctx := context.Background()
	store, err := providerstore.OpenSQLite(filepath.Join(t.TempDir(), "topology.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var cfg config.Config
	raw := `{"providers":{"p":{"type":"openai","data_boundary":"internal"}},"models":{"m":{"context_window":32768,"capabilities":["tools","vision","reasoning","structured_output"],"aliases":["alias"],"targets":[{"provider":"p","model":"upstream"},{"provider":"p","model":"backup"}]}}}`
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	if err := providerstore.SeedIfEmpty(ctx, store, &cfg); err != nil {
		t.Fatal(err)
	}
	assertMetadata := func(wantBoundary string, wantWindow float64, wantCaps int) {
		t.Helper()
		eff, err := providerstore.Overlay(&config.Config{}, store)
		if err != nil {
			t.Fatal(err)
		}
		for _, obj := range []any{ExportDocFrom(eff.Providers, eff.Models), ViewFrom(eff.Providers, eff.Models)} {
			b, _ := json.Marshal(obj)
			var m map[string]any
			_ = json.Unmarshal(b, &m)
			var p, model map[string]any
			if ps, ok := m["providers"].(map[string]any); ok {
				p = ps["p"].(map[string]any)
				model = m["models"].(map[string]any)["m"].(map[string]any)
			} else {
				p = m["providers"].([]any)[0].(map[string]any)
				model = m["models"].([]any)[0].(map[string]any)
			}
			caps, _ := model["capabilities"].([]any)
			if p["data_boundary"] != wantBoundary || model["context_window"] != wantWindow || len(caps) != wantCaps {
				t.Errorf("metadata lost: %s", b)
			}
		}
	}
	assertMetadata("internal", 32768, 4)
	p, err := ParseProviderWrite("p", []byte(`{"type":"openai","data_boundary":"external"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertProvider(ctx, p); err != nil {
		t.Fatal(err)
	}
	route, err := ParseModelWrite([]byte(`{"context_window":8192,"capabilities":["tools"],"targets":[{"provider":"p","model":"replacement"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetModel(ctx, "m", route); err != nil {
		t.Fatal(err)
	}
	assertMetadata("external", 8192, 1)
	if err := store.DeleteModel(ctx, "m"); err != nil {
		t.Fatal(err)
	}
	models, err := store.ListModels(ctx)
	if err != nil || len(models) != 0 {
		t.Fatalf("delete: %v %v", models, err)
	}
}

func TestRoutingMetadataWriteValidation(t *testing.T) {
	for _, boundary := range []string{"local", "INTERNAL", " "} {
		if _, err := ParseProviderWrite("p", []byte(`{"type":"openai","data_boundary":"`+boundary+`"}`)); err == nil {
			t.Errorf("accepted boundary %q", boundary)
		}
	}
	for _, metadata := range []string{`"capabilities":["unknown"]`, `"context_window":-1`} {
		if _, err := ParseModelWrite([]byte(`{` + metadata + `,"targets":[{"provider":"p","model":"m"}]}`)); err == nil {
			t.Errorf("accepted %s", metadata)
		}
	}
}

func TestRoutingMetadataExportOwnsSlices(t *testing.T) {
	providers := map[string]config.ProviderConfig{"p": {DataBoundary: "internal", APIKeyRef: &config.SecretRef{Env: "PROVIDER_KEY"}}}
	models := map[string]config.ModelConfig{"m": {Capabilities: []string{"tools"}, Aliases: []string{"a"}, Targets: []config.Target{{Provider: "p", Model: "m"}}}}
	exp := ExportDocFrom(providers, models)
	models["m"].Capabilities[0] = "vision"
	models["m"].Aliases[0] = "b"
	providers["p"].APIKeyRef.Env = "CHANGED_KEY"
	if exp.Models["m"].Capabilities[0] != "tools" || exp.Models["m"].Aliases[0] != "a" || exp.Providers["p"].APIKeyRef.Env != "PROVIDER_KEY" {
		t.Fatal("export aliases caller-owned topology")
	}
}
