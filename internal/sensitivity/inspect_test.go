package sensitivity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
)

func TestInspectContent(t *testing.T) {
	tests := []struct {
		name, protocol, raw string
		categories          []string
		incomplete          bool
		tools, vision       bool
		reasoning           bool
	}{
		{name: "system", protocol: "anthropic", raw: `{"system":"contact alice@example.test","messages":[{"role":"user","content":"hello"}]}`, categories: []string{"email"}},
		{name: "system blocks and cache control", protocol: "anthropic", raw: `{"system":[{"type":"text","text":"alice@example.test","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"hello"}]}`, categories: []string{"email"}},
		{name: "nested tool result", protocol: "anthropic", raw: `{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":[{"type":"text","text":"alice@example.test"}]}]}]}`, categories: []string{"email"}, tools: true},
		{name: "tool input keys and values", protocol: "anthropic", raw: `{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"f","input":{"alice@example.test":{"next":["bob@example.test"]}}}]}]}`, categories: []string{"email"}, tools: true},
		{name: "application types in tool input", protocol: "anthropic", raw: `{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"f","input":{"type":"invoice","nested":{"type":"image","owner":"alice@example.test"}}}]}]}`, categories: []string{"email"}, tools: true},
		{name: "application type in encoded arguments", protocol: "openai", raw: `{"messages":[{"role":"assistant","content":null,"tool_calls":[{"type":"function","function":{"name":"f","arguments":"{\"type\":\"document\",\"owner\":\"alice@example.test\"}"}}]}]}`, categories: []string{"email"}, tools: true},
		{name: "application type in tool result text", protocol: "anthropic", raw: `{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"{\"type\":\"image\",\"owner\":\"alice@example.test\"}"}]}]}`, categories: []string{"email"}, tools: true},
		{name: "escaped tool arguments", protocol: "openai", raw: `{"messages":[{"role":"assistant","content":null,"tool_calls":[{"id":"t","type":"function","function":{"name":"f","arguments":"{\"contact\":\"alice\\u0040example.test\"}"}}]}]}`, categories: []string{"email"}, tools: true},
		{name: "developer and tool history", protocol: "openai", raw: `{"messages":[{"role":"developer","content":"alice@example.test"},{"role":"tool","tool_call_id":"t","content":"bob@example.test"}]}`, categories: []string{"email"}, tools: true},
		{name: "metadata keys", protocol: "openai", raw: `{"metadata":{"alice@example.test":"safe"},"messages":[{"role":"user","content":"hello"}]}`, categories: []string{"email"}},
		{name: "tool definition schema", protocol: "openai", raw: `{"tools":[{"type":"function","function":{"name":"f","description":"alice@example.test","parameters":{"type":"object","properties":{"email":{"type":"string"}}}}}],"messages":[{"role":"user","content":"hello"}]}`, categories: []string{"email"}, tools: true},
		{name: "bedrock inner body", protocol: "bedrock", raw: `{"anthropic_version":"bedrock-2023-05-31","messages":[{"role":"user","content":"alice@example.test"}]}`, categories: []string{"email"}},
		{name: "bedrock text blocks", protocol: "bedrock", raw: `{"system":[{"text":"alice@example.test"}],"messages":[{"role":"user","content":[{"text":"hello"}]}]}`, categories: []string{"email"}},
		{name: "bedrock tool result", protocol: "bedrock", raw: `{"messages":[{"role":"user","content":[{"toolResult":{"toolUseId":"t","content":[{"json":{"email":"alice@example.test"}}]}}]}]}`, categories: []string{"email"}, tools: true},
		{name: "no pii", protocol: "anthropic", raw: `{"model":"m","messages":[{"role":"user","content":"write a sorting function"}]}`},
		{name: "image", protocol: "anthropic", raw: `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","data":"YWJj"}},{"type":"text","text":"alice@example.test"}]}]}`, categories: []string{"email"}, incomplete: true, vision: true},
		{name: "remote image", protocol: "openai", raw: `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.test/private"}}]}]}`, incomplete: true, vision: true},
		{name: "document", protocol: "anthropic", raw: `{"messages":[{"role":"user","content":[{"type":"document","source":{"type":"text","data":"alice@example.test"}}]}]}`, categories: []string{"email"}, incomplete: true},
		{name: "audio", protocol: "openai", raw: `{"messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"YWJj","format":"wav"}}]}]}`, incomplete: true},
		{name: "file", protocol: "openai", raw: `{"messages":[{"role":"user","content":[{"type":"file","file":{"file_id":"f"}}]}]}`, incomplete: true},
		{name: "redacted thinking", protocol: "anthropic", raw: `{"messages":[{"role":"assistant","content":[{"type":"redacted_thinking","data":"YWJj"}]}]}`, incomplete: true, reasoning: true},
		{name: "encrypted reasoning", protocol: "openai", raw: `{"messages":[{"role":"assistant","content":[{"type":"reasoning","encrypted_content":"YWJj"}]}]}`, incomplete: true, reasoning: true},
		{name: "thinking text", protocol: "anthropic", raw: `{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"alice@example.test","signature":"sig"}]}]}`, categories: []string{"email"}, reasoning: true},
		{name: "bedrock image", protocol: "bedrock", raw: `{"messages":[{"role":"user","content":[{"image":{"format":"png","source":{"bytes":"YWJj"}}}]}]}`, incomplete: true, vision: true},
		{name: "bedrock reasoning", protocol: "bedrock", raw: `{"messages":[{"role":"assistant","content":[{"reasoningContent":{"reasoningText":{"text":"alice@example.test","signature":"sig"}}}]}]}`, categories: []string{"email"}, reasoning: true},
		{name: "bedrock redaction", protocol: "bedrock", raw: `{"messages":[{"role":"assistant","content":[{"reasoningContent":{"redactedContent":"YWJj"}}]}]}`, incomplete: true, reasoning: true},
		{name: "unknown block", protocol: "anthropic", raw: `{"messages":[{"role":"user","content":[{"type":"future_block","text":"alice@example.test"}]}]}`, categories: []string{"email"}, incomplete: true},
		{name: "missing block type", protocol: "openai", raw: `{"messages":[{"role":"user","content":[{"text":"alice@example.test"}]}]}`, categories: []string{"email"}, incomplete: true},
		{name: "invalid text shape", protocol: "anthropic", raw: `{"messages":[{"role":"user","content":[{"type":"text","text":{"secret":"alice@example.test"}}]}]}`, categories: []string{"email"}, incomplete: true},
		{name: "unknown text block member", protocol: "anthropic", raw: `{"messages":[{"role":"user","content":[{"type":"text","text":"safe","source":{"type":"base64","data":"YWJj"}}]}]}`, incomplete: true},
		{name: "wrong protocol block", protocol: "openai", raw: `{"messages":[{"role":"user","content":[{"type":"tool_result","content":"safe"}]}]}`, incomplete: true, tools: true},
		{name: "unknown tool call", protocol: "openai", raw: `{"messages":[{"role":"assistant","content":null,"tool_calls":[{"type":"future","payload":"opaque"}]}]}`, incomplete: true, tools: true},
		{name: "malformed tool call shape", protocol: "openai", raw: `{"messages":[{"role":"assistant","content":null,"tool_calls":"opaque"}]}`, incomplete: true, tools: true},
		{name: "opaque reasoning details", protocol: "openai", raw: `{"messages":[{"role":"assistant","content":"safe","reasoning_details":[{"type":"reasoning.encrypted","data":"YWJj"}]}]}`, incomplete: true, reasoning: true},
		{name: "unknown body", protocol: "bedrock", raw: `{"prompt":"alice@example.test"}`, categories: []string{"email"}, incomplete: true},
		{name: "unknown role", protocol: "openai", raw: `{"messages":[{"role":"future","content":"alice@example.test"}]}`, categories: []string{"email"}, incomplete: true},
		{name: "missing messages", protocol: "anthropic", raw: `{}`, incomplete: true},
		{name: "empty messages", protocol: "openai", raw: `{"messages":[]}`, incomplete: true},
		{name: "invalid messages", protocol: "openai", raw: `{"messages":"alice@example.test"}`, categories: []string{"email"}, incomplete: true},
		{name: "invalid message", protocol: "openai", raw: `{"messages":[42]}`, incomplete: true},
		{name: "missing content", protocol: "anthropic", raw: `{"messages":[{"role":"user"}]}`, incomplete: true},
		{name: "null user content", protocol: "openai", raw: `{"messages":[{"role":"user","content":null}]}`, incomplete: true},
		{name: "array root", protocol: "openai", raw: `["alice@example.test"]`, categories: []string{"email"}, incomplete: true},
		{name: "null root", protocol: "openai", raw: `null`, incomplete: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := []byte(tt.raw)
			before := bytes.Clone(raw)
			got, err := NewInspector().Inspect(context.Background(), tt.protocol, raw)
			if err != nil {
				t.Fatal(err)
			}
			if got.Complete == tt.incomplete || !slices.Equal(got.Categories, tt.categories) ||
				got.HasTools != tt.tools || got.HasVision != tt.vision || got.HasReasoning != tt.reasoning {
				t.Fatalf("inspection = %+v", got)
			}
			if !bytes.Equal(raw, before) {
				t.Fatal("inspection mutated request")
			}
			encoded, err := json.Marshal(got)
			if err != nil || bytes.Contains(encoded, []byte("alice")) {
				t.Fatalf("result retained content: %s, %v", encoded, err)
			}
		})
	}
}

