package sensitivity

import (
	"context"
	"slices"
	"testing"
)

func TestResponsesInspectionSeesOriginalToolAndInstructionData(t *testing.T) {
	raw := []byte(`{"model":"auto","instructions":"Contact alice@example.test","max_output_tokens":64,"tools":[{"type":"function","name":"lookup","description":"Lookup a record","parameters":{"type":"object"}}],"input":[{"role":"user","content":[{"type":"input_text","text":"look up this record"}]},{"type":"function_call","call_id":"call_a","name":"lookup","arguments":"{\"card\":4111111111111111}"},{"type":"function_call_output","call_id":"call_a","output":"{\"person\":\"900101-1234567\"}"}]}`)
	got, err := NewInspector().Inspect(context.Background(), "responses", raw)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Complete || !got.HasTools || !got.HasHistory || got.UserTurns != 1 || got.OutputTokens != 64 {
		t.Fatalf("incorrect Responses shape: %+v", got)
	}
	for _, category := range []string{"email", "credit_card", "korean_rrn"} {
		if !slices.Contains(got.Categories, category) {
			t.Errorf("missing %s in %v", category, got.Categories)
		}
	}
}

func TestResponsesInspectionMarksUnavailableContentIncomplete(t *testing.T) {
	for name, raw := range map[string]string{
		"image":        `{"model":"auto","input":[{"role":"user","content":[{"type":"input_image","image_url":"https://example.test/a.png"}]}]}`,
		"encrypted":    `{"model":"auto","input":[{"type":"reasoning","encrypted_content":"opaque","summary":[]} ,{"role":"user","content":"hello"}]}`,
		"previous":     `{"model":"auto","previous_response_id":"resp_previous","input":"hello"}`,
		"conversation": `{"model":"auto","conversation":"conv_previous","input":"hello"}`,
		"unknown":      `{"model":"auto","input":[{"type":"future_media","payload":"opaque"}]}`,
		"remote tool":  `{"model":"auto","input":"hello","tools":[{"type":"mcp","server_url":"https://example.test"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			got, err := NewInspector().Inspect(context.Background(), "responses", []byte(raw))
			if err != nil || got.Complete {
				t.Fatalf("uninspectable request = %+v, %v", got, err)
			}
		})
	}
}

func TestResponsesCustomToolAndAssistantPhaseInspection(t *testing.T) {
	raw := []byte(`{"model":"auto","input":[{"role":"user","content":"fix it"},{"role":"assistant","phase":"commentary","content":[{"type":"output_text","text":"I will edit it","annotations":[]}]},{"type":"custom_tool_call","name":"apply_patch","call_id":"call_patch","input":"*** Begin Patch\n+alice@example.test\n*** End Patch"},{"type":"custom_tool_call_output","call_id":"call_patch","output":"Done"}],"tools":[{"type":"custom","name":"apply_patch","description":"Apply edits","format":{"type":"text"}}]}`)
	got, err := NewInspector().Inspect(context.Background(), "responses", raw)
	if err != nil || !got.Complete || !got.HasTools || !got.HasHistory || !slices.Contains(got.Categories, "email") {
		t.Fatalf("custom tool inspection = %+v, %v", got, err)
	}
}

func TestCodexNamespaceToolsAndSummaryPreferenceAreInspectable(t *testing.T) {
	raw := []byte(`{"model":"auto","client_metadata":{"session_id":"opaque"},"reasoning":{"summary":"auto"},"include":["reasoning.encrypted_content"],"input":"fix this bug","tools":[{"type":"namespace","name":"functions","description":"local tools","tools":[{"type":"custom","name":"apply_patch","description":"edit files","format":{"type":"text"}},{"type":"function","name":"lookup","description":"contact alice@example.test","parameters":{"type":"object"}}]}]}`)
	got, err := NewInspector().Inspect(context.Background(), "responses", raw)
	if err != nil || !got.Complete || !got.HasTools || got.HasReasoning || !slices.Contains(got.Categories, "email") {
		t.Fatalf("Codex namespace inspection = %+v, %v", got, err)
	}
}

func TestResponsesRemoteToolsAreDistinctFromOpaqueLocalContent(t *testing.T) {
	got, err := NewInspector().Inspect(context.Background(), "responses",
		[]byte(`{"model":"auto","input":"hello","tools":[{"type":"mcp","server_url":"https://example.test/tools"}]}`))
	if err != nil || got.Complete || !got.HasRemoteTools {
		t.Fatalf("remote egress capability lost: %+v, %v", got, err)
	}
}
