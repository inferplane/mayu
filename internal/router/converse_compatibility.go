package router

import (
	"bytes"
	"encoding/json"
	"regexp"
)

// This is a compatibility ceiling for the existing built-in converter, not a
// converter or a provider capability declaration. Its contract is exercised
// against the real Bedrock conversion/transport in request_routing_contract_test.
var converseToolName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,63}$`)

// conversePreservesRequest checks the request fields whose representations the
// built-in Converse path is known to discard. Run once after successful local
// inspection; retain only the boolean, never decoded text. Unknown tool shapes
// are refused rather than guessing their semantics. No provider declaration can
// override this check. This does not change the converter's legacy behavior.
func conversePreservesRequest(raw []byte) bool {
	var body struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
		Tools               []map[string]json.RawMessage `json:"tools"`
		ToolChoice          map[string]json.RawMessage   `json:"tool_choice"`
		MaxCompletionTokens json.RawMessage              `json:"max_completion_tokens"`
		MaxOutputTokens     json.RawMessage              `json:"max_output_tokens"`
		Stop                json.RawMessage              `json:"stop"`
		InferenceConfig     json.RawMessage              `json:"inferenceConfig"`
	}
	if json.Unmarshal(raw, &body) != nil || len(body.Messages) == 0 {
		return false
	}
	// The transport reads max_tokens/stop_sequences, never these alternatives.
	// Even a valid declared output limit must not disappear after admission.
	if len(body.MaxCompletionTokens) > 0 || len(body.MaxOutputTokens) > 0 ||
		len(body.Stop) > 0 || len(body.InferenceConfig) > 0 {
		return false
	}
	for _, message := range body.Messages {
		// OpenAI system/developer messages are folded into user text, changing
		// instruction authority. Only the Anthropic top-level system survives.
		if message.Role != "user" && message.Role != "assistant" {
			return false
		}
	}
	tools := make(map[string]bool, len(body.Tools))
	for _, tool := range body.Tools {
		if !onlyConverseFields(tool, "name", "description", "input_schema") {
			return false
		}
		var name, description string
		if json.Unmarshal(tool["name"], &name) != nil || !converseToolName.MatchString(name) || tools[name] {
			return false
		}
		if desc, ok := tool["description"]; ok && json.Unmarshal(desc, &description) != nil {
			return false
		}
		// parseTools drops omitted/null schemas. Require the known object
		// schema representation; server-tool shorthands are not ToolSpecs.
		schema := bytes.TrimSpace(tool["input_schema"])
		if len(schema) == 0 || schema[0] != '{' {
			return false
		}
		tools[name] = true
	}
	if len(body.ToolChoice) == 0 {
		return true
	}
	if !onlyConverseFields(body.ToolChoice, "type", "name") {
		return false
	}
	var kind, name string
	if json.Unmarshal(body.ToolChoice["type"], &kind) != nil {
		return false
	}
	if rawName, ok := body.ToolChoice["name"]; ok && json.Unmarshal(rawName, &name) != nil {
		return false
	}
	switch kind {
	case "auto":
		return name == ""
	case "any":
		return name == "" && len(tools) > 0
	case "tool":
		// resolveToolChoice silently removes a forced choice when its tool
		// was dropped or absent, permitting an unrelated model action.
		return tools[name]
	default:
		return false
	}
}

func onlyConverseFields(fields map[string]json.RawMessage, allowed ...string) bool {
	for name := range fields {
		found := false
		for _, known := range allowed {
			found = found || name == known
		}
		if !found {
			return false
		}
	}
	return true
}