func TestInspectContextSignals(t *testing.T) {
	tests := []struct {
		name, protocol, raw  string
		users                int
		history, tools       bool
		reasoning, structure bool
	}{
		{"single user", "anthropic", `{"system":"instructions","messages":[{"role":"user","content":"hi"}]}`, 1, false, false, false, false},
		{"system developer user", "openai", `{"messages":[{"role":"system","content":"system"},{"role":"developer","content":"developer"},{"role":"user","content":"hi"}]}`, 1, false, false, false, false},
		{"assistant history", "openai", `{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"hello"},{"role":"user","content":"again"}]}`, 2, true, false, false, false},
		{"two user turns", "anthropic", `{"messages":[{"role":"user","content":"hi"},{"role":"user","content":"again"}]}`, 2, true, false, false, false},
		{"tool history", "openai", `{"messages":[{"role":"tool","content":"done","tool_call_id":"t"},{"role":"user","content":"hi"}]}`, 1, true, true, false, false},
		{"tools", "anthropic", `{"tools":[{"name":"f","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"hi"}]}`, 1, false, true, false, false},
		{"legacy functions", "openai", `{"functions":[{"name":"f","parameters":{"type":"object"}}],"messages":[{"role":"user","content":"hi"}]}`, 1, false, true, false, false},
		{"empty tools", "openai", `{"tools":[],"messages":[{"role":"user","content":"hi"}]}`, 1, false, false, false, false},
		{"tool choice", "openai", `{"tool_choice":"required","messages":[{"role":"user","content":"hi"}]}`, 1, false, true, false, false},
		{"thinking", "anthropic", `{"thinking":{"type":"enabled","budget_tokens":1024},"messages":[{"role":"user","content":"hi"}]}`, 1, false, false, true, false},
		{"disabled thinking", "anthropic", `{"thinking":{"type":"disabled"},"messages":[{"role":"user","content":"hi"}]}`, 1, false, false, false, false},
		{"reasoning effort", "openai", `{"reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`, 1, false, false, true, false},
		{"reasoning text", "openai", `{"messages":[{"role":"assistant","content":"ok","reasoning_content":"analysis"},{"role":"user","content":"hi"}]}`, 1, true, false, true, false},
		{"response format", "openai", `{"response_format":{"type":"json_schema","json_schema":{"name":"f","schema":{"type":"object"}}},"messages":[{"role":"user","content":"hi"}]}`, 1, false, false, false, true},
		{"output format", "anthropic", `{"output_config":{"format":{"type":"json_schema","schema":{"type":"object"}}},"messages":[{"role":"user","content":"hi"}]}`, 1, false, false, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NewInspector().Inspect(context.Background(), tt.protocol, []byte(tt.raw))
			if err != nil || !got.Complete || got.UserTurns != tt.users || got.HasHistory != tt.history ||
				got.HasTools != tt.tools || got.HasReasoning != tt.reasoning || got.HasStructuredOutput != tt.structure {
				t.Fatalf("inspection = %+v, %v", got, err)
			}
		})
	}
}

