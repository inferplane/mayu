// Package mockprovider is a deterministic in-memory Provider for tests.
// It needs no network and emits a fixed Anthropic-shaped message + stream.
package mockprovider

import (
	"context"
	"encoding/json"
	"iter"

	"github.com/inferplane/inferplane/pkg/schema"
	"github.com/inferplane/inferplane/providers"
)

type mock struct{ model string }

// New returns a mock provider serving exactly one model id.
func New(model string) providers.Provider { return &mock{model: model} }

func (m *mock) Name() string { return "mock" }

func (m *mock) Models() []schema.ModelInfo {
	return []schema.ModelInfo{{Type: "model", ID: m.model, DisplayName: m.model}}
}

func ptrStr(s string) *string { return &s }
func ptrI64(i int64) *int64   { return &i }

func (m *mock) Complete(_ context.Context, _ *providers.ProxyRequest) (*providers.ProxyResponse, error) {
	in, out := int64(10), int64(5)
	resp := &schema.ChatResponse{
		ID: "msg_mock", Type: "message", Role: "assistant", Model: m.model,
		Content:    []schema.ContentBlock{{Type: "text", Text: ptrStr("ok")}},
		StopReason: ptrStr("end_turn"),
		Usage:      &schema.Usage{InputTokens: &in, OutputTokens: &out},
	}
	raw, _ := json.Marshal(resp)
	return &providers.ProxyResponse{StatusCode: 200, RawBody: raw, Parsed: resp}, nil
}

func (m *mock) Stream(_ context.Context, _ *providers.ProxyRequest) (iter.Seq2[*providers.StreamEvent, error], error) {
	in, out := int64(10), int64(5)
	events := []*providers.StreamEvent{
		{Raw: []byte("event: message_start\ndata: {\"type\":\"message_start\"}\n\n"),
			Chunk: &schema.ChatChunk{Type: "message_start"}},
		{Raw: []byte("event: message_delta\ndata: {\"type\":\"message_delta\"}\n\n"),
			Chunk: &schema.ChatChunk{Type: "message_delta", Usage: &schema.Usage{InputTokens: &in, OutputTokens: &out}}},
		{Raw: []byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"),
			Chunk: &schema.ChatChunk{Type: "message_stop"}},
	}
	return func(yield func(*providers.StreamEvent, error) bool) {
		for _, ev := range events {
			if !yield(ev, nil) {
				return
			}
		}
	}, nil
}
