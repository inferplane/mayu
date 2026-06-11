package config

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// envRefRE finds every `"env": "NAME"` secret ref in an example so the test
// can stub it — examples must load with nothing but env vars present (§7).
var envRefRE = regexp.MustCompile(`"env"\s*:\s*"([A-Z0-9_]+)"`)

// TestExampleConfigsLoad guards the §9 "attaches in 5 minutes" bar: every
// shipped example must actually load. The set must include the base example
// and the self-hosted (Ollama/vLLM via openai_compatible) one.
func TestExampleConfigsLoad(t *testing.T) {
	paths, err := filepath.Glob("../../examples/*.json")
	if err != nil || len(paths) == 0 {
		t.Fatalf("no example configs found: %v", err)
	}

	want := map[string]bool{"config.json": false, "config.selfhosted.json": false}
	for _, p := range paths {
		if _, ok := want[filepath.Base(p)]; ok {
			want[filepath.Base(p)] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("examples/%s missing", name)
		}
	}

	for _, p := range paths {
		t.Run(filepath.Base(p), func(t *testing.T) {
			raw, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range envRefRE.FindAllStringSubmatch(string(raw), -1) {
				t.Setenv(m[1], "example-stub-value")
			}
			cfg, err := Load(p)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if len(cfg.Providers) == 0 {
				t.Error("example has no providers")
			}
			if len(cfg.Models) == 0 {
				t.Error("example has no model routes")
			}
		})
	}
}