func TestInspectProtocolObjectMembers(t *testing.T) {
	tests := []struct {
		name, protocol, raw string
		incomplete          bool
		tools, reasoning    bool
		categories          []string
	}{
		{"openai call opaque member", "openai", `{"messages":[{"role":"assistant","content":null,"tool_calls":[{"type":"function","function":{"name":"f","arguments":"{}"},"encrypted_content":"YWJj"}]}]}`, true, true, false, nil},
		{"openai function opaque member", "openai", `{"messages":[{"role":"assistant","content":null,"tool_calls":[{"type":"function","function":{"name":"f","arguments":"{}","encrypted_content":"YWJj"}}]}]}`, true, true, false, nil},
		{"openai legacy function opaque member", "openai", `{"messages":[{"role":"assistant","content":null,"function_call":{"name":"f","arguments":"{}","encrypted_content":"YWJj"}}]}`, true, true, false, nil},
		{"openai call unknown member", "openai", `{"messages":[{"role":"assistant","content":null,"tool_calls":[{"type":"function","function":{"name":"f","arguments":"{}"},"future":{"data":"YWJj"}}]}]}`, true, true, false, nil},
		{"openai call malformed id", "openai", `{"messages":[{"role":"assistant","content":null,"tool_calls":[{"id":{"encrypted_content":"YWJj"},"type":"function","function":{"name":"f","arguments":"{}"}}]}]}`, true, true, false, nil},
		{"openai application arguments", "openai", `{"messages":[{"role":"assistant","content":null,"tool_calls":[{"id":"t","type":"function","function":{"name":"f","arguments":"{\"type\":\"image\",\"encrypted_content\":\"alice@example.test\"}"}}]}]}`, false, true, false, []string{"email"}},
		{"bedrock tool use opaque member", "bedrock", `{"messages":[{"role":"assistant","content":[{"toolUse":{"toolUseId":"t","name":"f","input":{},"encrypted_content":"YWJj"}}]}]}`, true, true, false, nil},
		{"bedrock tool result opaque member", "bedrock", `{"messages":[{"role":"user","content":[{"toolResult":{"toolUseId":"t","content":[{"json":{}}],"encrypted_content":"YWJj"}}]}]}`, true, true, false, nil},
		{"bedrock application input", "bedrock", `{"messages":[{"role":"assistant","content":[{"toolUse":{"toolUseId":"t","name":"f","input":{"type":"image","encrypted_content":"alice@example.test"}}}]}]}`, false, true, false, []string{"email"}},
		{"bedrock application result", "bedrock", `{"messages":[{"role":"user","content":[{"toolResult":{"toolUseId":"t","content":[{"json":{"type":"document","redactedContent":"alice@example.test"}}]}}]}]}`, false, true, false, []string{"email"}},
		{"bedrock reasoning text opaque member", "bedrock", `{"messages":[{"role":"assistant","content":[{"reasoningContent":{"reasoningText":{"text":"safe","redactedContent":"YWJj"}}}]}]}`, true, false, true, nil},
		{"bedrock reasoning malformed signature", "bedrock", `{"messages":[{"role":"assistant","content":[{"reasoningContent":{"reasoningText":{"text":"safe","signature":{"redactedContent":"YWJj"}}}}]}]}`, true, false, true, nil},
		{"bedrock reasoning text control", "bedrock", `{"messages":[{"role":"assistant","content":[{"reasoningContent":{"reasoningText":{"text":"safe","signature":"sig"}}}]}]}`, false, false, true, nil},
		{"openai reasoning option opaque member", "openai", `{"reasoning":{"effort":"high","encrypted_content":"YWJj"},"messages":[{"role":"user","content":"safe"}]}`, true, false, true, nil},
		{"openai reasoning option malformed effort", "openai", `{"reasoning":{"effort":{"encrypted_content":"YWJj"}},"messages":[{"role":"user","content":"safe"}]}`, true, false, true, nil},
		{"openai reasoning option control", "openai", `{"reasoning":{"effort":"high","summary":"auto"},"messages":[{"role":"user","content":"safe"}]}`, false, false, true, nil},
		{"openai reasoning text control", "openai", `{"messages":[{"role":"assistant","content":[{"type":"reasoning","text":"safe"}]}]}`, false, false, true, nil},
		{"anthropic wrong reasoning block", "anthropic", `{"messages":[{"role":"assistant","content":[{"type":"reasoning","text":"safe"}]}]}`, true, false, true, nil},
		{"bedrock wrong reasoning block", "bedrock", `{"messages":[{"role":"assistant","content":[{"type":"reasoning","text":"safe"}]}]}`, true, false, true, nil},
		{"anthropic thinking opaque option", "anthropic", `{"thinking":{"type":"enabled","budget_tokens":1024,"encrypted_content":"YWJj"},"messages":[{"role":"user","content":"safe"}]}`, true, false, true, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := []byte(tt.raw)
			before := bytes.Clone(raw)
			got, err := NewInspector().Inspect(context.Background(), tt.protocol, raw)
			if err != nil || got.Complete == tt.incomplete || got.HasTools != tt.tools ||
				got.HasReasoning != tt.reasoning || !slices.Equal(got.Categories, tt.categories) {
				t.Fatalf("inspection = %+v, %v", got, err)
			}
			if !bytes.Equal(raw, before) {
				t.Fatal("inspection mutated request")
			}
		})
	}
}

