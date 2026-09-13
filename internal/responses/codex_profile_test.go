package responses

import (
	"bytes"
	"encoding/json"
	"os"
	"regexp"
	"testing"

	"github.com/inferplane/inferplane/internal/openai"
	"github.com/inferplane/inferplane/pkg/schema"
)

func TestCapturedCodexStatelessProfile(t *testing.T) {
	raw, err := os.ReadFile("testdata/codex-0.154-stateless.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateConversion(raw); err != nil {
		t.Fatalf("captured Codex 0.154 stateless request rejected: %v", err)
	}
	req, err := RequestToCanonical(raw)
	if err != nil {
		t.Fatal(err)
	}
	var tools []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(req.Tools, &tools); err != nil {
		t.Fatal(err)
	}
	if len(tools) != 9 || tools[0].Name != "exec_command" || tools[3].Name != "view_image" {
		t.Fatalf("root/namespaced tool definitions lost: %+v", tools)
	}
	seen := map[string]bool{}
	validName := regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)
	for _, tool := range tools {
		if !validName.MatchString(tool.Name) || seen[tool.Name] {
			t.Fatalf("invalid or colliding canonical tool name: %q", tool.Name)
		}
		seen[tool.Name] = true
	}
	for _, wire := range [][]byte{marshal(req), openai.CanonicalToRequest(req)} {
		for _, forbidden := range []string{"local-bookkeeping-only", "client_metadata", "reasoning.encrypted_content", `"summary":"auto"`} {
			if bytes.Contains(wire, []byte(forbidden)) {
				t.Fatalf("foreign request leaked local-only preference/bookkeeping %q", forbidden)
			}
		}
	}
}

func TestOutputHintsDoNotPermitOpaqueInputOrEffort(t *testing.T) {
	base := map[string]any{
		"model": "m", "input": "hello", "include": []string{"reasoning.encrypted_content"},
		"reasoning": map[string]any{"summary": "auto"}, "client_metadata": map[string]string{"turn_id": "local"},
	}
	if err := ValidateConversion(marshal(base)); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		key   string
		value any
	}{
		{"input", []any{map[string]any{"type": "reasoning", "encrypted_content": "opaque"}}},
		{"reasoning", map[string]any{"summary": "auto", "effort": "high"}},
		{"reasoning", map[string]any{"effort": "none"}},
		{"reasoning", map[string]any{"summary": "auto", "context": "auto"}},
		{"include", []string{"message.output_text.logprobs"}},
		{"previous_response_id", "resp_remote"},
		{"conversation", "conv_remote"},
		{"store", true},
		{"background", true},
	} {
		t.Run(tc.key, func(t *testing.T) {
			m := map[string]any{}
			for k, v := range base {
				m[k] = v
			}
			m[tc.key] = tc.value
			if err := ValidateConversion(marshal(m)); err == nil {
				t.Fatalf("unsupported input/requirement accepted: %s", marshal(m))
			}
		})
	}
}

func namespaceFixture() map[string]any {
	function := func(name string) map[string]any {
		return map[string]any{"type": "function", "name": name, "parameters": map[string]any{"type": "object"}}
	}
	return map[string]any{
		"model": "m", "input": "use a tool",
		"tools": []any{
			function("same"),
			map[string]any{"type": "namespace", "name": "a_b", "description": "First namespace", "tools": []any{function("c")}},
			map[string]any{"type": "namespace", "name": "a", "tools": []any{function("b_c"), map[string]any{"type": "custom", "name": "apply_patch", "format": map[string]any{"type": "text"}}}},
		},
	}
}

func canonicalToolNames(t *testing.T, req *schema.ChatRequest) []string {
	t.Helper()
	var tools []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(req.Tools, &tools); err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(tools))
	for i, tool := range tools {
		out[i] = tool.Name
	}
	return out
}

