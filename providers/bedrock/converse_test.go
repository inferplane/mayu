package bedrock

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/inferplane/inferplane/pkg/schema"
	"github.com/inferplane/inferplane/providers"
)

func TestCanonicalToConverseExtractsTextAndSystem(t *testing.T) {
	raw := []byte(`{"model":"moonshot.kimi-k2","max_tokens":256,"system":[{"type":"text","text":"be brief"}],"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]},{"role":"assistant","content":"hi"}],"model_fields":{"top_k":40}}`)
	cr, err := toConverseRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if cr.System != "be brief" {
		t.Fatalf("system: %q", cr.System)
	}
	if len(cr.Messages) != 2 || cr.Messages[0].Role != "user" || textOf(cr.Messages[0]) != "hello" || textOf(cr.Messages[1]) != "hi" {
		t.Fatalf("messages: %+v", cr.Messages)
	}
	if cr.ModelFields["top_k"].(float64) != 40 {
		t.Fatalf("model_fields not carried: %+v", cr.ModelFields)
	}
}

// textOf concatenates the text blocks of a converse message, for tests that
// only care about plain text.
func textOf(m ConverseMessage) string {
	var s string
	for _, b := range m.Content {
		if b.Type == "text" && b.Text != nil {
			s += *b.Text
		}
	}
	return s
}

func TestToConverseRequestCarriesSamplingParams(t *testing.T) {
	raw := []byte(`{"model":"kimi","max_tokens":256,"temperature":0.7,"top_p":0.9,"stop_sequences":["END"],"messages":[{"role":"user","content":"hi"}]}`)
	cr, err := toConverseRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if cr.Inference["temperature"] != 0.7 {
		t.Fatalf("temperature not carried: %v", cr.Inference["temperature"])
	}
	if cr.Inference["topP"] != 0.9 {
		t.Fatalf("top_p not carried: %v", cr.Inference["topP"])
	}
	if ss, ok := cr.Inference["stopSequences"].([]string); !ok || len(ss) != 1 || ss[0] != "END" {
		t.Fatalf("stop_sequences not carried: %v", cr.Inference["stopSequences"])
	}
}

func TestToConverseRequestParsesTools(t *testing.T) {
	raw := []byte(`{"messages":[{"role":"user","content":"hi"}],"tools":[
		{"name":"bash","description":"run a command","input_schema":{"type":"object","properties":{"cmd":{"type":"string"}}}},
		{"name":"computer","description":"no schema"}
	],"tool_choice":{"type":"tool","name":"bash"}}`)
	cr, err := toConverseRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(cr.Tools) != 1 || cr.Tools[0].Name != "bash" {
		t.Fatalf("expected only the schema-bearing tool to survive, got %+v", cr.Tools)
	}
	if cr.ToolChoice.Type != "tool" || cr.ToolChoice.Name != "bash" {
		t.Fatalf("tool_choice: %+v", cr.ToolChoice)
	}
}

func TestToConverseRequestSkipsOversizedToolNames(t *testing.T) {
	// Bedrock's ToolSpecification.Name is capped at 64 chars; Anthropic allows
	// up to 128, and long MCP-qualified names routinely exceed 64 in practice.
	longName := "mcp__plugin_aws-serverless_aws-serverless-mcp__secure_esm_dynamodb_policy"
	if len(longName) <= bedrockToolNameMax {
		t.Fatalf("fixture name is not actually oversized: %d chars", len(longName))
	}
	raw := []byte(`{"messages":[{"role":"user","content":"hi"}],"tools":[
		{"name":"bash","input_schema":{"type":"object"}},
		{"name":"` + longName + `","input_schema":{"type":"object"}}
	]}`)
	cr, err := toConverseRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(cr.Tools) != 1 || cr.Tools[0].Name != "bash" {
		t.Fatalf("expected only the short-named tool to survive, got %+v", cr.Tools)
	}
}