func TestInspectOutputBudget(t *testing.T) {
	tests := []struct {
		name, protocol, field string
		want                  int64
		bad                   bool
	}{
		{"absent", "openai", "", 0, false},
		{"anthropic", "anthropic", `"max_tokens":123,`, 123, false},
		{"bedrock", "bedrock", `"max_tokens":456,`, 456, false},
		{"bedrock inference config", "bedrock", `"inferenceConfig":{"maxTokens":789},`, 789, false},
		{"openai completion", "openai", `"max_completion_tokens":321,`, 321, false},
		{"largest budget", "openai", `"max_tokens":900,"max_completion_tokens":200,`, 900, false},
		{"exact beyond float", "openai", `"max_tokens":9007199254740993,`, 9007199254740993, false},
		{"int64 max", "openai", `"max_tokens":9223372036854775807,`, math.MaxInt64, false},
		{"overflow", "openai", `"max_tokens":9223372036854775808,`, 0, true},
		{"negative", "openai", `"max_tokens":-1,`, 0, true},
		{"fraction", "openai", `"max_tokens":1.5,`, 0, true},
		{"huge exponent", "openai", `"max_tokens":1e999999999,`, 0, true},
		{"string", "openai", `"max_tokens":"alice@example.test",`, 0, true},
		{"null", "openai", `"max_tokens":null,`, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NewInspector().Inspect(context.Background(), tt.protocol, []byte(`{`+tt.field+`"messages":[{"role":"user","content":"hi"}]}`))
			if (err != nil) != tt.bad || got.OutputTokens != tt.want {
				t.Fatalf("inspection = %+v, %v", got, err)
			}
			if err != nil && (strings.Contains(err.Error(), "alice") || got.Complete) {
				t.Fatalf("unsafe error/result: %+v, %v", got, err)
			}
		})
	}
}