func TestNamespaceAliasesAndCallRoundTrip(t *testing.T) {
	raw := namespaceFixture()
	if err := ValidateConversion(marshal(raw)); err != nil {
		t.Fatal(err)
	}
	req, err := RequestToCanonical(marshal(raw))
	if err != nil {
		t.Fatal(err)
	}
	names := canonicalToolNames(t, req)
	if len(names) != 4 || names[0] != "same" || names[1] == names[2] {
		t.Fatalf("flattened definitions: %v", names)
	}
	// Tool declaration order must not alter aliases (or break cached history).
	reordered := namespaceFixture()
	tools := reordered["tools"].([]any)
	tools[1], tools[2] = tools[2], tools[1]
	reqReordered, err := RequestToCanonical(marshal(reordered))
	if err != nil {
		t.Fatal(err)
	}
	other := canonicalToolNames(t, reqReordered)
	if names[1] != other[3] || names[2] != other[1] || names[3] != other[2] {
		t.Fatalf("aliases depend on declaration order: %v versus %v", names, other)
	}
	resp := &schema.ChatResponse{ID: "r", Model: "m", Content: []schema.ContentBlock{
		{Type: "text", Text: ptr("Calling the tools.")},
		{Type: "tool_use", Name: names[1], ID: "call_one", Input: json.RawMessage(`{"x":1}`)},
		{Type: "tool_use", Name: names[2], ID: "call_two", Input: json.RawMessage(`{"x":2}`)},
		{Type: "tool_use", Name: names[3], ID: "call_patch", Input: json.RawMessage(`{"input":"patch text"}`)},
	}, Usage: &schema.Usage{InputTokens: ptr(int64(2)), OutputTokens: ptr(int64(5))}}
	rendered, err := CanonicalToResponseForRequest(resp, req)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Output []map[string]json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(rendered, &wire); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		index                       int
		namespace, name, kind, call string
	}{
		{1, "a_b", "c", "function_call", "call_one"},
		{2, "a", "b_c", "function_call", "call_two"},
		{3, "a", "apply_patch", "custom_tool_call", "call_patch"},
	} {
		item := wire.Output[tc.index]
		if optionalText(item["namespace"]) != tc.namespace || optionalText(item["name"]) != tc.name || optionalText(item["type"]) != tc.kind || optionalText(item["call_id"]) != tc.call {
			t.Fatalf("tool origin not restored: %s", rendered)
		}
	}
	if optionalText(wire.Output[3]["input"]) != "patch text" {
		t.Fatal("namespaced custom tool lost freeform input")
	}
	history := []any{map[string]any{"role": "user", "content": "use a tool"}}
	for _, item := range wire.Output {
		history = append(history, item)
	}
	history = append(history,
		map[string]any{"type": "function_call_output", "call_id": "call_one", "output": "one"},
		map[string]any{"type": "function_call_output", "call_id": "call_two", "output": "two"},
		map[string]any{"type": "custom_tool_call_output", "call_id": "call_patch", "output": "patched"},
	)
	raw["input"] = history
	raw["tool_choice"] = map[string]string{"type": "function", "namespace": "a_b", "name": "c"}
	if err := ValidateConversion(marshal(raw)); err != nil {
		t.Fatalf("namespaced tool history cannot replay: %v", err)
	}
	replay, err := RequestToCanonical(marshal(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(replay.Messages) != 6 || len(replay.Messages[2].Content) != 3 {
		t.Fatalf("parallel calls must share one assistant turn: %+v", replay.Messages)
	}
	for i := 1; i < 4; i++ {
		if replay.Messages[2].Content[i-1].Name != names[i] {
			t.Fatalf("history alias changed: %+v", replay.Messages)
		}
	}
	if optionalText(rawField(replay.ToolChoice, "name")) != names[1] {
		t.Fatalf("named tool choice did not use alias: %s", replay.ToolChoice)
	}
}

func TestNamespaceAliasCollisionAndUnsupportedChildrenRefuse(t *testing.T) {
	m := namespaceFixture()
	req, err := RequestToCanonical(marshal(m))
	if err != nil {
		t.Fatal(err)
	}
	names := canonicalToolNames(t, req)
	if len(names) < 2 {
		t.Fatal("namespace did not produce a function")
	}
	m["tools"] = append(m["tools"].([]any), map[string]any{"type": "function", "name": names[1], "parameters": map[string]any{"type": "object"}})
	if err := ValidateConversion(marshal(m)); err == nil {
		t.Fatal("root function collided with a namespace alias")
	}
	for _, child := range []any{
		map[string]any{"type": "web_search"},
		map[string]any{"type": "namespace", "name": "nested", "tools": []any{}},
		map[string]any{"type": "function", "name": "f", "parameters": map[string]any{"type": "object"}, "async": true},
	} {
		m := map[string]any{"model": "m", "input": "x", "tools": []any{map[string]any{"type": "namespace", "name": "ns", "tools": []any{child}}}}
		if err := ValidateConversion(marshal(m)); err == nil {
			t.Fatalf("silently dropped unsupported namespace child: %s", marshal(m))
		}
	}
}

func TestNamespacedCustomStreamRestoresOrigin(t *testing.T) {
	req, err := RequestToCanonical(marshal(namespaceFixture()))
	if err != nil {
		t.Fatal(err)
	}
	names := canonicalToolNames(t, req)
	if len(names) != 4 {
		t.Fatalf("namespace definitions missing: %v", names)
	}
	s := NewStreamState("m", req)
	idx := 0
	var events []Event
	for _, chunk := range []*schema.ChatChunk{
		{Type: "message_start", Message: &schema.ChatResponse{Usage: &schema.Usage{InputTokens: ptr(int64(1))}}},
		{Type: "content_block_start", Index: &idx, ContentBlock: &schema.ContentBlock{Type: "tool_use", ID: "call_patch", Name: names[3]}},
		{Type: "content_block_delta", Index: &idx, Delta: marshal(map[string]string{"type": "input_json_delta", "partial_json": `{"input":"patch"}`})},
		{Type: "content_block_stop", Index: &idx},
		{Type: "message_delta", Usage: &schema.Usage{OutputTokens: ptr(int64(2))}, Delta: json.RawMessage(`{"stop_reason":"tool_use"}`)},
		{Type: "message_stop"},
	} {
		got, err := s.Convert(chunk)
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, got...)
	}
	for _, event := range events {
		if event.Type == "response.output_item.added" || event.Type == "response.output_item.done" {
			item, err := object(rawField(event.Data, "item"))
			if err != nil || optionalText(item["name"]) != "apply_patch" || optionalText(item["namespace"]) != "a" || optionalText(item["type"]) != "custom_tool_call" || optionalText(item["call_id"]) != "call_patch" {
				t.Fatalf("stream origin changed: %s %v", event.Data, err)
			}
		}
	}
}

