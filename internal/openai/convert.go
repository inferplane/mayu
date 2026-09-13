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
	Stop                json.RawMessage `json:"stop,omitempty"`
	Tools               json.RawMessage `json:"tools,omitempty"`
	ToolChoice          json.RawMessage `json:"tool_choice,omitempty"`
}

type oaiUsage struct {
	PromptTokens        *int64                  `json:"prompt_tokens,omitempty"`
	CompletionTokens    *int64                  `json:"completion_tokens,omitempty"`
	TotalTokens         *int64                  `json:"total_tokens,omitempty"`
	PromptTokensDetails *oaiPromptTokensDetails `json:"prompt_tokens_details,omitempty"`
}

// oaiPromptTokensDetails carries the prompt-cache breakdown. OpenAI's
// prompt_tokens INCLUDES cached_tokens, so mapping prompt_tokens straight to
// InputTokens billed every cached prompt token at the full input rate instead
// of the ~10x cheaper cache-read rate — over-billing, the opposite direction
// from the streaming/Bedrock defects (ADR-030).
type oaiPromptTokensDetails struct {
	CachedTokens *int64 `json:"cached_tokens,omitempty"`
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
	// tools/tool_choice are CONVERTED to the canonical (Anthropic) shape,
	// mirroring CanonicalToRequest's write side — a raw copy left them in
	// OpenAI shape, which the anthropic-shaped consumers (Bedrock
	// Converse/Mantle) then dropped wholesale: an OpenAI client's tool
	// calling silently vanished on every cross-protocol target.
	if tools := toolsFromOAI(in.Tools); tools != nil {
		cr.Tools = tools
	}
	if tc := toolChoiceFromOAI(in.ToolChoice); tc != nil {
		cr.ToolChoice = tc
	}

	extra := map[string]json.RawMessage{}
	if len(in.Temperature) > 0 {
		extra["temperature"] = in.Temperature
	}
	if len(in.TopP) > 0 {
		extra["top_p"] = in.TopP
	}
	// OpenAI stop may be a string or an array; canonical stop_sequences is an
	// array. Anything else — null, booleans, numbers, objects — is dropped,
	// never forwarded: no valid stop_sequences value has those shapes, and
	// forwarding one 400s downstream (on count_tokens, a non-200 crashes
	// Claude Code — CLAUDE.md invariant).
	if len(in.Stop) > 0 {
		if in.Stop[0] == '"' {
			if arr, err := json.Marshal([]json.RawMessage{in.Stop}); err == nil {
				extra["stop_sequences"] = arr
			}
		} else if in.Stop[0] == '[' {
			extra["stop_sequences"] = in.Stop
		}
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
	// Sampling params ride in Extra (canonical keeps them untyped). OpenAI's
	// names match except stop_sequences → stop.
	for canonical, oai := range map[string]string{
		"temperature": "temperature", "top_p": "top_p", "stop_sequences": "stop",
	} {
		if v, ok := cr.Extra[canonical]; ok && len(v) > 0 {
			out[oai] = json.RawMessage(v)
		}
	}
	if tools := toolsToOAI(cr.Tools); tools != nil {
		out["tools"] = tools
	}
	if tc := toolChoiceToOAI(cr.ToolChoice); tc != nil {
		out["tool_choice"] = tc
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

// toolsToOAI maps Anthropic tool definitions ({name, description,
// input_schema}) to OpenAI function tools ({type:"function",
// function:{name, description, parameters}}). Nil on empty or unparsable
// input — the field is then omitted rather than sent malformed.
func toolsToOAI(raw json.RawMessage) []map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var tools []struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		InputSchema json.RawMessage `json:"input_schema,omitempty"`
		Strict      *bool           `json:"strict,omitempty"`
	}
	if json.Unmarshal(raw, &tools) != nil || len(tools) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		if t.Name == "" {
			continue
		}
		fn := map[string]any{"name": t.Name}
		if t.Description != "" {
			fn["description"] = t.Description
		}
		if len(t.InputSchema) > 0 {
			fn["parameters"] = json.RawMessage(t.InputSchema)
		}
		if t.Strict != nil {
			fn["strict"] = *t.Strict
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// toolChoiceToOAI maps an Anthropic tool_choice to the OpenAI wire:
// auto → "auto", any → "required", none → "none",
// {type:"tool", name:N} → {type:"function", function:{name:N}}.
func toolChoiceToOAI(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var tc struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &tc) != nil {
		return nil
	}
	switch tc.Type {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool":
		if tc.Name == "" {
			return nil
		}
		return map[string]any{"type": "function", "function": map[string]any{"name": tc.Name}}
	}
	return nil
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
	// Re-expose the cache split the way OpenAI does: prompt_tokens is the
	// INCLUSIVE total, with the cached read portion broken out underneath. We
	// hold every cache tier separately from InputTokens, so reads AND writes
	// have to be added back — folding reads alone under-reported prompt_tokens
	// on any cache-writing request. CacheWriteTiers picks the split or the flat
	// total, never both (double-counting guard, ADR-030).
	var cached int64
	if u.CacheReadInputTokens != nil {
		cached = *u.CacheReadInputTokens
	}
	w5, w1 := u.CacheWriteTiers()
	prompt += cached + w5 + w1
	out := map[string]any{
		"prompt_tokens":     prompt,
		"completion_tokens": completion,
		"total_tokens":      prompt + completion,
	}
	if cached != 0 {
		out["prompt_tokens_details"] = map[string]any{"cached_tokens": cached}
	}
	return out
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
		resp.Usage = usageFromOAI(in.Usage)
	}
	return resp, nil
}

// ── streaming ────────────────────────────────────────────────────────────────

// StreamState carries the minimal cross-event state OpenAI streaming needs:
// whether the leading role delta was already emitted.
type StreamState struct {
	// IncludeUsage mirrors the client's stream_options.include_usage. Usage is
	// rendered onto the terminal chunk ONLY when the client asked for it — an
	// OpenAI client that did not request it does not expect a usage object.
	IncludeUsage bool

	roleSent bool
	id       string
	model    string
	// usage accumulates across frames: the canonical stream keeps Anthropic's
	// vocabulary, so input/cache counts arrive on message_start while
	// message_delta commonly carries output alone (ADR-030 — fold, never
	// overwrite). Rendering message_delta's own usage would report a zero
	// prompt_tokens.
	usage *schema.Usage
}

// ChunkFromCanonical renders one canonical (Anthropic) streaming event to an
// OpenAI chat.completion.chunk. Events with no OpenAI equivalent (ping,
// content_block_start, content_block_stop, message_stop) return nil — the
// caller appends the terminal [DONE] line itself.
func ChunkFromCanonical(c *schema.ChatChunk, st *StreamState) []byte {
	// message_start merges its usage in its own case below, from exactly ONE
	// source. Note this is single-source hygiene, not double-count
	// prevention: MergeUsage folds latest-non-nil per field (never sums), so
	// merging both message.usage and a top-level c.Usage would be idempotent
	// today — the structure exists so a future additive merge cannot
	// silently turn "both set" into a double count. Do not remove the guard
	// on that reasoning.
	if c.Usage != nil && c.Type != "message_start" {
		st.usage = schema.MergeUsage(st.usage, c.Usage)
	}
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
		// Exactly one source: the nested message.usage (the wire's shape)
		// wins; a bare top-level usage on a start frame is the fallback.
		switch {
		case c.Message != nil && c.Message.Usage != nil:
			st.usage = schema.MergeUsage(st.usage, c.Message.Usage)
		case c.Usage != nil:
			st.usage = schema.MergeUsage(st.usage, c.Usage)
		}
		// Emit the opening role delta once.
		if !st.roleSent {
			st.roleSent = true
			return chunkJSON(st, map[string]any{"role": "assistant"}, nil, nil)
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
			return chunkJSON(st, delta, nil, nil)
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
			return chunkJSON(st, delta, nil, nil)
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
			return chunkJSON(st, delta, nil, nil)
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
		var usage map[string]any
		if st.IncludeUsage {
			usage = usageFromCanonical(st.usage)
		}
		return chunkJSON(st, map[string]any{}, &finish, usage)

	default:
		// message_stop, ping, error, content_block_stop: no OpenAI chunk.
		return nil
	}
}

// chunkJSON builds a single OpenAI chat.completion.chunk with one choice.
func chunkJSON(st *StreamState, delta map[string]any, finishReason *string, usage map[string]any) []byte {
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
	if usage != nil {
		out["usage"] = usage
	}
	raw, _ := json.Marshal(out)
	return raw
}

// StreamWantsUsage reports whether an OpenAI chat-completions request body asked
// for token usage on the stream (stream_options.include_usage). Usage frames are
// opt-in on this wire, so a client that never asked must not be sent one.
func StreamWantsUsage(clientBody []byte) bool {
	var in struct {
		StreamOptions *struct {
			IncludeUsage *bool `json:"include_usage"`
		} `json:"stream_options"`
	}
	if json.Unmarshal(clientBody, &in) != nil || in.StreamOptions == nil {
		return false
	}
	return in.StreamOptions.IncludeUsage != nil && *in.StreamOptions.IncludeUsage
}

// ChunkToCanonical parses one vLLM/OpenAI stream chunk into a canonical
// ChatChunk for governance observation. delta.content → a content_block_delta
// text_delta; delta.tool_calls → a content_block_start (the opener carrying
// id/name) or a content_block_delta input_json_delta (an argument fragment);
// finish_reason → a message_delta carrying stop_reason; usage (if present, e.g.
// with stream_options.include_usage) is attached.
func ChunkToCanonical(openaiChunk []byte) ([]*schema.ChatChunk, error) {
	var in struct {
		Choices []struct {
			Index int `json:"index"`
			Delta struct {
				Content   *string       `json:"content"`
				ToolCalls []oaiToolCall `json:"tool_calls"`
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
		usage = usageFromOAI(in.Usage)
	}

	if len(in.Choices) > 0 {
		// One OpenAI chunk can carry content, tool_calls, AND finish_reason
		// together (Ollama-style servers batch the whole tool call into the
		// final chunk) — every part must convert, in stream order: text,
		// then tool frames, then the stop. An early return on any one of
		// them silently drops the others.
		ch := in.Choices[0]
		var out []*schema.ChatChunk
		if ch.Delta.Content != nil && *ch.Delta.Content != "" {
			// content:"" often rides a tool_calls chunk; an empty text_delta
			// for it is noise, so only non-empty text converts.
			idx := ch.Index
			delta := mustJSON(map[string]any{"type": "text_delta", "text": *ch.Delta.Content})
			out = append(out, &schema.ChatChunk{Type: "content_block_delta", Index: &idx, Delta: delta})
		}
		// EVERY tool_calls entry is converted, not just the first: parallel
		// tool calls arrive as several entries in one chunk, each needing
		// its own canonical frame at its own block index. Tool indices shift
		// +1: OpenAI numbers tool calls independently of text, and the
		// choice's text stream owns canonical block index ch.Index — reusing
		// tc.Index verbatim opened a tool_use on top of the open text block.
		// Known theoretical gap: the +1 shift assumes ch.Index == 0 (every
		// real client single-choice stream); a non-zero ch.Index with
		// tc.Index = ch.Index-1 would collide. n>1 choices aren't served
		// through this path today — revisit if that ever changes.
		for _, tc := range ch.Delta.ToolCalls {
			idx := ch.Index + 1
			if tc.Index != nil {
				idx = *tc.Index + 1
			}
			if tc.ID != "" || tc.Function.Name != "" {
				// Opener. Arguments — complete OR a partial fragment — are
				// NEVER embedded in the block (a partial fragment failed
				// json.Valid and was replaced by {}, permanently corrupting
				// the tool input): the block opens with {} and any arguments
				// bytes re-emit as an input_json_delta, the same shape the
				// real Anthropic wire uses.
				block := &schema.ContentBlock{Type: "tool_use", ID: tc.ID, Name: tc.Function.Name, Input: json.RawMessage(`{}`)}
				out = append(out, &schema.ChatChunk{Type: "content_block_start", Index: &idx, ContentBlock: block})
			}
			if tc.Function.Arguments != "" {
				argIdx := idx
				delta := mustJSON(map[string]any{"type": "input_json_delta", "partial_json": tc.Function.Arguments})
				out = append(out, &schema.ChatChunk{Type: "content_block_delta", Index: &argIdx, Delta: delta})
			}
		}
		if ch.FinishReason != nil {
			delta := mustJSON(map[string]any{"stop_reason": mapFinishToStop(*ch.FinishReason)})
			out = append(out, &schema.ChatChunk{Type: "message_delta", Delta: delta})
		}
		if len(out) > 0 {
			// Usage rides the FIRST frame only — consumers fold usage per
			// frame, so repeating it across the fan-out double-counts.
			out[0].Usage = usage
			return out, nil
		}
	}

	if usage != nil {
		// A usage-only chunk (include_usage) with no choice deltas.
		return []*schema.ChatChunk{{Type: "message_delta", Delta: json.RawMessage(`{}`), Usage: usage}}, nil
	}
	return nil, nil
}

// usageFromOAI maps an OpenAI usage object to canonical usage, separating the
// cached prompt tokens out of prompt_tokens.
//
// OpenAI's prompt_tokens is INCLUSIVE of cached_tokens, while canonical
// InputTokens and CacheReadInputTokens are disjoint (that is what the pricing
// table's separate rates assume). Subtracting is therefore required, not
// cosmetic: without it every cached token was billed at the full input rate
// (ADR-030). Clamped at zero in case an upstream reports a larger cached count
// than prompt total.
func usageFromOAI(u *oaiUsage) *schema.Usage {
	if u == nil {
		return nil
	}
	out := &schema.Usage{OutputTokens: u.CompletionTokens}
	var cached int64
	if u.PromptTokensDetails != nil && u.PromptTokensDetails.CachedTokens != nil {
		cached = *u.PromptTokensDetails.CachedTokens
	}
	if u.PromptTokens != nil {
		in := *u.PromptTokens - cached
		if in < 0 {
			in = 0
		}
		out.InputTokens = &in
	}
	if cached != 0 {
		out.CacheReadInputTokens = &cached
	}
	return out
}

// toolsFromOAI maps OpenAI function tools ({type:"function", function:{name,
// description, parameters}}) to the canonical Anthropic shape ({name,
// description, input_schema}) — the inverse of toolsToOAI. Nil on empty or
// unparsable input, so the field is omitted rather than forwarded malformed.
func toolsFromOAI(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var tools []struct {
		Type     string `json:"type"`
		Function struct {
			Name        string          `json:"name"`
			Description string          `json:"description,omitempty"`
			Parameters  json.RawMessage `json:"parameters,omitempty"`
		} `json:"function"`
	}
	if json.Unmarshal(raw, &tools) != nil || len(tools) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		if t.Function.Name == "" {
			continue
		}
		m := map[string]any{"name": t.Function.Name}
		if t.Function.Description != "" {
			m["description"] = t.Function.Description
		}
		if len(t.Function.Parameters) > 0 {
			m["input_schema"] = json.RawMessage(t.Function.Parameters)
		}
		out = append(out, m)
	}
	if len(out) == 0 {
		return nil
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil
	}
	return b
}

// toolChoiceFromOAI maps an OpenAI tool_choice to the canonical Anthropic
// shape — the inverse of toolChoiceToOAI: "auto" → {type:auto},
// "required" → {type:any}, "none" → {type:none},
// {type:"function", function:{name:N}} → {type:"tool", name:N}.
func toolChoiceFromOAI(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return nil
		}
		switch s {
		case "auto":
			return json.RawMessage(`{"type":"auto"}`)
		case "required":
			return json.RawMessage(`{"type":"any"}`)
		case "none":
			return json.RawMessage(`{"type":"none"}`)
		}
		return nil
	}
	var tc struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if json.Unmarshal(raw, &tc) != nil || tc.Type != "function" || tc.Function.Name == "" {
		return nil
	}
	b, err := json.Marshal(map[string]any{"type": "tool", "name": tc.Function.Name})
	if err != nil {
		return nil
	}
	return b
}