func TestToConverseRequestClearsToolChoicePointingAtDroppedTool(t *testing.T) {
	// tool_choice pins a tool that gets dropped for being oversized — Bedrock
	// rejects a SpecificToolChoice referencing a tool absent from the tool
	// list, so the choice must fall back to unset (auto) rather than forward
	// a dangling reference.
	longName := "mcp__plugin_aws-serverless_aws-serverless-mcp__secure_esm_dynamodb_policy"
	raw := []byte(`{"messages":[{"role":"user","content":"hi"}],"tools":[
		{"name":"bash","input_schema":{"type":"object"}},
		{"name":"` + longName + `","input_schema":{"type":"object"}}
	],"tool_choice":{"type":"tool","name":"` + longName + `"}}`)
	cr, err := toConverseRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if cr.ToolChoice != (ConverseToolChoice{}) {
		t.Fatalf("expected tool_choice to fall back to unset, got %+v", cr.ToolChoice)
	}
	// A choice pointing at a tool that DID survive must still be forwarded.
	raw2 := []byte(`{"messages":[{"role":"user","content":"hi"}],"tools":[
		{"name":"bash","input_schema":{"type":"object"}}
	],"tool_choice":{"type":"tool","name":"bash"}}`)
	cr2, err := toConverseRequest(raw2)
	if err != nil {
		t.Fatal(err)
	}
	if cr2.ToolChoice.Type != "tool" || cr2.ToolChoice.Name != "bash" {
		t.Fatalf("expected the surviving tool's choice to be forwarded, got %+v", cr2.ToolChoice)
	}
}

func TestToConverseRequestSkipsInvalidToolShapes(t *testing.T) {
	raw := []byte(`{"messages":[{"role":"user","content":"hi"}],"tools":[
		{"name":"bash","input_schema":{"type":"object"}},
		{"name":"","input_schema":{"type":"object"}},
		{"name":"nullschema","input_schema":null}
	]}`)
	cr, err := toConverseRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(cr.Tools) != 1 || cr.Tools[0].Name != "bash" {
		t.Fatalf("expected empty-named and null-schema tools to be dropped, got %+v", cr.Tools)
	}
}

func TestToConverseRequestToolBlocks(t *testing.T) {
	raw := []byte(`{"messages":[
		{"role":"user","content":[{"type":"text","text":"list files"}]},
		{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"bash","input":{"cmd":"ls"}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"a.go\nb.go"}]},
		{"role":"assistant","content":[{"type":"thinking","thinking":"dropped"}]}
	]}`)
	cr, err := toConverseRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	// the thinking-only assistant turn has zero surviving blocks and is skipped.
	if len(cr.Messages) != 3 {
		t.Fatalf("expected 3 messages (thinking-only turn dropped), got %d: %+v", len(cr.Messages), cr.Messages)
	}
	toolUse := cr.Messages[1].Content[0]
	if toolUse.Type != "tool_use" || toolUse.ID != "t1" || toolUse.Name != "bash" {
		t.Fatalf("tool_use block: %+v", toolUse)
	}
	toolResult := cr.Messages[2].Content[0]
	if toolResult.Type != "tool_result" || toolResult.ToolUseID != "t1" {
		t.Fatalf("tool_result block: %+v", toolResult)
	}
}

func TestToConverseRequestFoldsNonUserAssistantRoleIntoSystem(t *testing.T) {
	// Real Claude Code traffic interleaves a trailing role:"system" message
	// (hook/session-start output) in the messages array. Anthropic's API
	// tolerates this; Bedrock's ConversationRole only has user/assistant and
	// rejects it — and because it's the LAST message, the failure surfaces as
	// "last turn must be a user message" rather than an obvious role error.
	raw := []byte(`{"system":"be helpful","messages":[
		{"role":"user","content":"hello"},
		{"role":"system","content":"SessionStart hook: some info"}
	]}`)
	cr, err := toConverseRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(cr.Messages) != 1 || cr.Messages[0].Role != "user" {
		t.Fatalf("expected the system-role message to be folded away, not passed through: %+v", cr.Messages)
	}
	if !strings.Contains(cr.System, "be helpful") || !strings.Contains(cr.System, "SessionStart hook: some info") {
		t.Fatalf("system role content not folded into system prompt: %q", cr.System)
	}
}

func TestProviderCompleteConverse(t *testing.T) {
	text := "brief answer"
	fc := &fakeConverser{resp: ConverseResponse{
		Content:      []schema.ContentBlock{{Type: "text", Text: &text}},
		StopReason:   "end_turn",
		InputTokens:  5,
		OutputTokens: 3,
	}}
	p := &provider{conv: fc, modelAPI: map[string]string{"moonshot.kimi-k2": "converse"}}
	raw := []byte(`{"model":"kimi-k2","messages":[{"role":"user","content":"q"}]}`)
	resp, err := p.Complete(context.Background(), &providers.ProxyRequest{Model: "kimi-k2", Upstream: "moonshot.kimi-k2", RawBody: raw})
	if err != nil {
		t.Fatal(err)
	}
	// the converse response must be rendered back into an Anthropic-shaped body
	if resp.StatusCode != 200 || !strings.Contains(string(resp.RawBody), "brief answer") {
		t.Fatalf("resp body: %s", resp.RawBody)
	}
	if resp.Parsed == nil || resp.Parsed.Usage == nil || *resp.Parsed.Usage.OutputTokens != 3 {
		t.Fatalf("usage: %+v", resp.Parsed)
	}
}