func TestInspectInputEstimate(t *testing.T) {
	plain := []byte(`{"messages":[{"role":"user","content":"한글abcdef"}]}`)
	escaped := []byte(`{ "messages": [ { "role": "user", "content": "\ud55c\uae00\u0061bcdef" } ] }`)
	a, err := NewInspector().Inspect(context.Background(), "openai", plain)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewInspector().Inspect(context.Background(), "openai", escaped)
	if err != nil || a.InputTokens != b.InputTokens || a.InputTokens < int64(len("한글abcdef")) {
		t.Fatalf("estimates = %d, %d, %v", a.InputTokens, b.InputTokens, err)
	}
	long, err := NewInspector().Inspect(context.Background(), "openai", []byte(`{"metadata":{"context":"`+strings.Repeat("x", 5000)+`"},"messages":[{"role":"user","content":"한글abcdef"}]}`))
	if err != nil || long.InputTokens < a.InputTokens+5000 {
		t.Fatalf("metadata omitted from estimate: %+v, %v", long, err)
	}
}

func TestInspectErrorsAreSanitized(t *testing.T) {
	for _, raw := range []string{``, `{"messages":`, `{"alice@example.test": invalid}`, `{"messages":[]} {"secret":"alice@example.test"}`, "{\"secret\":\"\xff\"}"} {
		t.Run(fmt.Sprintf("%q", raw), func(t *testing.T) {
			got, err := NewInspector().Inspect(context.Background(), "openai", []byte(raw))
			if err == nil || !reflect.DeepEqual(got, Result{}) || strings.Contains(err.Error(), "alice") || strings.Contains(err.Error(), "invalid}") {
				t.Fatalf("inspection = %+v, %v", got, err)
			}
			if errors.Unwrap(err) != nil {
				t.Fatalf("error wraps raw decoder details: %v", err)
			}
		})
	}
	got, err := NewInspector().Inspect(context.Background(), "alice@example.test", []byte(`{"messages":[]}`))
	if err == nil || got.Complete || strings.Contains(err.Error(), "alice") {
		t.Fatalf("unknown protocol: %+v, %v", got, err)
	}
}

