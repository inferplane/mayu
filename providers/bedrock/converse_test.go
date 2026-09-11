package bedrock

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	brtypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
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

func TestToConverseRequestSkipsInvalidCharsetToolNames(t *testing.T) {
	// Bedrock's ToolSpecification.Name only allows [a-zA-Z][a-zA-Z0-9_]* — no
	// hyphens, dots, or colons. Claude Code / MCP tool names commonly contain
	// hyphens (e.g. an MCP-qualified name), which is well within the 64-char
	// limit but still rejected by Bedrock with a ValidationException.
	raw := []byte(`{"messages":[{"role":"user","content":"hi"}],"tools":[
		{"name":"bash","input_schema":{"type":"object"}},
		{"name":"mcp__aws-sdk-v3__getObject","input_schema":{"type":"object"}}
	]}`)
	cr, err := toConverseRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(cr.Tools) != 1 || cr.Tools[0].Name != "bash" {
		t.Fatalf("expected the hyphenated tool name to be dropped, got %+v", cr.Tools)
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

func TestToConverseRequestKeepsSystemRoleInPlace(t *testing.T) {
	// Real Claude Code traffic interleaves role:"system" messages (hook
	// output) in the messages array — including MID-conversation. Bedrock's
	// ConversationRole only has user/assistant, so they can't pass through —
	// but folding them into the system prompt (the old behavior) mutates the
	// HEAD of the prompt on every turn they appear, invalidating the entire
	// prompt-cache prefix: observed live as cache_creation ≈ full input
	// (475k tokens) on every request, TTFT long enough that Claude Code
	// times out and falls back to another model. The content must stay
	// IN PLACE: merged into the next user message.
	raw := []byte(`{"system":"be helpful","messages":[
		{"role":"user","content":"turn 1"},
		{"role":"assistant","content":"reply 1"},
		{"role":"system","content":"hook output for turn 2"},
		{"role":"user","content":"turn 2"}
	]}`)
	cr, err := toConverseRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(cr.System, "hook output") {
		t.Fatalf("mid-conversation system role folded into system prompt (cache-busting): %q", cr.System)
	}
	if len(cr.Messages) != 3 {
		t.Fatalf("want 3 messages (user, assistant, user), got %d: %+v", len(cr.Messages), cr.Messages)
	}
	last := cr.Messages[2]
	if last.Role != "user" {
		t.Fatalf("last role = %q", last.Role)
	}
	joined := textOf(last)
	if !strings.Contains(joined, "hook output for turn 2") || !strings.Contains(joined, "turn 2") {
		t.Fatalf("system-role content not merged into the next user message: %q", joined)
	}
	// In-place order: hook text precedes the user text it preceded on the wire.
	if strings.Index(joined, "hook output") > strings.Index(joined, "turn 2") {
		t.Fatalf("merged out of order: %q", joined)
	}
}

func TestToConverseRequestTrailingSystemRoleJoinsTheLastUserMessage(t *testing.T) {
	// A TRAILING system-role message (session-start hook before any reply) has
	// no following user message to merge into. It must extend the PRECEDING
	// user turn rather than become a second consecutive user message, a shape
	// Converse can reject.
	raw := []byte(`{"system":"be helpful","messages":[
		{"role":"user","content":"hello"},
		{"role":"system","content":"SessionStart hook: some info"}
	]}`)
	cr, err := toConverseRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(cr.System, "SessionStart hook") {
		t.Fatalf("trailing system role folded into system prompt: %q", cr.System)
	}
	if len(cr.Messages) != 1 || cr.Messages[0].Role != "user" {
		t.Fatalf("want one merged user message: %+v", cr.Messages)
	}
	joined := textOf(cr.Messages[0])
	if !strings.Contains(joined, "hello") || !strings.Contains(joined, "SessionStart hook: some info") {
		t.Fatalf("merged text missing a part: %q", joined)
	}
	if strings.Index(joined, "hello") > strings.Index(joined, "SessionStart hook") {
		t.Fatalf("merged out of order: %q", joined)
	}
}

func TestToConverseRequestTrailingSystemRoleAfterAssistantBecomesUserMessage(t *testing.T) {
	// Same trailing hook, but the last turn is the assistant's — there is no
	// user message to extend, so the hook text becomes its own user turn.
	raw := []byte(`{"messages":[
		{"role":"user","content":"hello"},
		{"role":"assistant","content":"hi"},
		{"role":"system","content":"SessionStart hook: some info"}
	]}`)
	cr, err := toConverseRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(cr.Messages) != 3 || cr.Messages[2].Role != "user" {
		t.Fatalf("want a trailing user message: %+v", cr.Messages)
	}
	if !strings.Contains(textOf(cr.Messages[2]), "SessionStart hook: some info") {
		t.Fatalf("hook text missing: %q", textOf(cr.Messages[2]))
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

// TestCompleteConverseThrottledSurfacesUpstreamError pins the bug fix: a
// throttled Converse call must surface as a *providers.UpstreamError with the
// real status (429), not a bare error the ingress can only turn into a
// generic 502.
func TestCompleteConverseThrottledSurfacesUpstreamError(t *testing.T) {
	fc := &fakeConverser{err: &brtypes.ThrottlingException{}}
	p := &provider{conv: fc, modelAPI: map[string]string{"moonshot.kimi-k2": "converse"}}
	raw := []byte(`{"model":"kimi-k2","messages":[{"role":"user","content":"q"}]}`)
	_, err := p.Complete(context.Background(), &providers.ProxyRequest{Model: "kimi-k2", Upstream: "moonshot.kimi-k2", RawBody: raw})
	var ue *providers.UpstreamError
	if !errors.As(err, &ue) {
		t.Fatalf("expected *UpstreamError, got %v", err)
	}
	if ue.StatusCode != 429 {
		t.Fatalf("status = %d, want 429", ue.StatusCode)
	}
}

// TestStreamConversePreTTFTErrorSurfacesUpstreamError: ConverseStream never
// opened (AccessDeniedException before any bytes) — its real status must
// still reach the ingress's UpstreamError tee.
func TestStreamConversePreTTFTErrorSurfacesUpstreamError(t *testing.T) {
	fc := &fakeConverser{err: &brtypes.AccessDeniedException{}}
	p := &provider{conv: fc, modelAPI: map[string]string{"moonshot.kimi-k2": "converse"}}
	raw := []byte(`{"model":"kimi-k2","stream":true,"messages":[{"role":"user","content":"q"}]}`)
	_, err := p.Stream(context.Background(), &providers.ProxyRequest{Model: "kimi-k2", Upstream: "moonshot.kimi-k2", RawBody: raw, Stream: true})
	var ue *providers.UpstreamError
	if !errors.As(err, &ue) {
		t.Fatalf("expected *UpstreamError, got %v", err)
	}
	if ue.StatusCode != 403 {
		t.Fatalf("status = %d, want 403", ue.StatusCode)
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

func TestStreamConverseDiscardsOrphanedToolInputDelta(t *testing.T) {
	// A malformed/truncated upstream stream can send a tool-input delta before
	// any ToolUseStart (or text delta) has opened a block, leaving idx at its
	// initial -1. Emitting content_block_delta with index:-1 would hand the
	// client an invalid SSE frame; the orphaned delta must be discarded
	// instead, and the rest of the stream still terminates cleanly.
	fc := &fakeConverser{streamEv: []ConverseStreamEvent{
		{Kind: eventToolInputDelta, ToolDelta: `{"cmd":"ls"}`},
		{Kind: eventMessageStop, StopReason: "end_turn"},
		{Kind: eventUsage, InputTokens: 1, OutputTokens: 1},
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
	if strings.Contains(s, `"index":-1`) {
		t.Fatalf("orphaned tool-input delta must be discarded, not emitted with index:-1: %s", s)
	}
	if !strings.Contains(s, "event: message_delta") || !strings.Contains(s, "event: message_stop") {
		t.Fatalf("expected the stream to still terminate cleanly: %s", s)
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

// The OpenAI gpt-5.6 family on Converse reports inputTokens INCLUSIVE of the
// cache read/write counts (OpenAI prompt_tokens semantics) — observed on live
// ap-northeast-2 gateway traffic 2026-08-29→31: input = cacheRead + cacheWrite
// + Δ held across 20+ consecutive requests (audit fixture below). The
// Anthropic wire requires the three counts DISJOINT, so passing the inclusive
// value through double-counts every cached token: the client's context math
// runs at ~2x the real prompt and settle bills cache tokens twice (full input
// rate + cache rate) — the egress mirror of the OpenAI-ingress bug ADR-030
// fixed in usageFromOAI.
func TestUsageWithCacheInclusiveInputFamilies(t *testing.T) {
	t.Run("gpt-5.6 flat write total", func(t *testing.T) {
		// Live audit record 2026-08-31T04:11:53Z: in 50768, cr 40988, cw 9778.
		u := usageWithCache("global.openai.gpt-5.6-sol", 50768, 210, 40988, 0, 0, 9778)
		if u.InputTokens == nil || *u.InputTokens != 2 {
			t.Fatalf("input must become disjoint (50768-40988-9778=2), got %+v", u.InputTokens)
		}
		if u.CacheReadInputTokens == nil || *u.CacheReadInputTokens != 40988 {
			t.Errorf("cache_read must be untouched: %+v", u.CacheReadInputTokens)
		}
		if u.CacheCreationInputTokens == nil || *u.CacheCreationInputTokens != 9778 {
			t.Errorf("cache write total must be untouched: %+v", u.CacheCreationInputTokens)
		}
	})

	t.Run("gpt-5.6 TTL split", func(t *testing.T) {
		u := usageWithCache("global.openai.gpt-5.6-luna", 100, 5, 60, 30, 8, 38)
		if u.InputTokens == nil || *u.InputTokens != 2 {
			t.Fatalf("input must become disjoint (100-60-38=2), got %+v", u.InputTokens)
		}
	})

	t.Run("subtraction never yields a negative input", func(t *testing.T) {
		// A listed family reporting counts that contradict inclusive semantics
		// clamps to 0 (never over-bill — same posture as the unknown-TTL tier).
		u := usageWithCache("global.openai.gpt-5.6-terra", 10, 5, 40, 0, 0, 24)
		if u.InputTokens == nil || *u.InputTokens != 0 {
			t.Fatalf("input must clamp at 0, got %+v", u.InputTokens)
		}
	})

	t.Run("unlisted family passes through untouched", func(t *testing.T) {
		u := usageWithCache("moonshot.kimi-k2", 50768, 210, 40988, 0, 0, 9778)
		if u.InputTokens == nil || *u.InputTokens != 50768 {
			t.Fatalf("unverified family must keep upstream semantics, got %+v", u.InputTokens)
		}
	})

	t.Run("no cache counts leaves input alone even for a listed family", func(t *testing.T) {
		u := usageWithCache("global.openai.gpt-5.6-sol", 123, 5, 0, 0, 0, 0)
		if u.InputTokens == nil || *u.InputTokens != 123 {
			t.Fatalf("nothing to subtract, got %+v", u.InputTokens)
		}
	})
}

// The disjoint mapping must reach the provider response, not just the helper.
func TestCompleteConverseDisjointsInclusiveUsage(t *testing.T) {
	text := "ok"
	fc := &fakeConverser{resp: ConverseResponse{
		Content: []schema.ContentBlock{{Type: "text", Text: &text}}, StopReason: "end_turn",
		InputTokens: 50768, OutputTokens: 210, CacheReadTokens: 40988, CacheWriteTotal: 9778,
	}}
	p := &provider{conv: fc, modelAPI: map[string]string{"global.openai.gpt-5.6-sol": "converse"}}
	raw := []byte(`{"model":"gpt-5.6-sol","max_tokens":64,"messages":[{"role":"user","content":"q"}]}`)
	resp, err := p.Complete(context.Background(), &providers.ProxyRequest{Model: "gpt-5.6-sol", Upstream: "global.openai.gpt-5.6-sol", RawBody: raw})
	if err != nil {
		t.Fatal(err)
	}
	u := resp.Parsed.Usage
	if u.InputTokens == nil || *u.InputTokens != 2 {
		t.Fatalf("provider response input not disjointed: %+v", u.InputTokens)
	}
	if u.CacheReadInputTokens == nil || *u.CacheReadInputTokens != 40988 {
		t.Fatalf("cache_read lost: %+v", u.CacheReadInputTokens)
	}
}

// ADR-030: Bedrock reports prompt-cache counts SEPARATELY from InputTokens.
// Dropping them billed every cache read/write at zero on the Converse path
// while the InvokeModel passthrough billed them correctly — the same model
// costing different amounts depending on the API mode.
func TestUsageWithCache(t *testing.T) {
	t.Run("no cache keeps the pre-cache shape", func(t *testing.T) {
		u := usageWithCache("moonshot.kimi-k2", 10, 5, 0, 0, 0, 0)
		if u.CacheReadInputTokens != nil || u.CacheCreation != nil || u.CacheCreationInputTokens != nil {
			t.Fatalf("zero counts must stay nil so the JSON is unchanged: %+v", u)
		}
	})

	t.Run("TTL breakdown maps to the split fields", func(t *testing.T) {
		u := usageWithCache("moonshot.kimi-k2", 10, 5, 40, 20, 4, 24)
		if u.CacheReadInputTokens == nil || *u.CacheReadInputTokens != 40 {
			t.Errorf("cache_read: %+v", u.CacheReadInputTokens)
		}
		if u.CacheCreation == nil {
			t.Fatal("cache_creation split missing")
		}
		if got := u.CacheCreation.Ephemeral5mInputTokens; got == nil || *got != 20 {
			t.Errorf("5m tier: %+v", got)
		}
		if got := u.CacheCreation.Ephemeral1hInputTokens; got == nil || *got != 4 {
			t.Errorf("1h tier: %+v", got)
		}
		// The untiered total must NOT also be set, or the settlement path would
		// see both shapes and could double-count.
		if u.CacheCreationInputTokens != nil {
			t.Errorf("flat total must be omitted when the split is present: %+v", u.CacheCreationInputTokens)
		}
	})

	t.Run("untiered total is the fallback", func(t *testing.T) {
		u := usageWithCache("moonshot.kimi-k2", 10, 5, 0, 0, 0, 24)
		if u.CacheCreation != nil {
			t.Error("no split available, so cache_creation must stay nil")
		}
		if got := u.CacheCreationInputTokens; got == nil || *got != 24 {
			t.Errorf("flat total: %+v", got)
		}
	})

	// The resolved tiers must round-trip through the settlement helper the
	// ingress handlers actually call.
	t.Run("round-trips through CacheWriteTiers", func(t *testing.T) {
		u := usageWithCache("moonshot.kimi-k2", 10, 5, 40, 20, 4, 24)
		w5, w1h := u.CacheWriteTiers()
		if w5 != 20 || w1h != 4 {
			t.Fatalf("CacheWriteTiers = (%d, %d), want (20, 4)", w5, w1h)
		}
	})
}

func TestCacheWriteTiers_splitsByTTL(t *testing.T) {
	in5m, in1h := int32(20), int32(4)
	w5, w1h := cacheWriteTiers([]brtypes.CacheDetail{
		{InputTokens: &in5m, Ttl: brtypes.CacheTTLFiveMinutes},
		{InputTokens: &in1h, Ttl: brtypes.CacheTTLOneHour},
	})
	if w5 != 20 || w1h != 4 {
		t.Fatalf("got (%d, %d), want (20, 4)", w5, w1h)
	}

	// An unrecognized TTL lands on the cheaper tier so a new TTL can never
	// over-bill.
	unknown := int32(7)
	w5, w1h = cacheWriteTiers([]brtypes.CacheDetail{{InputTokens: &unknown, Ttl: brtypes.CacheTTL("99h")}})
	if w5 != 7 || w1h != 0 {
		t.Fatalf("unknown TTL: got (%d, %d), want (7, 0)", w5, w1h)
	}
}

// Verified against live Bedrock ap-northeast-2 (2026-08-28): OpenAI and xAI
// models reject temperature/topP/stopSequences in InferenceConfig with a 400
// ValidationException ("This model doesn't support the temperature field"),
// and zai models reject stopSequences — so those keys must be stripped
// per-upstream, while models off the list keep every param untouched.
func TestStripUnsupportedInference(t *testing.T) {
	full := func() map[string]any {
		return map[string]any{
			"maxTokens": int64(256), "temperature": 1.0, "topP": 0.9,
			"stopSequences": []string{"END"},
		}
	}
	all := []string{"temperature", "topP", "stopSequences"}
	stopOnly := []string{"stopSequences"}
	keepSampling := []string{"maxTokens", "temperature", "topP"}
	cases := []struct {
		upstream string
		stripped []string
		kept     []string
	}{
		// OpenAI gpt-5.6 reasoning models reject every sampling param…
		{"global.openai.gpt-5.6-sol", all, []string{"maxTokens"}},
		{"global.openai.gpt-5.6-luna", all, []string{"maxTokens"}},
		{"global.openai.gpt-5.6-terra", all, []string{"maxTokens"}},
		// …but gpt-5.4/5.5 (Mantle-only) and gpt-oss ACCEPT sampling params —
		// neither "openai." nor "openai.gpt-5" is narrow enough.
		{"openai.gpt-5.4", nil, []string{"maxTokens", "temperature", "topP", "stopSequences"}},
		{"openai.gpt-5.5", nil, []string{"maxTokens", "temperature", "topP", "stopSequences"}},
		{"openai.gpt-oss-120b-1:0", stopOnly, keepSampling},
		{"global.xai.grok-4.6", all, []string{"maxTokens"}},
		{"zai.glm-5", stopOnly, keepSampling},
		{"zai.glm-4.7", stopOnly, keepSampling},
		{"google.gemma-3-27b-it", stopOnly, keepSampling},
		{"minimax.minimax-m2.5", stopOnly, keepSampling},
		{"moonshot.kimi-k2-thinking", stopOnly, keepSampling},
		{"moonshotai.kimi-k2.5", stopOnly, keepSampling},
		{"qwen.qwen3-coder-next", stopOnly, keepSampling},
		{"deepseek.v3.2", stopOnly, keepSampling},
		// deepseek r1 accepts all three — "deepseek.v" must not match it.
		{"us.deepseek.r1-v1:0", nil, []string{"maxTokens", "temperature", "topP", "stopSequences"}},
		// Claude goes through invoke_model (apiFor, bedrock.go), never this
		// path — and must stay untouched even if routed here explicitly.
		{"global.anthropic.claude-sonnet-5", nil, []string{"maxTokens", "temperature", "topP", "stopSequences"}},
	}
	for _, tc := range cases {
		inf := full()
		stripUnsupportedInference(tc.upstream, inf)
		for _, k := range tc.stripped {
			if _, has := inf[k]; has {
				t.Errorf("%s: %q not stripped", tc.upstream, k)
			}
		}
		for _, k := range tc.kept {
			if _, has := inf[k]; !has {
				t.Errorf("%s: %q wrongly stripped", tc.upstream, k)
			}
		}
	}
}

func TestCompleteConverseStripsUnsupportedInference(t *testing.T) {
	text := "ok"
	fc := &fakeConverser{resp: ConverseResponse{
		Content: []schema.ContentBlock{{Type: "text", Text: &text}}, StopReason: "end_turn",
	}}
	p := &provider{conv: fc, modelAPI: map[string]string{"global.openai.gpt-5.6-sol": "converse"}}
	raw := []byte(`{"model":"gpt-5.6-sol","max_tokens":64,"temperature":1,"top_p":0.9,"stop_sequences":["END"],"messages":[{"role":"user","content":"q"}]}`)
	if _, err := p.Complete(context.Background(), &providers.ProxyRequest{Model: "gpt-5.6-sol", Upstream: "global.openai.gpt-5.6-sol", RawBody: raw}); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"temperature", "topP", "stopSequences"} {
		if _, has := fc.gotReq.Inference[k]; has {
			t.Errorf("complete: %q reached the upstream request", k)
		}
	}
	if fc.gotReq.Inference["maxTokens"] != int64(64) {
		t.Errorf("complete: maxTokens wrongly stripped: %v", fc.gotReq.Inference["maxTokens"])
	}
}

func TestStreamConverseStripsUnsupportedInference(t *testing.T) {
	fc := &fakeConverser{streamEv: []ConverseStreamEvent{{Kind: eventMessageStop, StopReason: "end_turn"}, {Kind: eventUsage, InputTokens: 1, OutputTokens: 1}}}
	p := &provider{conv: fc, modelAPI: map[string]string{"global.xai.grok-4.6": "converse"}}
	raw := []byte(`{"model":"grok-4.6","max_tokens":64,"temperature":1,"stop_sequences":["END"],"stream":true,"messages":[{"role":"user","content":"q"}]}`)
	evs, err := p.Stream(context.Background(), &providers.ProxyRequest{Model: "grok-4.6", Upstream: "global.xai.grok-4.6", RawBody: raw})
	if err != nil {
		t.Fatal(err)
	}
	for range evs { // drain so the fake records the request
	}
	for _, k := range []string{"temperature", "stopSequences"} {
		if _, has := fc.gotReq.Inference[k]; has {
			t.Errorf("stream: %q reached the upstream request", k)
		}
	}
}

// Review follow-up (PR #65, round 2): a hook firing between an assistant
// tool_use and the user tool_result must not put the merged text BEFORE the
// tool_result — tool_result blocks must stay first in the user message, or
// the upstream rejects the shape.
func TestToConverseRequestSystemRoleMergesAfterToolResults(t *testing.T) {
	raw := []byte(`{"messages":[
		{"role":"user","content":"list files"},
		{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"bash","input":{"cmd":"ls"}}]},
		{"role":"system","content":"hook output mid-tool-turn"},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"a.go"},{"type":"text","text":"continue"}]}
	]}`)
	cr, err := toConverseRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	last := cr.Messages[len(cr.Messages)-1]
	if last.Role != "user" || len(last.Content) != 3 {
		t.Fatalf("unexpected last message: %+v", last)
	}
	if last.Content[0].Type != "tool_result" {
		t.Fatalf("tool_result must stay FIRST in the user message, got %q first", last.Content[0].Type)
	}
	if last.Content[1].Type != "text" || last.Content[1].Text == nil || !strings.Contains(*last.Content[1].Text, "hook output") {
		t.Fatalf("hook text must follow the tool_result blocks: %+v", last.Content[1])
	}
}

// Claude Code's /model switch sends a max_tokens:1 probe; gpt-5.6 and xai
// reject any maxTokens < 16 (live 2026-09-04). floorMaxTokens raises only the
// listed families, only when under the floor, and never touches others.
func TestFloorMaxTokens(t *testing.T) {
	cases := []struct {
		upstream string
		in       int64
		want     int64
	}{
		{"global.openai.gpt-5.6-sol", 1, 16},
		{"global.openai.gpt-5.6-luna", 15, 16},
		{"global.openai.gpt-5.6-sol", 16, 16},
		{"global.openai.gpt-5.6-sol", 4096, 4096},
		{"global.xai.grok-4.6", 1, 16},
		{"openai.gpt-5.4", 1, 1}, // Mantle-served, accepts 1 — unlisted
		{"zai.glm-5", 1, 1},      // accepts 1 — unlisted
		{"anthropic.claude-opus-5", 1, 1},
	}
	for _, tc := range cases {
		inf := map[string]any{"maxTokens": tc.in}
		floorMaxTokens(tc.upstream, inf)
		if got := inf["maxTokens"].(int64); got != tc.want {
			t.Errorf("%s maxTokens %d -> %d, want %d", tc.upstream, tc.in, got, tc.want)
		}
	}
	// No maxTokens key at all: nothing is invented.
	inf := map[string]any{}
	floorMaxTokens("global.openai.gpt-5.6-sol", inf)
	if _, has := inf["maxTokens"]; has {
		t.Error("floorMaxTokens must not invent a maxTokens key")
	}
}
