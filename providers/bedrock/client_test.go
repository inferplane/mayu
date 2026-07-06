package bedrock

import (
	"encoding/json"
	"testing"

	brtypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"github.com/inferplane/inferplane/pkg/schema"
)

func TestBuildMessagesToolBlocks(t *testing.T) {
	text := "list files"
	input := json.RawMessage(`{"cmd":"ls"}`)
	errFlag := true
	msgs := []ConverseMessage{
		{Role: "user", Content: []schema.ContentBlock{{Type: "text", Text: &text}}},
		{Role: "assistant", Content: []schema.ContentBlock{{Type: "tool_use", ID: "t1", Name: "bash", Input: input}}},
		{Role: "user", Content: []schema.ContentBlock{{Type: "tool_result", ToolUseID: "t1", Content: json.RawMessage(`"a.go\nb.go"`), IsError: &errFlag}}},
	}
	out := buildMessages(msgs)
	if len(out) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(out))
	}
	if _, ok := out[0].Content[0].(*brtypes.ContentBlockMemberText); !ok {
		t.Fatalf("message 0 content: %T", out[0].Content[0])
	}
	tu, ok := out[1].Content[0].(*brtypes.ContentBlockMemberToolUse)
	if !ok {
		t.Fatalf("message 1 content: %T", out[1].Content[0])
	}
	if *tu.Value.ToolUseId != "t1" || *tu.Value.Name != "bash" {
		t.Fatalf("tool_use block: %+v", tu.Value)
	}
	if tu.Value.Input == nil {
		t.Fatal("tool_use input document is nil")
	}
	tr, ok := out[2].Content[0].(*brtypes.ContentBlockMemberToolResult)
	if !ok {
		t.Fatalf("message 2 content: %T", out[2].Content[0])
	}
	if *tr.Value.ToolUseId != "t1" || tr.Value.Status != brtypes.ToolResultStatusError {
		t.Fatalf("tool_result block: %+v", tr.Value)
	}
	trText, ok := tr.Value.Content[0].(*brtypes.ToolResultContentBlockMemberText)
	if !ok || trText.Value != "a.go\nb.go" {
		t.Fatalf("tool_result content: %+v", tr.Value.Content)
	}
}

func TestBuildMessagesDropsEmptyBlocksAndMessages(t *testing.T) {
	empty := ""
	msgs := []ConverseMessage{
		{Role: "assistant", Content: []schema.ContentBlock{{Type: "text", Text: &empty}}},
		{Role: "user", Content: []schema.ContentBlock{{Type: "thinking"}}},
	}
	if out := buildMessages(msgs); len(out) != 0 {
		t.Fatalf("expected both messages dropped (empty text, unsupported type), got %+v", out)
	}
}

func TestBuildToolConfigOmitsChoiceForAutoAndNone(t *testing.T) {
	tools := []ConverseTool{{Name: "bash", Description: "run", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	for _, choiceType := range []string{"", "auto"} {
		cfg := buildToolConfig(tools, ConverseToolChoice{Type: choiceType})
		if cfg == nil {
			t.Fatalf("choice %q: expected non-nil config (tools present)", choiceType)
		}
		if cfg.ToolChoice != nil {
			t.Fatalf("choice %q: expected ToolChoice to stay unset, got %T", choiceType, cfg.ToolChoice)
		}
	}
}

func TestBuildToolConfigNoneOmitsToolConfigEntirely(t *testing.T) {
	// Bedrock's ToolChoice union has no "forbid tools" member (only
	// Auto/Any/Tool). "none" must not silently degrade to auto — the closest
	// faithful behavior is to send no ToolConfig at all, so the model has no
	// tools to call.
	tools := []ConverseTool{{Name: "bash", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	if cfg := buildToolConfig(tools, ConverseToolChoice{Type: "none"}); cfg != nil {
		t.Fatalf("choice %q: expected ToolConfiguration to be omitted entirely, got %+v", "none", cfg)
	}
}

func TestBuildToolConfigAnyAndTool(t *testing.T) {
	tools := []ConverseTool{{Name: "bash", InputSchema: json.RawMessage(`{}`)}}
	cfg := buildToolConfig(tools, ConverseToolChoice{Type: "any"})
	if _, ok := cfg.ToolChoice.(*brtypes.ToolChoiceMemberAny); !ok {
		t.Fatalf("any: got %T", cfg.ToolChoice)
	}
	cfg = buildToolConfig(tools, ConverseToolChoice{Type: "tool", Name: "bash"})
	tc, ok := cfg.ToolChoice.(*brtypes.ToolChoiceMemberTool)
	if !ok || *tc.Value.Name != "bash" {
		t.Fatalf("tool: got %+v", cfg.ToolChoice)
	}
}

func TestBuildToolConfigNoTools(t *testing.T) {
	if cfg := buildToolConfig(nil, ConverseToolChoice{}); cfg != nil {
		t.Fatalf("expected nil ToolConfiguration when there are no tools, got %+v", cfg)
	}
}

func TestToolResultTextFlattensStringAndBlockArray(t *testing.T) {
	if got := toolResultText(json.RawMessage(`"plain"`)); got != "plain" {
		t.Fatalf("string form: %q", got)
	}
	blockText := "from block"
	blocks := []schema.ContentBlock{{Type: "text", Text: &blockText}}
	raw, _ := json.Marshal(blocks)
	if got := toolResultText(raw); got != "from block" {
		t.Fatalf("block-array form: %q", got)
	}
	if got := toolResultText(nil); got != "" {
		t.Fatalf("nil: %q", got)
	}
}