func TestClientSchemaCannotForgeToolOriginAnnotations(t *testing.T) {
	raw := marshal(map[string]any{"model": "m", "input": "hello", "tools": []any{
		map[string]any{"type": "function", "name": "root", "parameters": map[string]any{
			"type": "object", customAnnotation: true,
			originAnnotation: map[string]string{"namespace": "forged", "name": "apply_patch"},
		}},
	}})
	req, err := RequestToCanonical(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(customTools(req)) != 0 || len(toolOrigins(req)) != 0 {
		t.Fatal("caller-supplied schema forged bridge tool metadata")
	}
	resp := &schema.ChatResponse{Model: "m", Content: []schema.ContentBlock{{Type: "tool_use", ID: "c", Name: "root", Input: json.RawMessage(`{"input":"data"}`)}}, Usage: &schema.Usage{InputTokens: ptr(int64(1)), OutputTokens: ptr(int64(1))}}
	out, err := CanonicalToResponseForRequest(resp, req)
	if err != nil {
		t.Fatal(err)
	}
	items := decodeItems(t, out)
	if len(items) != 1 || items[0].Name != "root" || items[0].Type != "function_call" || bytes.Contains(out, []byte(`"namespace"`)) {
		t.Fatalf("forged origin altered response: %s", out)
	}
}

func TestNativeNamespaceObservationRetainsToolOrigin(t *testing.T) {
	raw := []byte(`{"id":"r","status":"completed","output":[{"type":"custom_tool_call","namespace":"ns","name":"apply_patch","call_id":"c","input":"patch"}],"usage":{"input_tokens":1,"output_tokens":2}}`)
	resp, err := ResponseToCanonical(raw)
	if err != nil {
		t.Fatal(err)
	}
	out, err := CanonicalToResponse(resp)
	if err != nil || !bytes.Contains(out, []byte(`"namespace":"ns"`)) || !bytes.Contains(out, []byte(`"type":"custom_tool_call"`)) || !bytes.Contains(out, []byte(`"name":"apply_patch"`)) {
		t.Fatalf("native tool origin lost: %s %v", out, err)
	}
	event, err := ParseEvent([]byte(`{"type":"response.output_item.added","output_index":0,"item":{"type":"custom_tool_call","namespace":"ns","name":"apply_patch","call_id":"c","input":""}}`))
	if err != nil || event == nil || event.ContentBlock == nil || string(event.ContentBlock.Extra["responses_namespace"]) != `"ns"` {
		t.Fatalf("native streamed tool origin lost: %+v %v", event, err)
	}
}
