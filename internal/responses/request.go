package responses

import (
	"bytes"
	"encoding/json"

	"github.com/inferplane/inferplane/pkg/schema"
)

const customAnnotation = "x-inferplane-responses-custom"

// RequestToCanonical builds an observation view. Unknown native Responses
// surfaces remain in Extra/opaque blocks; RawBody remains the forwarding source.
func RequestToCanonical(raw []byte) (*schema.ChatRequest, error) {
	m, err := object(raw)
	if err != nil {
		return nil, err
	}
	if err := knownCase(m, "model", "input", "instructions", "tools", "tool_choice", "stream", "max_output_tokens"); err != nil {
		return nil, err
	}
	model, err := text(m["model"])
	if err != nil || model == "" {
		return nil, ErrInvalid
	}
	cr := &schema.ChatRequest{Model: model, Messages: []schema.Message{}, Extra: map[string]json.RawMessage{}}
	if present(m["max_output_tokens"]) {
		var n int64
		if json.Unmarshal(m["max_output_tokens"], &n) != nil || n <= 0 {
			return nil, ErrInvalid
		}
		cr.MaxTokens = &n
	}
	if present(m["stream"]) {
		var stream bool
		if json.Unmarshal(m["stream"], &stream) != nil {
			return nil, ErrInvalid
		}
		cr.Stream = &stream
	}
	var system []schema.ContentBlock
	if present(m["instructions"]) {
		s, err := text(m["instructions"])
		if err != nil {
			return nil, err
		}
		system = append(system, schema.ContentBlock{Type: "text", Text: &s})
	}
	input := bytes.TrimSpace(m["input"])
	if present(input) {
		if input[0] == '"' {
			s, err := text(input)
			if err != nil {
				return nil, err
			}
			cr.Messages = append(cr.Messages, schema.Message{Role: "user", Content: []schema.ContentBlock{{Type: "text", Text: &s}}})
		} else {
			var items []json.RawMessage
			if json.Unmarshal(input, &items) != nil || input[0] != '[' {
				return nil, ErrInvalid
			}
			// Responses emits each parallel call as a separate item. Canonical
			// assistant messages represent whole call batches: the Chat bridge
			// must not insert another assistant turn before their tool results.
			toolTurn := -1
			for _, rawItem := range items {
				item, err := object(rawItem)
				if err != nil {
					return nil, err
				}
				kind := optionalText(item["type"])
				switch kind {
				case "", "message":
					role := optionalText(item["role"])
					if role != "user" && role != "assistant" && role != "system" && role != "developer" {
						return nil, ErrInvalid
					}
					blocks, err := contentToCanonical(item["content"])
					if err != nil {
						return nil, err
					}
					if role == "system" || role == "developer" {
						toolTurn = -1
						system = append(system, blocks...)
					} else {
						msg := schema.Message{Role: role, Content: blocks}
						if present(item["phase"]) {
							phase := optionalText(item["phase"])
							if role != "assistant" || phase != "commentary" && phase != "final_answer" {
								return nil, ErrInvalid
							}
							msg.Extra = map[string]json.RawMessage{"phase": item["phase"]}
						}
						if role == "assistant" && toolTurn >= 0 {
							// Preserve the phase at its original text block even
							// when multiple phase-bearing items share a call turn.
							for i := range msg.Content {
								if phase := msg.Extra["phase"]; present(phase) {
									if msg.Content[i].Extra == nil {
										msg.Content[i].Extra = map[string]json.RawMessage{}
									}
									msg.Content[i].Extra["phase"] = phase
								}
							}
							cr.Messages[toolTurn].Content = append(cr.Messages[toolTurn].Content, msg.Content...)
						} else {
							toolTurn = -1
							cr.Messages = append(cr.Messages, msg)
						}
					}
				case "function_call", "custom_tool_call":
					id := optionalText(item["call_id"])
					name, err := callName(item)
					if id == "" || err != nil {
						return nil, ErrInvalid
					}
					var input json.RawMessage
					if kind == "custom_tool_call" {
						s, err := text(item["input"])
						if err != nil {
							return nil, err
						}
						input = marshal(map[string]string{"input": s})
					} else {
						args, err := text(item["arguments"])
						if err != nil {
							return nil, err
						}
						if _, err := object([]byte(args)); err != nil {
							return nil, ErrInvalid
						}
						input = json.RawMessage(args)
					}
					if toolTurn < 0 {
						toolTurn = len(cr.Messages)
						cr.Messages = append(cr.Messages, schema.Message{Role: "assistant"})
					}
					cr.Messages[toolTurn].Content = append(cr.Messages[toolTurn].Content,
						schema.ContentBlock{Type: "tool_use", ID: id, Name: name, Input: input})
				case "function_call_output", "custom_tool_call_output":
					toolTurn = -1
					id := optionalText(item["call_id"])
					if id == "" || !present(item["output"]) {
						return nil, ErrInvalid
					}
					blocks, err := contentToCanonical(item["output"])
					if err != nil {
						return nil, err
					}
					result := item["output"]
					if bytes.TrimSpace(result)[0] != '"' {
						result = marshal(blocks)
					}
					cr.Messages = append(cr.Messages, schema.Message{Role: "user", Content: []schema.ContentBlock{{Type: "tool_result", ToolUseID: id, Content: result}}})
				default:
					toolTurn = -1
					// Never treat an opaque native item as empty ordinary text.
					cr.Messages = append(cr.Messages, schema.Message{Role: "assistant", Content: []schema.ContentBlock{{Type: "responses_opaque", Extra: map[string]json.RawMessage{"item": rawItem}}}})
				}
			}
		}
	}
	if len(system) > 0 {
		cr.System = marshal(system)
	}
	if present(m["tools"]) {
		cr.Tools, err = canonicalTools(m["tools"])
		if err != nil {
			return nil, err
		}
	}
	if present(m["tool_choice"]) {
		choice := optionalText(m["tool_choice"])
		switch choice {
		case "auto", "none":
			cr.ToolChoice = marshal(map[string]string{"type": choice})
		case "required":
			cr.ToolChoice = marshal(map[string]string{"type": "any"})
		default:
			tc, err := object(m["tool_choice"])
			if err != nil {
				return nil, err
			}
			if optionalText(tc["type"]) == "function" || optionalText(tc["type"]) == "custom" {
				name, err := callName(tc)
				if err != nil {
					return nil, err
				}
				cr.ToolChoice = marshal(map[string]string{"type": "tool", "name": name})
			} else {
				cr.ToolChoice = m["tool_choice"]
			}
		}
	}
	// Only portable sampling parameters enter canonical Extra. Native-only
	// options remain in RawBody; copying store/conversation into Extra would
	// accidentally send Responses fields to an Anthropic provider.
	for _, name := range []string{"temperature", "top_p"} {
		if present(m[name]) {
			cr.Extra[name] = m[name]
		}
	}
	return cr, nil
}