func TestCompleteConverseToolUse(t *testing.T) {
	fc := &fakeConverser{resp: ConverseResponse{
		Content:      []schema.ContentBlock{{Type: "tool_use", ID: "t1", Name: "bash", Input: json.RawMessage(`{"cmd":"ls"}`)}},
		StopReason:   "tool_use",
		InputTokens:  5,
		OutputTokens: 3,
	}}
	p := &provider{conv: fc, modelAPI: map[string]string{"glm.glm-4": "converse"}}
	raw := []byte(`{"model":"glm-4","messages":[{"role":"user","content":"list files"}]}`)
	resp, err := p.Complete(context.Background(), &providers.ProxyRequest{Model: "glm-4", Upstream: "glm.glm-4", RawBody: raw})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(resp.RawBody), `"tool_use"`) || !strings.Contains(string(resp.RawBody), `"stop_reason":"tool_use"`) {
		t.Fatalf("resp body missing tool_use content/stop_reason: %s", resp.RawBody)
	}
}

func TestProviderStreamConverse(t *testing.T) {
	fc := &fakeConverser{streamEv: []ConverseStreamEvent{
		{Kind: eventTextDelta, TextDelta: "par"},
		{Kind: eventTextDelta, TextDelta: "tial"},
		{Kind: eventBlockStop},
		{Kind: eventMessageStop, StopReason: "end_turn"},
		{Kind: eventUsage, InputTokens: 5, OutputTokens: 4},
	}}
	p := &provider{conv: fc, modelAPI: map[string]string{"glm.glm-4": "converse"}}
	seq, err := p.Stream(context.Background(), &providers.ProxyRequest{Model: "glm-4", Upstream: "glm.glm-4", RawBody: []byte(`{"messages":[]}`), Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	var sse strings.Builder
	for ev, err := range seq {
		if err != nil {
			t.Fatal(err)
		}
		sse.WriteString(string(ev.Raw))
	}
	s := sse.String()
	// must produce a well-formed Anthropic SSE sequence carrying the deltas.
	// Each ConverseStreamEvent maps to one content_block_delta, so the two
	// deltas "par" and "tial" appear as separate text_delta events (they are
	// NOT concatenated into "partial" — that would defeat streaming).
	if !strings.Contains(s, "event: message_start") || !strings.Contains(s, "event: message_stop") ||
		!strings.Contains(s, `"text":"par"`) || !strings.Contains(s, `"text":"tial"`) {
		t.Fatalf("converse stream not rendered as Anthropic SSE: %s", s)
	}
	if !strings.Contains(s, `"input_tokens":5`) || !strings.Contains(s, `"output_tokens":4`) {
		t.Fatalf("usage not carried into message_delta: %s", s)
	}
}

func TestStreamConverseUsageAfterMessageStop(t *testing.T) {
	// Regression: Bedrock delivers the Metadata (usage) event AFTER
	// MessageStop. The old implementation returned on MessageStop and always
	// reported 0/0 usage; this must carry the real numbers.
	fc := &fakeConverser{streamEv: []ConverseStreamEvent{
		{Kind: eventTextDelta, TextDelta: "hi"},
		{Kind: eventBlockStop},
		{Kind: eventMessageStop, StopReason: "end_turn"},
		{Kind: eventUsage, InputTokens: 7, OutputTokens: 3},
	}}
	p := &provider{conv: fc, modelAPI: map[string]string{"m": "converse"}}
	seq, err := p.Stream(context.Background(), &providers.ProxyRequest{Model: "m", Upstream: "m", RawBody: []byte(`{"messages":[]}`), Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	var sse strings.Builder
	for ev, err := range seq {
		if err != nil {
			t.Fatal(err)
		}
		sse.WriteString(string(ev.Raw))
	}
	s := sse.String()
	if !strings.Contains(s, `"input_tokens":7`) || !strings.Contains(s, `"output_tokens":3`) {
		t.Fatalf("expected real usage (7/3) after message_stop, got: %s", s)
	}
}

func TestStreamConverseNoMetadata(t *testing.T) {
	// Some streams may never emit a Metadata event; the terminal frame must
	// still be flushed (with zero usage) instead of hanging forever.
	fc := &fakeConverser{streamEv: []ConverseStreamEvent{
		{Kind: eventTextDelta, TextDelta: "hi"},
		{Kind: eventBlockStop},
		{Kind: eventMessageStop, StopReason: "end_turn"},
	}}
	p := &provider{conv: fc, modelAPI: map[string]string{"m": "converse"}}
	seq, err := p.Stream(context.Background(), &providers.ProxyRequest{Model: "m", Upstream: "m", RawBody: []byte(`{"messages":[]}`), Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	var sse strings.Builder
	for ev, err := range seq {
		if err != nil {
			t.Fatal(err)
		}
		sse.WriteString(string(ev.Raw))
	}
	if !strings.Contains(sse.String(), "event: message_stop") {
		t.Fatalf("expected a terminal frame even without a Metadata event: %s", sse.String())
	}
}

func TestStreamConverseNoTerminalEventsAtAll(t *testing.T) {
	// Neither MessageStop nor Metadata ever arrives (e.g. the upstream event
	// channel closes cleanly with no terminal event at all). The client must
	// still get a message_delta/message_stop pair rather than being left
	// hanging with only message_start + content deltas.
	fc := &fakeConverser{streamEv: []ConverseStreamEvent{
		{Kind: eventTextDelta, TextDelta: "hi"},
	}}
	p := &provider{conv: fc, modelAPI: map[string]string{"m": "converse"}}
	seq, err := p.Stream(context.Background(), &providers.ProxyRequest{Model: "m", Upstream: "m", RawBody: []byte(`{"messages":[]}`), Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	var sse strings.Builder
	for ev, err := range seq {
		if err != nil {
			t.Fatal(err)
		}
		sse.WriteString(string(ev.Raw))
	}
	s := sse.String()
	if !strings.Contains(s, "event: message_delta") || !strings.Contains(s, "event: message_stop") {
		t.Fatalf("expected a terminal frame even with no MessageStop/Metadata at all: %s", s)
	}
}

func TestStreamConverseToolUse(t *testing.T) {
	fc := &fakeConverser{streamEv: []ConverseStreamEvent{
		{Kind: eventTextDelta, TextDelta: "Sure, let me check."},
		{Kind: eventBlockStop},
		{Kind: eventToolUseStart, ToolUseID: "tooluse_abc", ToolName: "bash"},
		{Kind: eventToolInputDelta, ToolDelta: `{"cmd":`},
		{Kind: eventToolInputDelta, ToolDelta: `"ls"}`},
		{Kind: eventBlockStop},
		{Kind: eventMessageStop, StopReason: "tool_use"},
		{Kind: eventUsage, InputTokens: 10, OutputTokens: 6},
	}}
	p := &provider{conv: fc, modelAPI: map[string]string{"m": "converse"}}
	seq, err := p.Stream(context.Background(), &providers.ProxyRequest{Model: "m", Upstream: "m", RawBody: []byte(`{"messages":[]}`), Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	var events []string
	var sse strings.Builder
	for ev, err := range seq {
		if err != nil {
			t.Fatal(err)
		}
		sse.WriteString(string(ev.Raw))
		events = append(events, ev.Chunk.Type)
	}
	s := sse.String()
	wantOrder := []string{
		"message_start",
		"content_block_start", "content_block_delta", "content_block_stop", // text block, idx 0
		"content_block_start", "content_block_delta", "content_block_delta", "content_block_stop", // tool_use block, idx 1
		"message_delta", "message_stop",
	}
	if len(events) != len(wantOrder) {
		t.Fatalf("event order = %v, want %v", events, wantOrder)
	}
	for i, want := range wantOrder {
		if events[i] != want {
			t.Fatalf("event[%d] = %q, want %q (full: %v)", i, events[i], want, events)
		}
	}
	if !strings.Contains(s, `"type":"tool_use"`) || !strings.Contains(s, `"name":"bash"`) || !strings.Contains(s, `"id":"tooluse_abc"`) {
		t.Fatalf("tool_use content_block_start missing fields: %s", s)
	}
	if !strings.Contains(s, `"type":"input_json_delta"`) || !strings.Contains(s, `"partial_json":"{\"cmd\":"`) {
		t.Fatalf("input_json_delta missing: %s", s)
	}
	if !strings.Contains(s, `"stop_reason":"tool_use"`) {
		t.Fatalf("message_delta missing tool_use stop_reason: %s", s)
	}
	// the two tool_use blocks must share index 1 (sequential re-indexing, not
	// Bedrock's own ContentBlockIndex).
	if strings.Count(s, `"index":1`) < 4 {
		t.Fatalf("expected the tool_use block's start/delta/delta/stop to share index 1: %s", s)
	}
}
