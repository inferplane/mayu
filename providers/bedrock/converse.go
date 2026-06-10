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
		MaxTokens   *int64           `json:"max_tokens"`
		System      json.RawMessage  `json:"system"`
		Messages    []schema.Message `json:"messages"`
		ModelFields map[string]any   `json:"model_fields"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return ConverseRequest{}, err
	}
	cr := ConverseRequest{Inference: map[string]any{}, ModelFields: body.ModelFields}
	if body.MaxTokens != nil {
		cr.Inference["maxTokens"] = *body.MaxTokens
	}
	cr.System = systemText(body.System)
	for _, m := range body.Messages {
		cr.Messages = append(cr.Messages, ConverseMessage{Role: m.Role, Text: messageText(m)})
	}
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

func messageText(m schema.Message) string {
	var parts []string
	for _, b := range m.Content {
		if b.Type == "text" && b.Text != nil {
			parts = append(parts, *b.Text)
		}
	}
	return strings.Join(parts, "")
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
	txt := cresp.Text
	stop := cresp.StopReason
	in, out := cresp.InputTokens, cresp.OutputTokens
	resp := &schema.ChatResponse{
		Type: "message", Role: "assistant", Model: req.Model,
		Content:    []schema.ContentBlock{{Type: "text", Text: &txt}},
		StopReason: &stop,
		Usage:      &schema.Usage{InputTokens: &in, OutputTokens: &out},
	}
	rawBody, _ := json.Marshal(resp)
	return &providers.ProxyResponse{StatusCode: 200, RawBody: rawBody, Parsed: resp}, nil
}

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
		idx := 0
		emit := func(c *schema.ChatChunk) bool {
			var b strings.Builder
			schema.WriteAnthropicSSE(&b, c)
			return yield(&providers.StreamEvent{Raw: []byte(b.String()), Chunk: c}, nil)
		}
		empty := ""
		if !emit(&schema.ChatChunk{Type: "message_start", Message: &schema.ChatResponse{Type: "message", Role: "assistant", Model: req.Model}}) {
			return
		}
		if !emit(&schema.ChatChunk{Type: "content_block_start", Index: &idx, ContentBlock: &schema.ContentBlock{Type: "text", Text: &empty}}) {
			return
		}
		for e, eerr := range evs {
			if eerr != nil {
				yield(nil, eerr)
				return
			}
			if e.Done {
				if !emit(&schema.ChatChunk{Type: "content_block_stop", Index: &idx}) {
					return
				}
				in, out := e.InputTokens, e.OutputTokens
				stop := e.StopReason
				delta, _ := json.Marshal(map[string]any{"stop_reason": stop, "stop_sequence": nil})
				if !emit(&schema.ChatChunk{Type: "message_delta", Delta: delta, Usage: &schema.Usage{InputTokens: &in, OutputTokens: &out}}) {
					return
				}
				emit(&schema.ChatChunk{Type: "message_stop"})
				return
			}
			td := e.TextDelta
			delta, _ := json.Marshal(map[string]any{"type": "text_delta", "text": td})
			if !emit(&schema.ChatChunk{Type: "content_block_delta", Index: &idx, Delta: delta}) {
				return
			}
		}
	}, nil
}
