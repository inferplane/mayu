package bedrock

import (
	"encoding/json"
	"testing"
)

func TestNeedsAdaptiveRewrite(t *testing.T) {
	broken := []string{
		"global.anthropic.claude-opus-4-8",
		"anthropic.claude-fable-5",
		"global.anthropic.claude-sonnet-5",
		"anthropic.claude-opus-4-7",
		"anthropic.claude-mythos-preview",
	}
	for _, upstream := range broken {
		if !needsAdaptiveRewrite(upstream) {
			t.Errorf("needsAdaptiveRewrite(%q) = false, want true", upstream)
		}
	}

	// Models that still accept the legacy shape (supported, or supported-but-
	// deprecated) must NOT match — a false positive here would regress a
	// currently-working model.
	ok := []string{
		"anthropic.claude-sonnet-4-6",
		"anthropic.claude-opus-4-6",
		"global.anthropic.claude-haiku-4-5-20251001-v1:0",
		// substring trap: "sonnet-4-5" must not match the "sonnet-5" pattern.
		"global.anthropic.claude-sonnet-4-5-20250929-v1:0",
		"anthropic.claude-3-haiku-20240307-v1:0",
	}
	for _, upstream := range ok {
		if needsAdaptiveRewrite(upstream) {
			t.Errorf("needsAdaptiveRewrite(%q) = true, want false (currently-working model must not be touched)", upstream)
		}
	}
}

func TestEffortForBudget(t *testing.T) {
	cases := []struct {
		tokens int64
		ok     bool
		want   string
	}{
		{0, false, "medium"}, // no budget_tokens sent at all
		{1024, true, "low"},
		{2048, true, "low"},
		{2049, true, "medium"},
		{8192, true, "medium"},
		{8193, true, "high"},
		{100000, true, "high"},
	}
	for _, c := range cases {
		if got := effortForBudget(c.tokens, c.ok); got != c.want {
			t.Errorf("effortForBudget(%d, %v) = %q, want %q", c.tokens, c.ok, got, c.want)
		}
	}
}

func TestRewriteLegacyThinking(t *testing.T) {
	rewrite := func(t *testing.T, body string) map[string]json.RawMessage {
		var top map[string]json.RawMessage
		if err := json.Unmarshal([]byte(body), &top); err != nil {
			t.Fatal(err)
		}
		rewriteLegacyThinking(top)
		return top
	}

	t.Run("legacy enabled+budget_tokens rewritten to adaptive+effort", func(t *testing.T) {
		top := rewrite(t, `{"thinking":{"type":"enabled","budget_tokens":1024}}`)
		if string(top["thinking"]) != `{"type":"adaptive"}` {
			t.Fatalf("thinking = %s", top["thinking"])
		}
		var oc struct{ Effort string }
		if err := json.Unmarshal(top["output_config"], &oc); err != nil || oc.Effort != "low" {
			t.Fatalf("output_config = %s (err=%v)", top["output_config"], err)
		}
	})

	t.Run("already adaptive is idempotent (no output_config added if already type adaptive)", func(t *testing.T) {
		top := rewrite(t, `{"thinking":{"type":"adaptive"}}`)
		if string(top["thinking"]) != `{"type":"adaptive"}` {
			t.Fatalf("thinking = %s", top["thinking"])
		}
		if _, has := top["output_config"]; has {
			t.Fatalf("output_config must not be added for an already-adaptive request: %s", top["output_config"])
		}
	})

	t.Run("existing output_config is never overwritten", func(t *testing.T) {
		top := rewrite(t, `{"thinking":{"type":"enabled","budget_tokens":1024},"output_config":{"effort":"max"}}`)
		var oc struct{ Effort string }
		if err := json.Unmarshal(top["output_config"], &oc); err != nil || oc.Effort != "max" {
			t.Fatalf("output_config clobbered: %s", top["output_config"])
		}
	})

	t.Run("no thinking field is a no-op", func(t *testing.T) {
		top := rewrite(t, `{"max_tokens":16}`)
		if _, has := top["thinking"]; has {
			t.Fatalf("thinking must not be added when absent: %s", top["thinking"])
		}
		if _, has := top["output_config"]; has {
			t.Fatalf("output_config must not be added when thinking absent: %s", top["output_config"])
		}
	})

	t.Run("unparsable thinking is a no-op, not an error", func(t *testing.T) {
		top := rewrite(t, `{"thinking":"not-an-object"}`)
		if string(top["thinking"]) != `"not-an-object"` {
			t.Fatalf("unparsable thinking must pass through untouched: %s", top["thinking"])
		}
	})

	t.Run("thinking.type disabled is left untouched", func(t *testing.T) {
		top := rewrite(t, `{"thinking":{"type":"disabled"}}`)
		if string(top["thinking"]) != `{"type":"disabled"}` {
			t.Fatalf("disabled thinking must not be rewritten: %s", top["thinking"])
		}
	})

	t.Run("budget_tokens absent defaults to medium effort", func(t *testing.T) {
		top := rewrite(t, `{"thinking":{"type":"enabled"}}`)
		var oc struct{ Effort string }
		if err := json.Unmarshal(top["output_config"], &oc); err != nil || oc.Effort != "medium" {
			t.Fatalf("output_config = %s (err=%v)", top["output_config"], err)
		}
	})
}
