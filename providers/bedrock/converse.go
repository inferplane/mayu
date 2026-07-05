package bedrock

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
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
		blocks := messageBlocks(m)
		if len(blocks) == 0 {
			continue // Bedrock rejects a message with zero content blocks
		}
		cr.Messages = append(cr.Messages, ConverseMessage{Role: m.Role, Content: blocks})
	}
	cr.Tools = parseTools(body.Tools)
	cr.ToolChoice = parseToolChoice(body.ToolChoice)
	return cr, nil
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

// parseTools decodes Anthropic's "tools" array into Bedrock-shaped tool specs.
// A tool with no input_schema is skipped: Bedrock requires one, and
// server-tool shorthands (e.g. computer use) can't be expressed as a ToolSpec.
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
		if len(t.InputSchema) == 0 {
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
	cresp, err := p.conv.Converse(ctx, req.Upstream, cr)
	if err != nil {
		return nil, fmt.Errorf("bedrock: converse: %w", err)
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
	evs, err := p.conv.ConverseStream(ctx, req.Upstream, cr)
	if err != nil {
		return nil, fmt.Errorf("bedrock: converse stream: %w", err)
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
				if !emit(&schema.ChatChunk{Type: "content_block_stop", Index: &idx}) {
					return
				}
				blockOpen = false
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
		// The stream ended without pairing both events (e.g. no Metadata event
		// at all) — flush a terminal frame anyway so the client isn't left
		// hanging, with whatever stop reason/usage we did receive (zero usage
		// if none arrived).
		if stopReasonSet || usageSet {
			finish()
		}
	}, nil
}
