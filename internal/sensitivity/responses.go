package sensitivity

// Responses inspection runs on the original envelope, not on a conversion to
// Chat Completions. The generic walker has already scanned every scalar/key,
// including encoded tool argument JSON; this file establishes coverage.
func (r *Result) responsesShape(body map[string]any) error {
	r.options("openai", body)
	r.onlyFields(body, "model", "input", "instructions", "tools", "tool_choice",
		"parallel_tool_calls", "stream", "store", "include", "reasoning", "text",
		"max_output_tokens", "metadata", "truncation", "prompt_cache_key",
		"prompt_cache_retention", "previous_response_id", "conversation",
		"background", "service_tier", "safety_identifier", "user", "temperature",
		"top_p", "top_logprobs", "max_tool_calls", "stream_options", "client_metadata")
	if reasoning, ok := body["reasoning"].(map[string]any); ok && !present(reasoning["effort"]) {
		// A summary/output-inclusion preference does not require a reasoning
		// model. Actual reasoning items below still mark opaque history.
		r.HasReasoning = false
	}
	r.optionalText(body, "instructions")
	if present(body["previous_response_id"]) || present(body["conversation"]) {
		r.Complete = false // prior content is held elsewhere, not in these bytes
	}
	if text, ok := body["text"].(map[string]any); ok {
		if format, ok := text["format"].(map[string]any); ok && format["type"] != "text" {
			r.HasStructuredOutput = true
		}
	}
	if tools, exists := body["tools"]; exists {
		list, ok := tools.([]any)
		if !ok {
			r.Complete = false
		}
		r.responsesTools(list, 0)
	}
	switch input := body["input"].(type) {
	case string:
		r.UserTurns++
	case []any:
		if len(input) == 0 {
			r.Complete = false
		}
		for _, value := range input {
			item, ok := value.(map[string]any)
			if !ok {
				r.Complete = false
				continue
			}
			kind, _ := item["type"].(string)
			switch kind {
			case "", "message":
				r.onlyFields(item, "type", "role", "content", "id", "status", "phase")
				switch item["role"] {
				case "user":
					r.UserTurns++
				case "assistant":
					r.HasHistory = true
				case "system", "developer":
				default:
					r.Complete = false
				}
				r.responsesContent(item["content"])
			case "function_call":
				r.HasHistory, r.HasTools = true, true
				r.onlyFields(item, "type", "id", "call_id", "name", "arguments", "status", "namespace")
				r.requireText(item, "call_id")
				if err := r.functionCall(map[string]any{"name": item["name"], "arguments": item["arguments"]}); err != nil {
					return err
				}
			case "custom_tool_call":
				r.HasHistory, r.HasTools = true, true
				r.onlyFields(item, "type", "id", "call_id", "name", "input", "status", "namespace")
				r.requireText(item, "call_id")
				r.requireText(item, "name")
				r.requireText(item, "input")
			case "function_call_output", "custom_tool_call_output":
				r.HasHistory, r.HasTools = true, true
				r.onlyFields(item, "type", "id", "call_id", "output", "status")
				r.requireText(item, "call_id")
				r.responsesContent(item["output"])
			case "reasoning":
				r.HasHistory, r.HasReasoning = true, true
				r.Complete = false // summaries do not establish full reasoning coverage
			default:
				r.Complete = false
			}
		}
	default:
		r.Complete = false
	}
	if r.UserTurns > 1 {
		r.HasHistory = true
	}
	return nil
}

func (r *Result) responsesTools(tools []any, depth int) {
	if depth > 8 {
		r.Complete = false
		return
	}
	for _, value := range tools {
		tool, ok := value.(map[string]any)
		if !ok {
			r.Complete = false
			continue
		}
		r.HasTools = true
		switch tool["type"] {
		case "namespace":
			r.onlyFields(tool, "type", "name", "description", "tools")
			r.requireText(tool, "name")
			r.optionalText(tool, "description")
			nested, ok := tool["tools"].([]any)
			if !ok {
				r.Complete = false
			}
			r.responsesTools(nested, depth+1)
		case "function":
			r.onlyFields(tool, "type", "name", "description", "parameters", "strict", "defer_loading")
			r.requireText(tool, "name")
			r.optionalText(tool, "description")
		case "custom":
			r.onlyFields(tool, "type", "name", "description", "format", "defer_loading")
			r.requireText(tool, "name")
			r.optionalText(tool, "description")
			if format, ok := tool["format"].(map[string]any); ok {
				r.onlyFields(format, "type", "syntax", "definition")
				if format["type"] != "text" && format["type"] != "grammar" {
					r.Complete = false
				}
			}
		default:
			// Hosted tools/MCP may fetch unseen data or create new egress.
			r.HasRemoteTools = true
			r.Complete = false
		}
	}
}

func (r *Result) responsesContent(value any) {
	switch content := value.(type) {
	case string:
	case []any:
		for _, value := range content {
			block, ok := value.(map[string]any)
			if !ok {
				r.Complete = false
				continue
			}
			switch block["type"] {
			case "input_text":
				r.onlyFields(block, "type", "text")
				r.requireText(block, "text")
			case "output_text":
				r.onlyFields(block, "type", "text", "annotations", "logprobs")
				r.requireText(block, "text")
			case "refusal":
				r.onlyFields(block, "type", "refusal")
				r.requireText(block, "refusal")
			case "input_image":
				r.HasVision = true
				r.Complete = false
			default:
				r.Complete = false
			}
		}
	default:
		r.Complete = false
	}
}
