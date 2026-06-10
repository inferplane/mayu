package bedrock

import (
	"context"
	"encoding/json"
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