func TestInspectDuplicateKeys(t *testing.T) {
	for _, raw := range []string{
		`{"messages":[{"role":"user","content":"alice@example.test","content":"safe"}]}`,
		`{"messages":[{"role":"user","content":"alice@example.test"}],"messages":[{"role":"user","content":"safe"}]}`,
		`{"messages":[{"role":"user","content":"alice@example.test","con\u0074ent":"safe"}]}`,
		`{"messages":[{"role":"assistant","tool_calls":[{"type":"function","function":{"name":"f","arguments":"{\"contact\":\"alice@example.test\",\"contact\":\"safe\"}"}}]}]}`,
	} {
		before := []byte(raw)
		got, err := NewInspector().Inspect(context.Background(), "openai", before)
		if err == nil || !reflect.DeepEqual(got, Result{}) || strings.Contains(err.Error(), "alice") ||
			strings.Contains(err.Error(), "contact") || string(before) != raw {
			t.Fatalf("duplicate keys: %+v, %v", got, err)
		}
		if MatchesKeywords(before, []string{"safe", "alice"}) {
			t.Fatal("ambiguous duplicate keys produced a keyword match")
		}
	}
}

func TestInspectMalformedToolArguments(t *testing.T) {
	raw := []byte(`{"messages":[{"role":"assistant","content":null,"tool_calls":[{"type":"function","function":{"name":"f","arguments":"{\"owner\":\"alice\\u0040example.test"}}]}]}`)
	got, err := NewInspector().Inspect(context.Background(), "openai", raw)
	if err == nil || !reflect.DeepEqual(got, Result{}) || strings.Contains(err.Error(), "alice") {
		t.Fatalf("malformed tool arguments: %+v, %v", got, err)
	}
	if MatchesKeywords(raw, []string{"alice"}) {
		t.Fatal("malformed tool arguments produced a partial keyword match")
	}
}

func TestInspectCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := NewInspector().Inspect(ctx, "openai", []byte(`{"messages":[{"role":"user","content":"hello"}]}`))
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(got, Result{}) {
		t.Fatalf("cancelled inspection = %+v, %v", got, err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	during := &cancelDuringTraversal{Context: ctx, cancel: cancel}
	raw := []byte(`{"messages":[{"role":"user","content":"` + strings.Repeat("safe ", 10000) + `"}]}`)
	got, err = NewInspector().Inspect(during, "openai", raw)
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(got, Result{}) {
		t.Fatalf("cancellation during traversal = %+v, %v", got, err)
	}
}

// Deterministic cancellation after work starts, with the real context error.
type cancelDuringTraversal struct {
	context.Context
	cancel context.CancelFunc
	checks int
}

func (c *cancelDuringTraversal) Err() error {
	c.checks++
	if c.checks == 20 {
		c.cancel()
	}
	return c.Context.Err()
}

func TestInspectorReusableConcurrently(t *testing.T) {
	inspector := NewInspector()
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			raw := []byte(`{"messages":[{"role":"user","content":"alice@example.test"}]}`)
			got, err := inspector.Inspect(context.Background(), "openai", raw)
			if err != nil || !slices.Equal(got.Categories, []string{"email"}) {
				t.Errorf("inspection = %+v, %v", got, err)
				return
			}
			got.Categories[0] = "caller-owned"
		})
	}
	wg.Wait()
	got, err := inspector.Inspect(context.Background(), "openai", []byte(`{"messages":[{"role":"user","content":"safe"}]}`))
	if err != nil || len(got.Categories) != 0 {
		t.Fatalf("cross-request retention = %+v, %v", got, err)
	}
}
