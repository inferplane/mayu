package bedrock

import (
	"context"
	"strings"
	"testing"

	"github.com/inferplane/inferplane/providers"
)

func TestCanonicalToConverseExtractsTextAndSystem(t *testing.T) {
	raw := []byte(`{"model":"moonshot.kimi-k2","max_tokens":256,"system":[{"type":"text","text":"be brief"}],"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]},{"role":"assistant","content":"hi"}],"model_fields":{"top_k":40}}`)
	cr, err := toConverseRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if cr.System != "be brief" {
		t.Fatalf("system: %q", cr.System)
	}
	if len(cr.Messages) != 2 || cr.Messages[0].Role != "user" || cr.Messages[0].Text != "hello" || cr.Messages[1].Text != "hi" {
		t.Fatalf("messages: %+v", cr.Messages)
	}
	if cr.ModelFields["top_k"].(float64) != 40 {
		t.Fatalf("model_fields not carried: %+v", cr.ModelFields)
	}
}

func TestProviderCompleteConverse(t *testing.T) {
	fc := &fakeConverser{resp: ConverseResponse{Text: "brief answer", StopReason: "end_turn", InputTokens: 5, OutputTokens: 3}}
	p := &provider{conv: fc, modelAPI: map[string]string{"moonshot.kimi-k2": "converse"}}
	raw := []byte(`{"model":"kimi-k2","messages":[{"role":"user","content":"q"}]}`)
	resp, err := p.Complete(context.Background(), &providers.ProxyRequest{Model: "kimi-k2", Upstream: "moonshot.kimi-k2", RawBody: raw})
	if err != nil {
		t.Fatal(err)
	}
	// the converse response must be rendered back into an Anthropic-shaped body
	if resp.StatusCode != 200 || !strings.Contains(string(resp.RawBody), "brief answer") {
		t.Fatalf("resp body: %s", resp.RawBody)
	}
	if resp.Parsed == nil || resp.Parsed.Usage == nil || *resp.Parsed.Usage.OutputTokens != 3 {
		t.Fatalf("usage: %+v", resp.Parsed)
	}
}

func TestProviderStreamConverse(t *testing.T) {
	fc := &fakeConverser{streamEv: []ConverseStreamEvent{
		{TextDelta: "par"}, {TextDelta: "tial"}, {Done: true, StopReason: "end_turn", InputTokens: 5, OutputTokens: 4},
	}}
	p := &provider{conv: fc, modelAPI: map[string]string{"glm.glm-4": "converse"}}
	seq, err := p.Stream(context.Background(), &providers.ProxyRequest{Model: "glm-4", Upstream: "glm.glm-4", RawBody: []byte(`{"messages":[]}`), Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	var sse strings.Builder
	for ev, err := range seq {
		if err != nil {
			t.Fatal(err)
		}
		sse.WriteString(string(ev.Raw))
	}
	s := sse.String()
	// must produce a well-formed Anthropic SSE sequence carrying the deltas.
	// Each ConverseStreamEvent maps to one content_block_delta, so the two
	// deltas "par" and "tial" appear as separate text_delta events (they are
	// NOT concatenated into "partial" — that would defeat streaming).
	if !strings.Contains(s, "event: message_start") || !strings.Contains(s, "event: message_stop") ||
		!strings.Contains(s, `"text":"par"`) || !strings.Contains(s, `"text":"tial"`) {
		t.Fatalf("converse stream not rendered as Anthropic SSE: %s", s)
	}
}
