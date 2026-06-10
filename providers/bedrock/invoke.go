package bedrock

import "encoding/json"

const bedrockAnthropicVersion = `"bedrock-2023-05-31"`

// toInvokeBody rewrites an Anthropic Messages request body for Bedrock
// InvokeModel: drop top-level "model" (it goes in the URL) and inject
// "anthropic_version". Parsing only the TOP LEVEL into json.RawMessage keeps
// every system/messages/tools VALUE byte-identical, so the prompt-cache prefix
// is preserved (§4.4). Top-level key order is irrelevant to the cache.
func toInvokeBody(raw []byte) ([]byte, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, err
	}
	delete(top, "model")
	if _, has := top["anthropic_version"]; !has {
		top["anthropic_version"] = json.RawMessage(bedrockAnthropicVersion)
	}
	return json.Marshal(top)
}
