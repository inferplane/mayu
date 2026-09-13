package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRoutingMetadataConfigValidation(t *testing.T) {
	for _, tc := range []struct {
		body  string
		valid bool
	}{
		{`{"providers":{"p":{"type":"openai","data_boundary":"internal"}},"models":{"m":{"capabilities":["tools","vision","reasoning","structured_output"]}}}`, true},
		{`{"providers":{"p":{"type":"openai","data_boundary":"unknown"}}}`, true},
		{`{"providers":{"p":{"type":"openai","data_boundary":"local"}}}`, false},
		{`{"models":{"m":{"capabilities":["audio"]}}}`, false},
	} {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(tc.body), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := LoadRaw(path)
		if (err == nil) != tc.valid {
			t.Errorf("valid=%v error=%v body=%s", tc.valid, err, tc.body)
		}
	}
}
