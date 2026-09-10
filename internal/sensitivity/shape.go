package sensitivity

import (
	"encoding/json"
	"slices"
	"strings"
)

// Shape recognition is deliberately limited to known ingress envelopes and
// actual content positions. Every string elsewhere is still scanned by walker.
func (r *Result) inspectShape(protocol string, root any) error {
	body, ok := root.(map[string]any)
	if !ok {
		r.Complete = false
		return nil
	}
	r.options(protocol, body)
	if system, exists := body["system"]; exists {
		r.content(protocol, system)
	}
	messages, ok := body["messages"].([]any)
	if !ok || len(messages) == 0 {
		r.Complete = false
		return nil
	}
	for _, value := range messages {
		message, ok := value.(map[string]any)
		if !ok {
			r.Complete = false
			continue
		}
		r.options(protocol, message)
		if err := r.messageTools(protocol, message); err != nil {
			return err
		}
		role, _ := message["role"].(string)
		switch role {
		case "user":
			r.UserTurns++
		case "assistant":
			r.HasHistory = true
		case "tool", "function":
			r.HasHistory, r.HasTools = true, true
			if protocol != "openai" {
				r.Complete = false
			}
		case "system", "developer":
			if protocol != "openai" {
				r.Complete = false
			}
		default:
			r.Complete = false
		}
		content, exists := message["content"]
		if role == "assistant" && content == nil && (present(message["tool_calls"]) || present(message["function_call"])) {
			continue // OpenAI permits omitted/null content for tool calls.
		}
		if !exists {
			r.Complete = false
		} else {
			r.content(protocol, content)
		}
	}
	if r.UserTurns > 1 {
		r.HasHistory = true
	}
	return nil
}

func (r *Result) messageTools(protocol string, message map[string]any) error {
	if value, exists := message["tool_calls"]; exists {
		calls, ok := value.([]any)
		if !ok || protocol != "openai" {
			r.Complete = false
		}
		for _, value := range calls {
			call, ok := value.(map[string]any)
			if !ok || call["type"] != "function" {
				r.Complete = false
				continue
			}
			r.onlyFields(call, "id", "type", "function")
			r.optionalText(call, "id")
			if err := r.functionCall(call["function"]); err != nil {
				return err
			}
		}
	}
	if value, exists := message["function_call"]; exists {
		if protocol != "openai" {
			r.Complete = false
		}
		return r.functionCall(value)
	}
	return nil
}

func (r *Result) functionCall(value any) error {
	function, ok := value.(map[string]any)
	if !ok {
		r.Complete = false
		return nil
	}
	r.onlyFields(function, "name", "arguments")
	if _, ok := function["name"].(string); !ok {
		r.Complete = false
	}
	arguments, ok := function["arguments"].(string)
	if !ok {
		r.Complete = false
		return nil
	}
	if !json.Valid([]byte(arguments)) {
		return errMalformed
	}
	if !strings.HasPrefix(strings.TrimSpace(arguments), "{") {
		r.Complete = false
	}
	return nil
}

func present(value any) bool {
	switch value := value.(type) {
	case nil:
		return false
	case []any:
		return len(value) != 0
	case string:
		return value != "" && value != "none"
	default:
		return true
	}
}

func (r *Result) options(protocol string, object map[string]any) {
	for _, key := range []string{"tools", "functions", "tool_calls", "function_call", "tool_choice", "toolConfig"} {
		if present(object[key]) {
			r.HasTools = true
		}
	}
	for _, key := range []string{"reasoning", "reasoning_content", "reasoning_effort"} {
		if present(object[key]) {
			r.HasReasoning = true
		}
	}
	if reasoning, exists := object["reasoning"]; exists {
		if protocol != "openai" {
			r.Complete = false
		}
		switch value := reasoning.(type) {
		case string: // Inspectable reasoning text was scanned by walker.
		case map[string]any:
			r.onlyFields(value, "effort", "summary")
			r.optionalText(value, "effort", "summary")
		default:
			r.Complete = false
		}
	}
	if present(object["reasoning_details"]) {
		// This extension can carry encrypted reasoning; its variants are not
		// part of the supported ingress contract.
		r.HasReasoning = true
		r.Complete = false
	}
	if thinking, exists := object["thinking"]; exists {
		cfg, ok := thinking.(map[string]any)
		if !ok {
			r.Complete = false
			r.HasReasoning = true
		} else {
			r.onlyFields(cfg, "type", "budget_tokens")
			if cfg["type"] != "disabled" {
				r.HasReasoning = true
			}
		}
	}
	for _, key := range []string{"response_format", "output_format", "structured_output"} {
		if present(object[key]) {
			r.HasStructuredOutput = true
		}
	}
	if cfg, ok := object["output_config"].(map[string]any); ok && present(cfg["format"]) {
		r.HasStructuredOutput = true
	}
	for _, key := range []string{"audio", "image", "document", "file", "encrypted_content", "redacted_thinking"} {
		if _, exists := object[key]; exists {
			r.Complete = false
			if key == "image" {
				r.HasVision = true
			}
			if key == "encrypted_content" || key == "redacted_thinking" {
				r.HasReasoning = true
			}
		}
	}
}

