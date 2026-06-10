// Package openai converts between the OpenAI Chat Completions wire format and
// inferplane's canonical schema (an Anthropic-Messages superset, pkg/schema).
// Conversion is best-effort and centered on the cases Claude Code / OpenCode
// actually exercise: text, system prompts, tool calls (function calling), and
// token usage. Exotic multi-modal content is reduced to its text parts.
//
// Direction matters:
//   - RequestToCanonical / ResponseToCanonical / ChunkToCanonical parse an
//     OpenAI wire payload INTO canonical (used for governance observation and
//     OpenAI ingress).
//   - CanonicalToRequest / ResponseFromCanonical / ChunkFromCanonical render
//     canonical OUT to the OpenAI wire (used when an OpenAI client talks to an
//     Anthropic-native provider, or vice-versa).
//
// Lossiness (documented): canonical "thinking"/"redacted_thinking" blocks have
// no OpenAI equivalent and are DROPPED by CanonicalToRequest. The
// tool_use ↔ tool_calls and tool_result ↔ tool message round trips are lossless
// for the common function-calling case (arguments JSON string ↔ input object).
package openai

import (
	"encoding/json"
	"strings"

	"github.com/inferplane/inferplane/pkg/schema"
)

// finishToStop maps an OpenAI finish_reason to a canonical (Anthropic)
// stop_reason. Unknown values fall back to "end_turn".
var finishToStop = map[string]string{
	"stop":       "end_turn",
	"length":     "max_tokens",
	"tool_calls": "tool_use",
}

// stopToFinish is the inverse of finishToStop. Unknown values fall back to "stop".
var stopToFinish = map[string]string{
	"end_turn":   "stop",
	"max_tokens": "length",
	"tool_use":   "tool_calls",
}

func mapFinishToStop(finish string) string {
	if s, ok := finishToStop[finish]; ok {
		return s
	}
	return "end_turn"
}

func mapStopToFinish(stop string) string {
	if f, ok := stopToFinish[stop]; ok {
		return f
	}
	return "stop"
}

// ── OpenAI wire shapes (only the fields we map) ──────────────────────────────

type oaiToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Index    *int   `json:"index,omitempty"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type oaiMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	Name       string          `json:"name,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	ToolCalls  []oaiToolCall   `json:"tool_calls,omitempty"`
}