func contentToCanonical(raw json.RawMessage) ([]schema.ContentBlock, error) {
	raw = bytes.TrimSpace(raw)
	if !present(raw) {
		return nil, ErrInvalid
	}
	if raw[0] == '"' {
		s, err := text(raw)
		return []schema.ContentBlock{{Type: "text", Text: &s}}, err
	}
	var parts []json.RawMessage
	if raw[0] != '[' || json.Unmarshal(raw, &parts) != nil {
		return nil, ErrInvalid
	}
	out := make([]schema.ContentBlock, 0, len(parts))
	for _, rawPart := range parts {
		p, err := object(rawPart)
		if err != nil {
			return nil, err
		}
		switch optionalText(p["type"]) {
		case "input_text", "output_text":
			s, err := text(p["text"])
			if err != nil {
				return nil, err
			}
			out = append(out, schema.ContentBlock{Type: "text", Text: &s})
		case "refusal":
			s, err := text(p["refusal"])
			if err != nil {
				return nil, err
			}
			out = append(out, schema.ContentBlock{Type: "text", Text: &s, Extra: map[string]json.RawMessage{"responses_refusal": json.RawMessage("true")}})
		default:
			out = append(out, schema.ContentBlock{Type: "responses_opaque", Extra: map[string]json.RawMessage{"part": rawPart}})
		}
	}
	return out, nil
}