func (r *Result) content(protocol string, content any) {
	switch content := content.(type) {
	case string:
		// Already scanned, including any JSON encoded inside the string.
	case []any:
		for _, value := range content {
			block, ok := value.(map[string]any)
			if !ok {
				r.Complete = false
				continue
			}
			r.block(protocol, block)
		}
	default:
		r.Complete = false
	}
}

func (r *Result) requireText(block map[string]any, key string) {
	if _, ok := block[key].(string); !ok {
		r.Complete = false
	}
}

// Use these only at protocol-owned positions. Application payloads such as tool
// inputs, decoded function arguments and tool-result JSON keep arbitrary fields.
func (r *Result) onlyFields(object map[string]any, allowed ...string) {
	for key := range object {
		if !slices.Contains(allowed, key) {
			r.Complete = false
		}
	}
}

func (r *Result) optionalText(object map[string]any, keys ...string) {
	for _, key := range keys {
		if _, exists := object[key]; exists {
			r.requireText(object, key)
		}
	}
}

func (r *Result) block(protocol string, block map[string]any) {
	kind, hasType := block["type"].(string)
	if !hasType && protocol == "bedrock" {
		r.bedrockBlock(block)
		return
	}
	var fields []string
	switch kind {
	case "text":
		r.requireText(block, "text")
		fields = []string{"text", "citations"}
	case "refusal":
		r.requireText(block, "refusal")
		fields = []string{"refusal"}
		if protocol != "openai" {
			r.Complete = false
		}
	case "tool_use", "server_tool_use":
		r.HasTools = true
		fields = []string{"id", "name", "input"}
		if protocol == "openai" {
			r.Complete = false
		}
		if _, ok := block["input"].(map[string]any); !ok {
			r.Complete = false
		}
	case "tool_result":
		r.HasTools = true
		fields = []string{"tool_use_id", "content", "is_error"}
		if protocol == "openai" {
			r.Complete = false
		}
		r.content(protocol, block["content"])
	case "thinking":
		r.HasReasoning = true
		r.requireText(block, "thinking")
		fields = []string{"thinking", "signature"}
		if protocol == "openai" {
			r.Complete = false
		}
	case "reasoning":
		r.HasReasoning = true
		r.requireText(block, "text")
		fields = []string{"text"}
		if protocol != "openai" {
			r.Complete = false
		}
		if _, exists := block["encrypted_content"]; exists {
			r.Complete = false
		}
	case "redacted_thinking", "encrypted_thinking":
		r.HasReasoning = true
		r.Complete = false
	case "image", "image_url":
		r.HasVision = true
		r.Complete = false
	case "audio", "input_audio", "document", "file", "video":
		r.Complete = false
	default:
		r.Complete = false
	}
	if fields != nil {
		for key := range block {
			if key != "type" && key != "cache_control" && !slices.Contains(fields, key) {
				r.Complete = false
			}
		}
	}
}

func (r *Result) bedrockBlock(block map[string]any) {
	// Bedrock's untagged union has exactly one member. Cache metadata does not
	// add a content alternative, but every unknown union member is incomplete.
	count := 0
	for key, value := range block {
		if key == "cachePoint" {
			continue
		}
		count++
		switch key {
		case "text":
			r.requireText(block, key)
		case "image":
			r.HasVision = true
			r.Complete = false
		case "audio", "video", "document":
			r.Complete = false
		case "toolUse":
			r.HasTools = true
			object, ok := value.(map[string]any)
			if !ok {
				r.Complete = false
			} else {
				r.onlyFields(object, "toolUseId", "name", "input")
				r.optionalText(object, "toolUseId", "name")
				if _, ok := object["input"].(map[string]any); !ok {
					r.Complete = false
				}
			}
		case "toolResult":
			r.HasTools = true
			object, ok := value.(map[string]any)
			if !ok {
				r.Complete = false
				continue
			}
			r.onlyFields(object, "toolUseId", "content", "status")
			r.optionalText(object, "toolUseId", "status")
			content, ok := object["content"].([]any)
			if !ok {
				r.Complete = false
				continue
			}
			for _, value := range content {
				item, ok := value.(map[string]any)
				if !ok {
					r.Complete = false
				} else if _, jsonData := item["json"]; jsonData && len(item) == 1 {
					// Application JSON, not a protocol content block.
				} else {
					r.bedrockBlock(item)
				}
			}
		case "reasoningContent":
			r.HasReasoning = true
			object, ok := value.(map[string]any)
			if !ok {
				r.Complete = false
				continue
			}
			text, ok := object["reasoningText"].(map[string]any)
			if !ok || len(object) != 1 {
				r.Complete = false
			} else {
				r.onlyFields(text, "text", "signature")
				r.requireText(text, "text")
				r.optionalText(text, "signature")
			}
		default:
			r.Complete = false
		}
	}
	if count != 1 {
		r.Complete = false
	}
}
