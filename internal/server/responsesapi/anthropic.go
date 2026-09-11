package responsesapi

import (
	"encoding/json"

	"github.com/inferplane/inferplane/pkg/schema"
)

// anthropicRequest serializes the already validated portable canonical view.
// Responses bookkeeping never reaches /v1/messages. Tool sequence/order carries
// the conversation; the foreign API has no Responses message-phase field.
func anthropicRequest(request *schema.ChatRequest) ([]byte, error) {
	copy := *request
	if copy.MaxTokens == nil {
		n := int64(4096) // Anthropic requires an explicit output bound.
		copy.MaxTokens = &n
	}
	raw, err := json.Marshal(copy)
	if err != nil {
		return nil, err
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, err
	}
	var messages []map[string]json.RawMessage
	if err := json.Unmarshal(body["messages"], &messages); err != nil {
		return nil, err
	}
	for _, message := range messages {
		delete(message, "phase")
		if content, err := withoutProtocolPhase(message["content"]); err != nil {
			return nil, err
		} else {
			message["content"] = content
		}
	}
	body["messages"], err = json.Marshal(messages)
	if err != nil {
		return nil, err
	}
	if toolsRaw := body["tools"]; len(toolsRaw) > 0 {
		var tools []map[string]json.RawMessage
		if err := json.Unmarshal(toolsRaw, &tools); err != nil {
			return nil, err
		}
		for _, tool := range tools {
			delete(tool, "strict") // strict:true was rejected before admission.
		}
		body["tools"], err = json.Marshal(tools)
		if err != nil {
			return nil, err
		}
	}
	return json.Marshal(body)
}

func withoutProtocolPhase(raw json.RawMessage) (json.RawMessage, error) {
	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return raw, nil // string tool results are application data
	}
	for _, block := range blocks {
		delete(block, "phase")
		if string(block["type"]) == `"tool_result"` {
			content, err := withoutProtocolPhase(block["content"])
			if err != nil {
				return nil, err
			}
			if len(content) != 0 {
				block["content"] = content
			}
		}
	}
	return json.Marshal(blocks)
}
