package sensitivity

import "testing"

func TestContextKeywordsUseLatestUserIntent(t *testing.T) {
	for protocol, raw := range map[string]string{
		"openai":    `{"model":"auto","tools":[{"type":"function","function":{"name":"f","description":"security architecture"}}],"messages":[{"role":"system","content":"architecture"},{"role":"user","content":"architecture"},{"role":"assistant","content":"done"},{"role":"user","content":"say hello"}]}`,
		"anthropic": `{"model":"auto","system":"architecture","messages":[{"role":"user","content":"architecture"},{"role":"assistant","content":"done"},{"role":"user","content":[{"type":"text","text":"say hello"},{"type":"tool_result","tool_use_id":"t","content":"architecture"}]}]}`,
		"responses": `{"model":"auto","instructions":"architecture","input":[{"role":"user","content":"architecture"},{"role":"assistant","content":[{"type":"output_text","text":"done"}]},{"role":"user","content":[{"type":"input_text","text":"say hello"}]},{"type":"function_call_output","call_id":"t","output":"architecture"}]}`,
	} {
		t.Run(protocol, func(t *testing.T) {
			if MatchesContextKeywords([]byte(raw), protocol, []string{"architecture"}) {
				t.Fatal("tool definitions, history or tool output inflated task class")
			}
			if !MatchesContextKeywords([]byte(raw), protocol, []string{"HELLO"}) {
				t.Fatal("latest user intent was not matched case-insensitively")
			}
		})
	}
	if !MatchesContextKeywords([]byte(`{"model":"auto","input":"review this architecture"}`), "responses", []string{"architecture"}) {
		t.Fatal("Responses string input not classified")
	}
}
