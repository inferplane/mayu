package bedrock

import (
	"encoding/json"
	"testing"
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
