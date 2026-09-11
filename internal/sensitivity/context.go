package sensitivity

import (
	"context"
	"strings"
)

// MatchesContextKeywords classifies the most recent actual user instruction.
// System prompts, tool schemas, previous instructions and tool results remain
// part of privacy inspection/context-size accounting, but cannot permanently
// force every later task into the strong class.
func MatchesContextKeywords(raw []byte, protocol string, keywords []string) bool {
	w := walker{ctx: context.Background(), visit: func(string) error { return nil }}
	root, err := w.decode(raw, 0)
	if err != nil {
		return false
	}
	body, ok := root.(map[string]any)
	if !ok {
		return false
	}
	var latest string
	messages, _ := body["messages"].([]any)
	if protocol == "responses" {
		if text, ok := body["input"].(string); ok {
			latest = text
		} else {
			messages, _ = body["input"].([]any)
		}
	}
	for i := len(messages) - 1; i >= 0; i-- {
		message, ok := messages[i].(map[string]any)
		if !ok || message["role"] != "user" {
			continue
		}
		latest = instructionText(message["content"])
		if latest != "" {
			break
		}
	}
	text := fold(latest)
	for _, keyword := range keywords {
		if keyword != "" && strings.Contains(text, fold(keyword)) {
			return true
		}
	}
	return false
}

func instructionText(content any) string {
	if text, ok := content.(string); ok {
		return text
	}
	blocks, _ := content.([]any)
	var out strings.Builder
	for _, value := range blocks {
		block, ok := value.(map[string]any)
		if !ok || block["type"] != "text" && block["type"] != "input_text" {
			continue
		}
		if text, ok := block["text"].(string); ok {
			out.WriteString(text)
			out.WriteByte('\n')
		}
	}
	return out.String()
}
