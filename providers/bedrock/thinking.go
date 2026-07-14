package bedrock

import (
	"encoding/json"
	"strings"
)

// legacyThinkingBrokenModels is the upstream-model-ID substring allow-list of
// models that REJECT the legacy extended-thinking shape
// (`thinking: {"type": "enabled", "budget_tokens": N}`) with a 400
// ValidationException and require the newer
// `thinking: {"type": "adaptive"}` + top-level `output_config: {"effort":
// ...}` shape instead (Anthropic's own support matrix lists these as "Not
// supported (400 error)" for the legacy shape). Deliberately NOT a deny-list:
// models not on this list (e.g. claude-haiku-4-5, claude-sonnet-4-6 — the
// latter still accepts the legacy shape, just deprecated) are left completely
// untouched, so an unrecognized future model fails the same way it did before
// this rewrite (a plain 400) rather than risking a new regression from a
// wrong match. Anthropic will keep releasing models with this same
// restriction — extend this list as they're confirmed broken, don't
// preemptively guess.
var legacyThinkingBrokenModels = []string{
	"opus-4-7",
	"opus-4-8",
	"fable-5",
	"sonnet-5",
	"mythos",
}

// needsAdaptiveRewrite reports whether upstream is a model known to reject
// the legacy thinking shape, using the same upstream-model-ID substring
// matching style as apiFor (bedrock.go).
func needsAdaptiveRewrite(upstream string) bool {
	for _, m := range legacyThinkingBrokenModels {
		if strings.Contains(upstream, m) {
			return true
		}
	}
	return false
}

// effortForBudget maps a legacy budget_tokens value to the closest adaptive-
// thinking effort level. The thresholds are a judgment call, not a spec —
// Anthropic doesn't publish a token-to-effort equivalence, so this only needs
// to land in the right ballpark. Missing/unparsable budgets default to
// "medium" (Anthropic's own effort default is "high", but "medium" is a more
// conservative match for a caller that asked for a specific, usually modest,
// budget rather than unlimited effort).
func effortForBudget(tokens int64, ok bool) string {
	if !ok {
		return "medium"
	}
	switch {
	case tokens <= 2048:
		return "low"
	case tokens <= 8192:
		return "medium"
	default:
		return "high"
	}
}

// legacyThinking is the shape Claude Code (and the standard Anthropic
// Messages API, pre-adaptive-thinking) sends today.
type legacyThinking struct {
	Type         string `json:"type"`
	BudgetTokens *int64 `json:"budget_tokens"`
}

// rewriteLegacyThinking rewrites top["thinking"] in place from the legacy
// `{"type":"enabled","budget_tokens":N}` shape to the adaptive-thinking shape
// models on legacyThinkingBrokenModels require, adding a top-level
// "output_config" only when the caller didn't already send one (never
// override an explicit client choice). It is a no-op — never an error — when
// "thinking" is absent, unparsable, or not `type: "enabled"`: this is a
// best-effort compatibility rewrite, not a validator, so a request shape we
// don't recognize passes through untouched rather than becoming a local
// error the caller didn't ask for.
func rewriteLegacyThinking(top map[string]json.RawMessage) {
	raw, has := top["thinking"]
	if !has {
		return
	}
	var t legacyThinking
	if err := json.Unmarshal(raw, &t); err != nil || t.Type != "enabled" {
		return
	}
	top["thinking"] = json.RawMessage(`{"type":"adaptive"}`)
	if _, has := top["output_config"]; has {
		return
	}
	var budget int64
	var budgetOK bool
	if t.BudgetTokens != nil {
		budget, budgetOK = *t.BudgetTokens, true
	}
	effort, _ := json.Marshal(effortForBudget(budget, budgetOK))
	top["output_config"] = json.RawMessage(`{"effort":` + string(effort) + `}`)
}