// EnsureIncludeUsage sets stream_options.include_usage=true on a top-level
// OpenAI request map, MERGING into any stream_options object already present
// rather than replacing the key wholesale. CanonicalToRequest emits no
// stream_options today, so replacement happened to be lossless — but a future
// field riding that object would be silently clobbered by a wholesale write
// (review finding, PR #65). A non-object stream_options value is replaced:
// it was invalid on this wire to begin with.
func EnsureIncludeUsage(top map[string]json.RawMessage) {
	so := map[string]json.RawMessage{}
	if prev, has := top["stream_options"]; has {
		_ = json.Unmarshal(prev, &so)
	}
	so["include_usage"] = json.RawMessage("true")
	if raw, err := json.Marshal(so); err == nil {
		top["stream_options"] = raw
	}
}

// IsUsageOnlyFrame matches the canonical form ChunkToCanonical gives a
// usage-only OpenAI chunk (choices:[] + usage — what include_usage appends at
// stream end): a message_delta whose delta is empty and whose usage is set. A
// finish frame carries stop_reason in its delta and never matches. Providers
// that inject include_usage use this to strip the frame's Raw from client
// tees the client never asked to receive.
func IsUsageOnlyFrame(c *schema.ChatChunk) bool {
	return c != nil && c.Type == "message_delta" && c.Usage != nil && string(c.Delta) == "{}"
}
