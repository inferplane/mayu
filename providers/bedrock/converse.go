package bedrock

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"regexp"
	"strings"

	"github.com/inferplane/inferplane/pkg/schema"
	"github.com/inferplane/inferplane/providers"
)

func toConverseRequest(raw []byte) (ConverseRequest, error) {
	var body struct {
		MaxTokens     *int64           `json:"max_tokens"`
		Temperature   *float64         `json:"temperature"`
		TopP          *float64         `json:"top_p"`
		StopSequences []string         `json:"stop_sequences"`
		System        json.RawMessage  `json:"system"`
		Messages      []schema.Message `json:"messages"`
		Tools         json.RawMessage  `json:"tools"`
		ToolChoice    json.RawMessage  `json:"tool_choice"`
		ModelFields   map[string]any   `json:"model_fields"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return ConverseRequest{}, err
	}
	cr := ConverseRequest{Inference: map[string]any{}, ModelFields: body.ModelFields}
	if body.MaxTokens != nil {
		cr.Inference["maxTokens"] = *body.MaxTokens
	}
	// Sampling params pass through to InferenceConfig. Keys match the names
	// buildInference (client.go) reads: temperature, topP, stopSequences.
	if body.Temperature != nil {
		cr.Inference["temperature"] = *body.Temperature
	}
	if body.TopP != nil {
		cr.Inference["topP"] = *body.TopP
	}
	if len(body.StopSequences) > 0 {
		cr.Inference["stopSequences"] = body.StopSequences
	}
	cr.System = systemText(body.System)
	for _, m := range body.Messages {
		if m.Role != "user" && m.Role != "assistant" {
			// Bedrock's ConversationRole only has user/assistant. Real Claude
			// Code traffic interleaves other roles (observed: "system", for
			// hook/session-start output) as ordinary messages — Anthropic's
			// API tolerates this, but Bedrock rejects it outright and, since
			// it's usually the LAST message, "role must be user/assistant"
			// surfaces as the more confusing "last turn must be a user
			// message". Fold the text into the system prompt instead of
			// dropping it or passing an invalid role through.
			if t := flattenText(m.Content); t != "" {
				if cr.System != "" {
					cr.System += "\n\n" + t
				} else {
					cr.System = t
				}
			}
			continue
		}
		blocks := messageBlocks(m)
		if len(blocks) == 0 {
			continue // Bedrock rejects a message with zero content blocks
		}
		cr.Messages = append(cr.Messages, ConverseMessage{Role: m.Role, Content: blocks})
	}
	cr.Tools = parseTools(body.Tools)
	cr.ToolChoice = resolveToolChoice(parseToolChoice(body.ToolChoice), cr.Tools)
	return cr, nil
}

// resolveToolChoice drops a "tool" choice that points at a tool parseTools
// already dropped (schema-less, empty-named, over bedrockToolNameMax, or an
// invalid-charset name) —
// otherwise buildToolConfig would send Bedrock a SpecificToolChoice for a
// tool absent from the tool list, which Bedrock rejects with a
// ValidationException. Falling back to the zero value leaves ToolChoice
// unset on the Bedrock call (buildToolConfig, client.go), i.e. auto.
func resolveToolChoice(choice ConverseToolChoice, tools []ConverseTool) ConverseToolChoice {
	if choice.Type != "tool" {
		return choice
	}
	for _, t := range tools {
		if t.Name == choice.Name {
			return choice
		}
	}
	return ConverseToolChoice{}
}

// systemText extracts plain text from an Anthropic system field (string or
// array of text blocks). M4: text only.
func systemText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if raw[0] == '"' {
		var s string
		json.Unmarshal(raw, &s)
		return s
	}
	var blocks []schema.ContentBlock
	json.Unmarshal(raw, &blocks)
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != nil {
			parts = append(parts, *b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// flattenText concatenates a message's text blocks, ignoring tool_use/
// tool_result/thinking/image — used only for folding a non-user/assistant
// role's content into the system prompt (see toConverseRequest).
func flattenText(blocks []schema.ContentBlock) string {
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != nil {
			parts = append(parts, *b.Text)
		}
	}
	return strings.Join(parts, "")
}

// messageBlocks keeps only the block types Bedrock Converse can represent
// (text, tool_use, tool_result); thinking/image/document blocks are dropped
// (out of scope — see the design doc). The SDK-specific mapping, including
// tool_result flattening, happens in buildMessages (client.go).
func messageBlocks(m schema.Message) []schema.ContentBlock {
	var out []schema.ContentBlock
	for _, b := range m.Content {
		switch b.Type {
		case "text", "tool_use", "tool_result":
			out = append(out, b)
		}
	}
	return out
}

// bedrockToolNameMax is Bedrock's ToolSpecification.Name limit (a
// ValidationException, not a soft truncation). Anthropic allows tool names up
// to 128 chars — long MCP-qualified names (e.g.
// "mcp__plugin_foo_bar__some_long_action") are common there and routinely
// exceed 64. Truncating would still break correctly: the client maps a
// tool_use response back to its local tool registry by exact name, so a
// truncated name it doesn't recognize can never be executed. Dropping the
// tool is the same trade-off already made for schema-less tools below — the
// model loses that one capability instead of the whole request failing.
const bedrockToolNameMax = 64

// bedrockToolNameRE is Bedrock's ToolSpecification.Name charset: it must start
// with a letter and contain only letters, digits, and underscores — no
// hyphens, dots, colons, or spaces. Claude Code / MCP tool names routinely
// contain hyphens (e.g. "mcp__aws-sdk-v3__getObject"), which pass every other
// check here but make Bedrock reject the WHOLE request with a
// ValidationException. Same drop-and-continue trade-off as the oversized-name
// case below.
var bedrockToolNameRE = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]*$`)

// parseTools decodes Anthropic's "tools" array into Bedrock-shaped tool specs,
// skipping any tool Bedrock would reject outright: no input_schema (server-tool
// shorthands like computer use can't be expressed as a ToolSpec), an empty,
// over-length, or invalid-charset name, or a JSON "null" input_schema. A
// dropped tool's tool_choice reference is cleaned up separately by
// resolveToolChoice.
func parseTools(raw json.RawMessage) []ConverseTool {
	if len(raw) == 0 {
		return nil
	}
	var tools []struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"input_schema"`
	}
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil
	}
	var out []ConverseTool
	for _, t := range tools {
		// Bedrock's ToolSpecification.Name/InputSchema are both required and
		// non-null: an empty name, a name over the length or charset limit, a
		// missing input_schema, or a JSON "null" input_schema (still 4 bytes,
		// so a bare len==0 check misses it) all produce a ValidationException
		// — skip the tool rather than send one Bedrock will reject.
		if t.Name == "" || len(t.Name) > bedrockToolNameMax || !bedrockToolNameRE.MatchString(t.Name) ||
			len(t.InputSchema) == 0 || string(t.InputSchema) == "null" {
			continue
		}
		out = append(out, ConverseTool{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema})
	}
	return out
}

// parseToolChoice decodes Anthropic's tool_choice object. An absent or
// unrecognized value yields the zero ConverseToolChoice, which
// buildToolConfig (client.go) leaves unset on the Bedrock call.
func parseToolChoice(raw json.RawMessage) ConverseToolChoice {
	if len(raw) == 0 {
		return ConverseToolChoice{}
	}
	var tc struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &tc); err != nil {
		return ConverseToolChoice{}
	}
	return ConverseToolChoice{Type: tc.Type, Name: tc.Name}
}

func (p *provider) completeConverse(ctx context.Context, req *providers.ProxyRequest) (*providers.ProxyResponse, error) {
	cr, err := toConverseRequest(req.RawBody)
	if err != nil {
		return nil, fmt.Errorf("bedrock: converse req: %w", err)
	}
	cr.Guardrail = p.guardrailFor(req)
	cresp, err := p.conv.Converse(ctx, req.Upstream, cr)
	if err != nil {
		// Classify the SDK error into its real upstream status — see errors.go.
		return nil, upstreamError(err)
	}
	stop := cresp.StopReason
	in, out := cresp.InputTokens, cresp.OutputTokens
	content := cresp.Content
	if content == nil {
		content = []schema.ContentBlock{} // never emit "content":null (real API always sends an array)
	}
	resp := &schema.ChatResponse{
		Type: "message", Role: "assistant", Model: req.Model,
		Content:    content,
		StopReason: &stop,
		Usage:      &schema.Usage{InputTokens: &in, OutputTokens: &out},
	}
	rawBody, _ := json.Marshal(resp)
	return &providers.ProxyResponse{StatusCode: 200, RawBody: rawBody, Parsed: resp}, nil
}

// streamConverse replays Bedrock's Converse event stream as Anthropic SSE. It
// tracks one open content block at a time (lazily opened on the first delta
// that needs it) and re-indexes blocks sequentially, ignoring Bedrock's own
// ContentBlockIndex. The terminal frame (message_delta + message_stop) is only
// emitted once BOTH the stop reason (MessageStop) and usage (a later Metadata
// event) have arrived — Bedrock delivers Metadata strictly after MessageStop,
// so waiting for whichever comes last (instead of returning on MessageStop)
// is what makes streamed usage land in the audit/billing record at all.
func (p *provider) streamConverse(ctx context.Context, req *providers.ProxyRequest) (iter.Seq2[*providers.StreamEvent, error], error) {
	cr, err := toConverseRequest(req.RawBody)
	if err != nil {
		return nil, fmt.Errorf("bedrock: converse req: %w", err)
	}
	cr.Guardrail = p.guardrailFor(req)
	evs, err := p.conv.ConverseStream(ctx, req.Upstream, cr)
	if err != nil {
		// Pre-TTFT: the stream never opened, so its real status can still be
		// teed to the client (see errors.go).
		return nil, upstreamError(err)
	}
	return func(yield func(*providers.StreamEvent, error) bool) {
		emit := func(c *schema.ChatChunk) bool {
			var b strings.Builder
			schema.WriteAnthropicSSE(&b, c)
			return yield(&providers.StreamEvent{Raw: []byte(b.String()), Chunk: c}, nil)
		}
		if !emit(&schema.ChatChunk{Type: "message_start", Message: &schema.ChatResponse{Type: "message", Role: "assistant", Model: req.Model}}) {
			return
		}

		idx := -1
		blockOpen := false
		var stopReason string
		stopReasonSet := false
		var usageIn, usageOut int64
		usageSet := false

		finish := func() {
			if blockOpen {
				// Mark closed before emitting: finish is only ever called
				// once (every call site returns right after), but leaving
				// blockOpen stale on an early return here — if the consumer
				// stops iterating mid-emit — is a needless hidden invariant.
				blockOpen = false
				if !emit(&schema.ChatChunk{Type: "content_block_stop", Index: &idx}) {
					// Cannot fall through to message_delta/message_stop here:
					// emit's false means yield already returned false once,
					// and Go's range-over-func contract panics if the loop
					// body calls yield again afterward ("range function
					// continued iteration after function for loop body
					// returned false"). A consumer that stopped mid-stream
					// gets NO further frames from this or any other
					// provider — this is the iterator protocol, not a gap
					// specific to usage settlement. Nothing to do but return.
					return
				}
			}
			in, out := usageIn, usageOut
			delta, _ := json.Marshal(map[string]any{"stop_reason": stopReason, "stop_sequence": nil})
			if !emit(&schema.ChatChunk{Type: "message_delta", Delta: delta, Usage: &schema.Usage{InputTokens: &in, OutputTokens: &out}}) {
				return
			}
			emit(&schema.ChatChunk{Type: "message_stop"})
		}

		for e, eerr := range evs {
			if eerr != nil {
				yield(nil, eerr)
				return
			}
			switch e.Kind {
			case eventTextDelta:
				if !blockOpen {
					idx++
					empty := ""
					if !emit(&schema.ChatChunk{Type: "content_block_start", Index: &idx, ContentBlock: &schema.ContentBlock{Type: "text", Text: &empty}}) {
						return
					}
					blockOpen = true
				}
				delta, _ := json.Marshal(map[string]any{"type": "text_delta", "text": e.TextDelta})
				if !emit(&schema.ChatChunk{Type: "content_block_delta", Index: &idx, Delta: delta}) {
					return
				}
			case eventToolUseStart:
				if blockOpen {
					if !emit(&schema.ChatChunk{Type: "content_block_stop", Index: &idx}) {
						return
					}
				}
				idx++
				blockOpen = true
				if !emit(&schema.ChatChunk{Type: "content_block_start", Index: &idx, ContentBlock: &schema.ContentBlock{
					Type: "tool_use", ID: e.ToolUseID, Name: e.ToolName, Input: json.RawMessage("{}"),
				}}) {
					return
				}
			case eventToolInputDelta:
				if idx < 0 {
					// A tool-input delta before any ToolUseStart/text block has
					// opened one — malformed/truncated upstream stream. Emitting
					// content_block_delta with index:-1 would hand the client an
					// invalid SSE frame; discard the orphaned delta instead.
					continue
				}
				delta, _ := json.Marshal(map[string]any{"type": "input_json_delta", "partial_json": e.ToolDelta})
				if !emit(&schema.ChatChunk{Type: "content_block_delta", Index: &idx, Delta: delta}) {
					return
				}
			case eventBlockStop:
				if blockOpen {
					if !emit(&schema.ChatChunk{Type: "content_block_stop", Index: &idx}) {
						return
					}
					blockOpen = false
				}
			case eventMessageStop:
				stopReason = e.StopReason
				stopReasonSet = true
				if usageSet {
					finish()
					return
				}
			case eventUsage:
				usageIn, usageOut = e.InputTokens, e.OutputTokens
				usageSet = true
				if stopReasonSet {
					finish()
					return
				}
			}
		}
		// Reaching here means the event channel closed before both
		// MessageStop and Metadata arrived (e.g. no Metadata event at all, or
		// — in principle — a clean close with neither). Flush a terminal
		// frame unconditionally so the client is never left hanging with no
		// message_delta/message_stop at all; whichever of stop
		// reason/usage we didn't receive defaults to its zero value, the
		// same safe default already used for the no-Metadata case.
		finish()
	}, nil
}
