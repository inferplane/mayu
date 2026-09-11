package sensitivity

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestRequestRedactionCoversAllDetectedOccurrences(t *testing.T) {
	cases := map[string]string{
		"anthropic": `{"model":"auto","system":"Reach a@example.test or b@example.test","messages":[{"role":"user","content":[{"type":"text","text":"900101-1234567 and 4111 1111 1111 1111; call 010-1234-5678"}]}]}`,
		"bedrock":   `{"anthropic_version":"bedrock-2023-05-31","messages":[{"role":"user","content":"a@example.test; 900101-1234567"}],"max_tokens":64}`,
		"openai":    `{"model":"auto","messages":[{"role":"system","content":"a@example.test"},{"role":"assistant","content":null,"tool_calls":[{"id":"call_a","type":"function","function":{"name":"lookup","arguments":"{\"nested\":\"{\\\"email\\\":\\\"b@example.test\\\"}\"}"}}]},{"role":"tool","tool_call_id":"call_a","content":"c@example.test; 123-45-6789; 192.168.1.2"}]}`,
		"responses": `{"model":"auto","instructions":"a@example.test","input":[{"role":"user","content":"900101-1234567"},{"type":"custom_tool_call","name":"apply_patch","call_id":"call_patch","input":"*** Begin Patch\n+ b@example.test\n*** End Patch"},{"type":"custom_tool_call_output","call_id":"call_patch","output":"c@example.test"}]}`,
	}
	for protocol, body := range cases {
		t.Run(protocol, func(t *testing.T) {
			raw := []byte(body)
			before := bytes.Clone(raw)
			masked, err := NewRedactor().Redact(context.Background(), protocol, raw)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(raw, before) || bytes.Equal(masked, raw) || !json.Valid(masked) {
				t.Fatal("must preserve original and return changed valid JSON")
			}
			got, err := NewInspector().Inspect(context.Background(), protocol, masked)
			if err != nil || !got.Complete || len(got.Categories) != 0 {
				t.Fatalf("redaction left unsafe content: %+v, %v", got, err)
			}
			if strings.Contains(string(masked), "example.test") || strings.Contains(string(masked), "900101") {
				t.Fatal("original protected value survived")
			}
			again, err := NewRedactor().Redact(context.Background(), protocol, raw)
			if err != nil || !bytes.Equal(masked, again) {
				t.Fatal("identical prompts must produce identical masked prefixes")
			}
			idempotent, err := NewRedactor().Redact(context.Background(), protocol, masked)
			if err != nil || !bytes.Equal(masked, idempotent) {
				t.Fatal("already-sanitized body must remain byte-identical")
			}
		})
	}
}

func TestRequestRedactionRefusesUnsafeStructuralAndOpaqueContent(t *testing.T) {
	for name, body := range map[string]string{
		"model":       `{"model":"a@example.test","messages":[{"role":"user","content":"hi"}]}`,
		"key":         `{"model":"auto","messages":[{"role":"user","content":[{"type":"tool_use","id":"t","name":"f","input":{"a@example.test":"hello"}}]}]}`,
		"numeric":     `{"model":"auto","messages":[{"role":"user","content":[{"type":"tool_use","id":"t","name":"f","input":{"card":4111111111111111}}]}]}`,
		"schema name": `{"model":"auto","tools":[{"name":"a@example.test","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"hello"}]}`,
		"opaque":      `{"model":"auto","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","data":"opaque","media_type":"image/png"}}]}]}`,
		"duplicate":   `{"model":"auto","messages":[{"role":"user","content":"a@example.test","content":"clean"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			masked, err := NewRedactor().Redact(context.Background(), "anthropic", []byte(body))
			if err == nil || len(masked) != 0 {
				t.Fatalf("unsafe transform was allowed: %q, %v", masked, err)
			}
			if strings.Contains(err.Error(), "example.test") || strings.Contains(err.Error(), "411111") {
				t.Fatal("redactor error exposed protected data")
			}
		})
	}
}

func TestRequestRedactionPreservesUnchangedBytesAndCancellation(t *testing.T) {
	raw := []byte("{ \"model\":\"auto\", \"messages\":[{\"role\":\"user\",\"content\":\"hello\"}] }")
	got, err := NewRedactor().Redact(context.Background(), "openai", raw)
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("unmodified request changed: %s, %v", got, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := NewRedactor().Redact(ctx, "openai", raw); err == nil || len(got) > 0 {
		t.Fatal("cancelled redaction must not produce a usable body")
	}
}

func TestRedactionDoesNotRewriteNestedSchemaConstraints(t *testing.T) {
	for _, keyword := range []string{"const", "enum", "default"} {
		value := `{"content":"alice@example.test"}`
		if keyword == "enum" {
			value = `[` + value + `]`
		}
		raw := []byte(`{"model":"auto","input":"hello","tools":[{"type":"function","name":"f","parameters":{"type":"object","properties":{"x":{"` + keyword + `":` + value + `}}}}]}`)
		if body, err := NewRedactor().Redact(context.Background(), "responses", raw); err == nil || len(body) != 0 {
			t.Fatalf("rewrote %s validation constraint: %s %v", keyword, body, err)
		}
	}
	for protocol, raw := range map[string]string{
		"responses": `{"model":"auto","input":"hi","text":{"format":{"type":"json_schema","name":"r","schema":{"const":{"content":"alice@example.test"}}}}}`,
		"openai":    `{"model":"auto","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_schema","json_schema":{"name":"r","schema":{"const":{"content":"alice@example.test"}}}}}`,
	} {
		if body, err := NewRedactor().Redact(context.Background(), protocol, []byte(raw)); err == nil || len(body) != 0 {
			t.Fatalf("%s output schema constraint was rewritten: %s %v", protocol, body, err)
		}
	}
}