type oaiRequest struct {
	Model               string          `json:"model"`
	Messages            []oaiMessage    `json:"messages"`
	MaxTokens           *int64          `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int64          `json:"max_completion_tokens,omitempty"`
	Stream              *bool           `json:"stream,omitempty"`
	Temperature         json.RawMessage `json:"temperature,omitempty"`
	TopP                json.RawMessage `json:"top_p,omitempty"`
	Tools               json.RawMessage `json:"tools,omitempty"`
}

type oaiUsage struct {
	PromptTokens     *int64 `json:"prompt_tokens,omitempty"`
	CompletionTokens *int64 `json:"completion_tokens,omitempty"`
	TotalTokens      *int64 `json:"total_tokens,omitempty"`
}

type oaiRespMessage struct {
	Role      string        `json:"role"`
	Content   *string       `json:"content"`
	ToolCalls []oaiToolCall `json:"tool_calls,omitempty"`
}

type oaiResponse struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int            `json:"index"`
		Message      oaiRespMessage `json:"message"`
		FinishReason *string        `json:"finish_reason"`
	} `json:"choices"`
	Usage *oaiUsage `json:"usage,omitempty"`
}

// ── helpers ──────────────────────────────────────────────────────────────────

func strPtr(s string) *string { return &s }

// oaiContentToText flattens an OpenAI message content value (string OR an array
// of content parts) into a plain string. Non-text parts are ignored.
func oaiContentToText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
		return ""
	}
	if raw[0] == '[' {
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(raw, &parts) == nil {
			var b strings.Builder
			for _, p := range parts {
				if p.Type == "text" || p.Text != "" {
					b.WriteString(p.Text)
				}
			}
			return b.String()
		}
	}
	return ""
}

// textBlock builds a canonical text content block.
func textBlock(s string) schema.ContentBlock {
	return schema.ContentBlock{Type: "text", Text: strPtr(s)}
}

// ── OpenAI → canonical request ───────────────────────────────────────────────

// RequestToCanonical parses an OpenAI Chat Completions request body into the
// canonical schema. system messages collapse into cr.System (a JSON array of
// {type:text,text} blocks); user/assistant/tool messages become cr.Messages.
// assistant tool_calls become tool_use blocks (arguments string → input
// object); tool messages become a user message carrying a tool_result block.
// max_tokens || max_completion_tokens → cr.MaxTokens; temperature/top_p are
// carried verbatim in cr.Extra.
func RequestToCanonical(openaiBody []byte) (*schema.ChatRequest, error) {
	var in oaiRequest
	if err := json.Unmarshal(openaiBody, &in); err != nil {
		return nil, err
	}

	cr := &schema.ChatRequest{Model: in.Model}
	if in.MaxTokens != nil {
		cr.MaxTokens = in.MaxTokens
	} else if in.MaxCompletionTokens != nil {
		cr.MaxTokens = in.MaxCompletionTokens
	}
	if in.Stream != nil {
		cr.Stream = in.Stream
	}

	var systemBlocks []schema.ContentBlock
	for _, m := range in.Messages {
		switch m.Role {
		case "system", "developer":
			systemBlocks = append(systemBlocks, textBlock(oaiContentToText(m.Content)))
		case "tool":
			// A tool result is its own canonical block inside a user message.
			block := schema.ContentBlock{
				Type:      "tool_result",
				ToolUseID: m.ToolCallID,
			}
			if content := oaiContentToText(m.Content); content != "" {
				block.Content = mustJSON(content)
			}
			cr.Messages = append(cr.Messages, schema.Message{
				Role:    "user",
				Content: []schema.ContentBlock{block},
			})
		case "user", "assistant":
			var blocks []schema.ContentBlock
			if text := oaiContentToText(m.Content); text != "" {
				blocks = append(blocks, textBlock(text))
			}
			for _, tc := range m.ToolCalls {
				input := json.RawMessage(tc.Function.Arguments)
				if len(input) == 0 || !json.Valid(input) {
					input = json.RawMessage(`{}`)
				}
				blocks = append(blocks, schema.ContentBlock{
					Type:  "tool_use",
					ID:    tc.ID,
					Name:  tc.Function.Name,
					Input: input,
				})
			}
			if len(blocks) == 0 {
				// assistant content may be null with no tool_calls — keep an
				// empty text block so the turn is not lost.
				blocks = append(blocks, textBlock(""))
			}
			cr.Messages = append(cr.Messages, schema.Message{Role: m.Role, Content: blocks})
		}
	}

	if len(systemBlocks) > 0 {
		if raw, err := json.Marshal(systemBlocks); err == nil {
			cr.System = raw
		}
	}
	if len(in.Tools) > 0 {
		cr.Tools = in.Tools
	}

	extra := map[string]json.RawMessage{}
	if len(in.Temperature) > 0 {
		extra["temperature"] = in.Temperature
	}
	if len(in.TopP) > 0 {
		extra["top_p"] = in.TopP
	}
	if len(extra) > 0 {
		cr.Extra = extra
	}
	return cr, nil
}

func mustJSON(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`null`)
	}
	return raw
}

// ── canonical → OpenAI request ───────────────────────────────────────────────

// CanonicalToRequest renders a canonical request to an OpenAI Chat Completions
// body, for the Anthropic-ingress → openai_compatible-provider path. text and
// tool blocks map back; canonical thinking blocks are DROPPED (no OpenAI
// equivalent). cr.System becomes a leading OpenAI system message.
func CanonicalToRequest(cr *schema.ChatRequest) []byte {
	out := map[string]any{"model": cr.Model}
	if cr.MaxTokens != nil {
		out["max_tokens"] = *cr.MaxTokens
	}
	if cr.Stream != nil {
		out["stream"] = *cr.Stream
	}

	var msgs []map[string]any

	// system blocks → a single OpenAI system message.
	if len(cr.System) > 0 {
		if sys := systemToText(cr.System); sys != "" {
			msgs = append(msgs, map[string]any{"role": "system", "content": sys})
		}
	}

	for _, m := range cr.Messages {
		switch m.Role {
		case "user":
			// A user message that is purely tool_result blocks becomes one or
			// more OpenAI tool messages; otherwise a normal user message.
			var toolResults []schema.ContentBlock
			var textParts []string
			for _, b := range m.Content {
				switch b.Type {
				case "tool_result":
					toolResults = append(toolResults, b)
				case "text":
					if b.Text != nil {
						textParts = append(textParts, *b.Text)
					}
				}
			}
			for _, tr := range toolResults {
				msgs = append(msgs, map[string]any{
					"role":         "tool",
					"tool_call_id": tr.ToolUseID,
					"content":      toolResultContentToText(tr.Content),
				})
			}
			if len(textParts) > 0 || len(toolResults) == 0 {
				msgs = append(msgs, map[string]any{"role": "user", "content": strings.Join(textParts, "")})
			}
		case "assistant":
			var textParts []string
			var toolCalls []map[string]any
			for _, b := range m.Content {
				switch b.Type {
				case "text":
					if b.Text != nil {
						textParts = append(textParts, *b.Text)
					}
				case "tool_use":
					args := "{}"
					if len(b.Input) > 0 {
						args = string(b.Input)
					}
					toolCalls = append(toolCalls, map[string]any{
						"id":   b.ID,
						"type": "function",
						"function": map[string]any{
							"name":      b.Name,
							"arguments": args,
						},
					})
					// thinking / redacted_thinking blocks: dropped.
				}
			}
			am := map[string]any{"role": "assistant"}
			if len(textParts) > 0 {
				am["content"] = strings.Join(textParts, "")
			} else {
				am["content"] = nil
			}
			if len(toolCalls) > 0 {
				am["tool_calls"] = toolCalls
			}
			msgs = append(msgs, am)
		}
	}

	out["messages"] = msgs
	raw, _ := json.Marshal(out)
	return raw
}

// systemToText flattens cr.System (string OR array of text blocks) into text.
func systemToText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
		return ""
	}
	var blocks []schema.ContentBlock
	if json.Unmarshal(raw, &blocks) == nil {
		var b strings.Builder
		for _, blk := range blocks {
			if blk.Type == "text" && blk.Text != nil {
				b.WriteString(*blk.Text)
			}
		}
		return b.String()
	}
	return ""
}

// toolResultContentToText flattens a tool_result.content value (string OR
// block array) into a plain string for an OpenAI tool message.
func toolResultContentToText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
		return ""
	}
	if raw[0] == '[' {
		var blocks []schema.ContentBlock
		if json.Unmarshal(raw, &blocks) == nil {
			var b strings.Builder
			for _, blk := range blocks {
				if blk.Text != nil {
					b.WriteString(*blk.Text)
				}
			}
			return b.String()
		}
	}
	// object or other: serialize as-is.
	return string(raw)
}

// ── canonical → OpenAI response ──────────────────────────────────────────────

// ResponseFromCanonical renders a canonical (Anthropic-shaped) response to an
// OpenAI chat.completion object. text blocks concatenate into message.content;
// tool_use blocks become tool_calls; stop_reason → finish_reason; usage maps
// input/output → prompt/completion tokens.
func ResponseFromCanonical(resp *schema.ChatResponse) []byte {
	var content strings.Builder
	var toolCalls []map[string]any
	for _, b := range resp.Content {
		switch b.Type {
		case "text":
			if b.Text != nil {
				content.WriteString(*b.Text)
			}
		case "tool_use":
			args := "{}"
			if len(b.Input) > 0 {
				args = string(b.Input)
			}
			toolCalls = append(toolCalls, map[string]any{
				"id":   b.ID,
				"type": "function",
				"function": map[string]any{
					"name":      b.Name,
					"arguments": args,
				},
			})
		}
	}

	msg := map[string]any{"role": "assistant", "content": content.String()}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}

	finish := "stop"
	if resp.StopReason != nil {
		finish = mapStopToFinish(*resp.StopReason)
	}

	out := map[string]any{
		"id":      resp.ID,
		"object":  "chat.completion",
		"model":   resp.Model,
		"choices": []map[string]any{{"index": 0, "message": msg, "finish_reason": finish}},
	}
	if u := usageFromCanonical(resp.Usage); u != nil {
		out["usage"] = u
	}
	raw, _ := json.Marshal(out)
	return raw
}

func usageFromCanonical(u *schema.Usage) map[string]any {
	if u == nil {
		return nil
	}
	var prompt, completion int64
	if u.InputTokens != nil {
		prompt = *u.InputTokens
	}
	if u.OutputTokens != nil {
		completion = *u.OutputTokens
	}
	return map[string]any{
		"prompt_tokens":     prompt,
		"completion_tokens": completion,
		"total_tokens":      prompt + completion,
	}
}

// ── OpenAI → canonical response (observation) ────────────────────────────────

// ResponseToCanonical parses a vLLM/OpenAI chat.completion response into the
// canonical schema, for governance observation. choices[0].message becomes
// content blocks (text + tool_use from tool_calls); finish_reason →
// stop_reason; usage maps prompt/completion → input/output tokens.
func ResponseToCanonical(openaiBody []byte) (*schema.ChatResponse, error) {
	var in oaiResponse
	if err := json.Unmarshal(openaiBody, &in); err != nil {
		return nil, err
	}
	resp := &schema.ChatResponse{
		ID:    in.ID,
		Type:  "message",
		Role:  "assistant",
		Model: in.Model,
	}
	if len(in.Choices) > 0 {
		c := in.Choices[0]
		resp.Role = c.Message.Role
		if resp.Role == "" {
			resp.Role = "assistant"
		}
		if c.Message.Content != nil && *c.Message.Content != "" {
			resp.Content = append(resp.Content, textBlock(*c.Message.Content))
		}
		for _, tc := range c.Message.ToolCalls {
			input := json.RawMessage(tc.Function.Arguments)
			if len(input) == 0 || !json.Valid(input) {
				input = json.RawMessage(`{}`)
			}
			resp.Content = append(resp.Content, schema.ContentBlock{
				Type:  "tool_use",
				ID:    tc.ID,
				Name:  tc.Function.Name,
				Input: input,
			})
		}
		if c.FinishReason != nil {
			resp.StopReason = strPtr(mapFinishToStop(*c.FinishReason))
		}
	}
	if resp.Content == nil {
		resp.Content = []schema.ContentBlock{}
	}
	if in.Usage != nil {
		resp.Usage = &schema.Usage{
			InputTokens:  in.Usage.PromptTokens,
			OutputTokens: in.Usage.CompletionTokens,
		}
	}
	return resp, nil
}

// ── streaming ────────────────────────────────────────────────────────────────

// StreamState carries the minimal cross-event state OpenAI streaming needs:
// whether the leading role delta was already emitted.
type StreamState struct {
	roleSent bool
	id       string
	model    string
}

// ChunkFromCanonical renders one canonical (Anthropic) streaming event to an
// OpenAI chat.completion.chunk. Events with no OpenAI equivalent (ping,
// content_block_start, content_block_stop, message_stop) return nil — the
// caller appends the terminal [DONE] line itself.
func ChunkFromCanonical(c *schema.ChatChunk, st *StreamState) []byte {
	switch c.Type {
	case "message_start":
		if c.Message != nil {
			if st.id == "" {
				st.id = c.Message.ID
			}
			if st.model == "" {
				st.model = c.Message.Model
			}
		}
		// Emit the opening role delta once.
		if !st.roleSent {
			st.roleSent = true
			return chunkJSON(st, map[string]any{"role": "assistant"}, nil)
		}
		return nil

	case "content_block_delta":
		var d struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		_ = json.Unmarshal(c.Delta, &d)
		switch d.Type {
		case "text_delta":
			delta := map[string]any{"content": d.Text}
			if !st.roleSent {
				st.roleSent = true
				delta["role"] = "assistant"
			}
			return chunkJSON(st, delta, nil)
		case "input_json_delta":
			// Tool-call argument streaming. Emit a tool_calls delta carrying
			// the partial arguments string for the block at this index.
			var pj struct {
				PartialJSON string `json:"partial_json"`
			}
			_ = json.Unmarshal(c.Delta, &pj)
			idx := 0
			if c.Index != nil {
				idx = *c.Index
			}
			tc := map[string]any{
				"index":    idx,
				"function": map[string]any{"arguments": pj.PartialJSON},
			}
			delta := map[string]any{"tool_calls": []map[string]any{tc}}
			if !st.roleSent {
				st.roleSent = true
				delta["role"] = "assistant"
			}
			return chunkJSON(st, delta, nil)
		}
		return nil

	case "content_block_start":
		// A tool_use block start carries id/name — emit a tool_calls delta with them.
		if c.ContentBlock != nil && c.ContentBlock.Type == "tool_use" {
			idx := 0
			if c.Index != nil {
				idx = *c.Index
			}
			tc := map[string]any{
				"index": idx,
				"id":    c.ContentBlock.ID,
				"type":  "function",
				"function": map[string]any{
					"name":      c.ContentBlock.Name,
					"arguments": "",
				},
			}
			delta := map[string]any{"tool_calls": []map[string]any{tc}}
			if !st.roleSent {
				st.roleSent = true
				delta["role"] = "assistant"
			}
			return chunkJSON(st, delta, nil)
		}
		return nil

	case "message_delta":
		var d struct {
			StopReason *string `json:"stop_reason"`
		}
		_ = json.Unmarshal(c.Delta, &d)
		finish := "stop"
		if d.StopReason != nil {
			finish = mapStopToFinish(*d.StopReason)
		}
		return chunkJSON(st, map[string]any{}, &finish)

	default:
		// message_stop, ping, error, content_block_stop: no OpenAI chunk.
		return nil
	}
}

// chunkJSON builds a single OpenAI chat.completion.chunk with one choice.
func chunkJSON(st *StreamState, delta map[string]any, finishReason *string) []byte {
	choice := map[string]any{"index": 0, "delta": delta}
	if finishReason != nil {
		choice["finish_reason"] = *finishReason
	} else {
		choice["finish_reason"] = nil
	}
	out := map[string]any{
		"id":      st.id,
		"object":  "chat.completion.chunk",
		"model":   st.model,
		"choices": []map[string]any{choice},
	}
	raw, _ := json.Marshal(out)
	return raw
}

// ChunkToCanonical parses one vLLM/OpenAI stream chunk into a canonical
// ChatChunk for governance observation. delta.content → a content_block_delta
// text_delta; finish_reason → a message_delta carrying stop_reason; usage (if
// present, e.g. with stream_options.include_usage) is attached.
func ChunkToCanonical(openaiChunk []byte) (*schema.ChatChunk, error) {
	var in struct {
		Choices []struct {
			Index int `json:"index"`
			Delta struct {
				Content *string `json:"content"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage *oaiUsage `json:"usage,omitempty"`
	}
	if err := json.Unmarshal(openaiChunk, &in); err != nil {
		return nil, err
	}

	var usage *schema.Usage
	if in.Usage != nil {
		usage = &schema.Usage{
			InputTokens:  in.Usage.PromptTokens,
			OutputTokens: in.Usage.CompletionTokens,
		}
	}

	if len(in.Choices) > 0 {
		ch := in.Choices[0]
		if ch.FinishReason != nil {
			delta := mustJSON(map[string]any{"stop_reason": mapFinishToStop(*ch.FinishReason)})
			return &schema.ChatChunk{Type: "message_delta", Delta: delta, Usage: usage}, nil
		}
		if ch.Delta.Content != nil {
			idx := ch.Index
			delta := mustJSON(map[string]any{"type": "text_delta", "text": *ch.Delta.Content})
			return &schema.ChatChunk{Type: "content_block_delta", Index: &idx, Delta: delta, Usage: usage}, nil
		}
	}

	if usage != nil {
		// A usage-only chunk (include_usage) with no choice deltas.
		return &schema.ChatChunk{Type: "message_delta", Delta: json.RawMessage(`{}`), Usage: usage}, nil
	}
	return nil, nil
}