// ValidateConversion rejects features whose semantics cannot be carried by the
// supported stateless text/function/custom-tool bridge. Native forwarding does
// not call this function.
func ValidateConversion(raw []byte) error {
	cr, err := RequestToCanonical(raw)
	if err != nil {
		return err
	}
	m, _ := object(raw)
	allowed := map[string]bool{}
	for _, k := range []string{"model", "input", "instructions", "tools", "tool_choice", "stream", "max_output_tokens", "temperature", "top_p", "store", "background", "previous_response_id", "conversation", "text", "reasoning", "include", "metadata", "client_metadata", "user", "safety_identifier", "prompt_cache_key", "prompt_cache_retention", "parallel_tool_calls", "service_tier", "truncation"} {
		allowed[k] = true
	}
	for k, value := range m {
		if !allowed[k] && present(value) {
			return ErrUnsupported
		}
	}
	for _, k := range []string{"previous_response_id", "conversation"} {
		if present(m[k]) {
			return ErrUnsupported
		}
	}
	for _, k := range []string{"background", "store"} {
		if present(m[k]) && string(m[k]) != "false" {
			return ErrUnsupported
		}
	}
	if present(m["parallel_tool_calls"]) && string(m["parallel_tool_calls"]) != "true" {
		return ErrUnsupported
	}
	if present(m["client_metadata"]) {
		if _, err := object(m["client_metadata"]); err != nil {
			return ErrInvalid
		}
	}
	if present(m["reasoning"]) {
		reasoning, err := object(m["reasoning"])
		if err != nil {
			return ErrInvalid
		}
		for k, v := range reasoning {
			// Codex's summary-only preference does not submit reasoning
			// state or require an effort level. We do not invent a summary.
			if k != "summary" || optionalText(v) != "auto" {
				return ErrUnsupported
			}
		}
	}
	if present(m["include"]) {
		var include []string
		if json.Unmarshal(m["include"], &include) != nil {
			return ErrUnsupported
		}
		for _, field := range include {
			// An optional output inclusion is not encrypted input. The
			// stateless bridge omits it and never fabricates opaque state.
			if field != "reasoning.encrypted_content" {
				return ErrUnsupported
			}
		}
	}
	if present(m["truncation"]) && optionalText(m["truncation"]) != "disabled" {
		return ErrUnsupported
	}
	if present(m["text"]) {
		t, err := object(m["text"])
		if err != nil {
			return ErrInvalid
		}
		for k := range t {
			if k != "format" {
				return ErrUnsupported
			}
		}
		if present(t["format"]) {
			f, err := object(t["format"])
			if err != nil || optionalText(f["type"]) != "text" || len(f) != 1 {
				return ErrUnsupported
			}
		}
	}
	entries, err := flattenTools(m["tools"], true)
	if err != nil {
		return err
	}
	// Native tool execution metadata and references are not ordinary local
	// client tool calls. Namespaces use explicit mappings; async/caller
	// execution semantics remain unsupported.
	var items []map[string]json.RawMessage
	if input := bytes.TrimSpace(m["input"]); len(input) > 0 && input[0] == '[' {
		_ = json.Unmarshal(input, &items)
		for _, item := range items {
			for _, k := range []string{"async", "caller", "prompt_cache_breakpoint"} {
				if present(item[k]) {
					return ErrUnsupported
				}
			}
			if err := validateNamespacedCall(item, entries); err != nil {
				return err
			}
		}
	}
	if present(m["tool_choice"]) && optionalText(m["tool_choice"]) == "" {
		tc, err := object(m["tool_choice"])
		if err != nil || optionalText(tc["name"]) == "" || optionalText(tc["type"]) != "function" && optionalText(tc["type"]) != "custom" {
			return ErrUnsupported
		}
		if err := validateNamespacedCall(tc, entries); err != nil {
			return err
		}
	}
	var system []schema.ContentBlock
	_ = json.Unmarshal(cr.System, &system)
	for _, b := range system {
		if b.Type != "text" {
			return ErrUnsupported
		}
	}
	for _, msg := range cr.Messages {
		for _, b := range msg.Content {
			if b.Type == "responses_opaque" {
				return ErrUnsupported
			}
			if b.Type == "tool_result" && bytes.HasPrefix(bytes.TrimSpace(b.Content), []byte("[")) {
				var parts []schema.ContentBlock
				if json.Unmarshal(b.Content, &parts) != nil {
					return ErrInvalid
				}
				for _, p := range parts {
					if p.Type != "text" {
						return ErrUnsupported
					}
				}
			}
		}
	}
	return nil
}

func customTools(req *schema.ChatRequest) map[string]bool {
	out := map[string]bool{}
	if req == nil {
		return out
	}
	var tools []struct {
		Name   string                     `json:"name"`
		Schema map[string]json.RawMessage `json:"input_schema"`
	}
	_ = json.Unmarshal(req.Tools, &tools)
	for _, t := range tools {
		if string(t.Schema[customAnnotation]) == "true" {
			out[t.Name] = true
		}
	}
	return out
}
