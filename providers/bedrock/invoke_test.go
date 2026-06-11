package bedrock

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/inferplane/inferplane/providers"
)

func TestToInvokeBodyStripsModelAddsVersionPreservesCachePrefix(t *testing.T) {
	in := []byte(`{"model":"claude-sonnet-4-6","max_tokens":1024,"system":[{"type":"text","text":"sys","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]}]}`)
	out, err := toInvokeBody(in)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	json.Unmarshal(out, &m)
	if _, has := m["model"]; has {
		t.Fatal("model must be stripped (it's in the URL for InvokeModel)")
	}
	if string(m["anthropic_version"]) != `"bedrock-2023-05-31"` {
		t.Fatalf("anthropic_version not injected: %s", m["anthropic_version"])
	}
	// cache-relevant prefix (system/messages) bytes must be IDENTICAL to input
	var inMap map[string]json.RawMessage
	json.Unmarshal(in, &inMap)
	if string(m["system"]) != string(inMap["system"]) {
		t.Fatalf("system bytes mutated:\n got: %s\nwant: %s", m["system"], inMap["system"])
	}
	if string(m["messages"]) != string(inMap["messages"]) {
		t.Fatalf("messages bytes mutated:\n got: %s\nwant: %s", m["messages"], inMap["messages"])
	}
}

func TestToInvokeBodyStripsStream(t *testing.T) {
	// Bedrock's Anthropic InvokeModel schema rejects a top-level "stream" key
	// ("stream: Extra inputs are not permitted" — streaming is selected by the
	// InvokeModelWithResponseStream OPERATION, not the body). Verified against
	// the live service 2026-06-12; leaving it in 502s every streaming request.
	in := []byte(`{"model":"m","stream":true,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	out, err := toInvokeBody(in)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	json.Unmarshal(out, &m)
	if _, has := m["stream"]; has {
		t.Fatal(`"stream" must be stripped (Bedrock rejects it as an extra input)`)
	}
}

func TestToInvokeBodyKeepsExistingAnthropicVersion(t *testing.T) {
	// if a client already set anthropic_version, don't clobber a beta the user chose
	in := []byte(`{"model":"m","anthropic_version":"bedrock-2023-05-31","messages":[]}`)
	out, _ := toInvokeBody(in)
	var m map[string]json.RawMessage
	json.Unmarshal(out, &m)
	if string(m["anthropic_version"]) != `"bedrock-2023-05-31"` {
		t.Fatalf("version: %s", m["anthropic_version"])
	}
}

func TestProviderCompleteInvoke(t *testing.T) {
	fi := &fakeInvoker{respBody: []byte(`{"id":"msg_b","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":7,"output_tokens":2}}`)}
	p := &provider{inv: fi, modelAPI: map[string]string{}}
	raw := []byte(`{"model":"claude-sonnet-4-6","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	resp, err := p.Complete(context.Background(), &providers.ProxyRequest{Model: "claude-sonnet-4-6", Upstream: "anthropic.claude-sonnet-4-6-v1:0", RawBody: raw})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || resp.Parsed == nil || resp.Parsed.Usage == nil || *resp.Parsed.Usage.InputTokens != 7 {
		t.Fatalf("resp: %+v", resp.Parsed)
	}
	// the invoker must have received the URL modelId and a model-less, versioned body
	if fi.gotModelID != "anthropic.claude-sonnet-4-6-v1:0" {
		t.Fatalf("modelID: %q", fi.gotModelID)
	}
	var sent map[string]json.RawMessage
	json.Unmarshal(fi.gotBody, &sent)
	if _, has := sent["model"]; has {
		t.Fatal("sent body still has model")
	}
	if string(sent["anthropic_version"]) != `"bedrock-2023-05-31"` {
		t.Fatal("sent body missing anthropic_version")
	}
}

func TestStreamInvokeReserializesToAnthropicSSE(t *testing.T) {
	// Bedrock event-stream payloads are Anthropic SSE event JSON. Provider must
	// re-emit them as Anthropic SSE bytes (Raw) + parsed Chunk, preserving the
	// thinking→text block ORDER.
	payloads := [][]byte{
		[]byte(`{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"x","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":3,"output_tokens":1}}}`),
		[]byte(`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`),
		[]byte(`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"reason"}}`),
		[]byte(`{"type":"content_block_stop","index":0}`),
		[]byte(`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`),
		[]byte(`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"answer"}}`),
		[]byte(`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"input_tokens":3,"output_tokens":9}}`),
		[]byte(`{"type":"message_stop"}`),
	}
	fi := &fakeInvoker{streamRaw: payloads}
	p := &provider{inv: fi, modelAPI: map[string]string{}}
	seq, err := p.Stream(context.Background(), &providers.ProxyRequest{Model: "claude-sonnet-4-6", Upstream: "anthropic.claude-sonnet-4-6-v1:0", RawBody: []byte(`{"model":"m","stream":true,"messages":[]}`), Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	var sse strings.Builder
	var types []string
	var lastOut int64
	for ev, err := range seq {
		if err != nil {
			t.Fatal(err)
		}
		sse.WriteString(string(ev.Raw))
		if ev.Chunk != nil {
			types = append(types, ev.Chunk.Type)
			if ev.Chunk.Usage != nil && ev.Chunk.Usage.OutputTokens != nil {
				lastOut = *ev.Chunk.Usage.OutputTokens
			}
		}
	}
	// Raw must be valid Anthropic SSE
	if !strings.Contains(sse.String(), "event: message_start\n") || !strings.Contains(sse.String(), "event: message_stop\n") {
		t.Fatalf("not Anthropic SSE: %s", sse.String())
	}
	// thinking block (index 0) must precede text block (index 1)
	joined := strings.Join(types, ",")
	wantOrder := "message_start,content_block_start,content_block_delta,content_block_stop,content_block_start,content_block_delta,message_delta,message_stop"
	if joined != wantOrder {
		t.Fatalf("block order broken:\n got: %s\nwant: %s", joined, wantOrder)
	}
	if lastOut != 9 {
		t.Fatalf("usage: %d", lastOut)
	}
	// verify the thinking delta is before the text delta in the raw SSE
	ti := strings.Index(sse.String(), "thinking_delta")
	xi := strings.Index(sse.String(), "text_delta")
	if ti < 0 || xi < 0 || ti > xi {
		t.Fatalf("thinking must precede text in SSE output")
	}
}
