package responses

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/inferplane/inferplane/pkg/schema"
)

func TestRequestToolsHistoryAndPhase(t *testing.T) {
	raw := []byte(`{"model":"auto","store":false,"instructions":"Be careful.","input":[
{"role":"developer","content":"Use tools."},
{"role":"user","content":[{"type":"input_text","text":"Fix it"}]},
{"type":"message","role":"assistant","phase":"commentary","content":[{"type":"output_text","text":"Inspecting"}]},
{"type":"function_call","call_id":"call_read","name":"read","arguments":"{\"path\":\"a.go\"}"},
{"type":"function_call_output","call_id":"call_read","output":"package main"},
{"type":"custom_tool_call","call_id":"call_patch","name":"apply_patch","input":"*** Begin Patch\n*** End Patch"},
{"type":"custom_tool_call_output","call_id":"call_patch","output":[{"type":"input_text","text":"Done"}]}],
"tools":[{"type":"function","name":"read","parameters":{"type":"object","properties":{"path":{"type":"string"}}}},
{"type":"custom","name":"apply_patch","description":"Patch files","format":{"type":"grammar","syntax":"lark","definition":"start: /.+/"}}],
"tool_choice":"auto","max_output_tokens":200,"stream":true}`)
	if err := ValidateConversion(raw); err != nil {
		t.Fatal(err)
	}
	got, err := RequestToCanonical(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.Model != "auto" || got.MaxTokens == nil || *got.MaxTokens != 200 || len(got.Messages) != 6 {
		t.Fatalf("request: %+v", got)
	}
	if !strings.Contains(string(got.System), "Be careful.") || !strings.Contains(string(got.System), "Use tools.") {
		t.Fatalf("instructions lost: %s", got.System)
	}
	if string(got.Messages[1].Extra["phase"]) != `"commentary"` {
		t.Fatalf("phase lost: %+v", got.Messages[1])
	}
	call := got.Messages[2].Content[0]
	if call.ID != "call_read" || call.Name != "read" || string(call.Input) != `{"path":"a.go"}` {
		t.Fatalf("function call: %+v", call)
	}
	patch := got.Messages[4].Content[0]
	if patch.ID != "call_patch" || !strings.Contains(string(patch.Input), `"input"`) {
		t.Fatalf("custom call: %+v", patch)
	}
	if got.Messages[5].Content[0].ToolUseID != "call_patch" || !strings.Contains(string(got.Tools), `"input_schema"`) {
		t.Fatal("tool definitions/results lost")
	}
}

func TestConversionRejectsUnsupportedButNativeObservationAllows(t *testing.T) {
	for _, extra := range []string{
		`"previous_response_id":"resp_old"`, `"conversation":"conv_old"`,
		`"background":true`, `"store":true`, `"tools":[{"type":"web_search"}]`,
		`"input":[{"type":"reasoning","encrypted_content":"opaque"}]`,
		`"input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,AA"}]}]`,
		`"input":[{"type":"item_reference","id":"old"}]`,
		`"text":{"format":{"type":"json_schema","name":"answer","schema":{"type":"object"}}}`,
		`"truncation":"auto"`, `"prompt":{"id":"pmpt_remote"}`,
	} {
		t.Run(extra, func(t *testing.T) {
			raw := []byte(`{"model":"m",` + extra + `}`)
			if err := ValidateConversion(raw); err == nil {
				t.Fatal("unsupported conversion accepted")
			}
			if _, err := RequestToCanonical(raw); err != nil {
				t.Fatalf("native observation should allow valid opaque input: %v", err)
			}
		})
	}
}

func TestRequestRejectsMalformedAndAmbiguous(t *testing.T) {
	for _, raw := range []string{
		`null`, `[]`, `{"model":"m"} trailing`, `{"model":"a","model":"b"}`,
		`{"Model":"a","model":"b"}`, `{"model":3}`, `{"model":"m","input":123}`,
		`{"model":"m","input":[{"type":"function_call","name":"f","call_id":"c","arguments":"not-json"}]}`,
	} {
		if _, err := RequestToCanonical([]byte(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func TestResponseUsageAndRoundTrip(t *testing.T) {
	raw := []byte(`{"id":"resp_a","object":"response","model":"m","status":"completed","output":[
{"id":"msg_a","type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"Done","annotations":[]}]},
{"type":"function_call","id":"fc_a","call_id":"call_a","name":"read","arguments":"{\"path\":\"a.go\"}"}],
"usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":70},"output_tokens":9,"output_tokens_details":{"reasoning_tokens":4},"total_tokens":109}}`)
	cr, err := ResponseToCanonical(raw)
	if err != nil {
		t.Fatal(err)
	}
	if cr.Usage == nil || *cr.Usage.InputTokens != 30 || *cr.Usage.CacheReadInputTokens != 70 || *cr.Usage.OutputTokens != 9 {
		t.Fatalf("cache usage: %+v", cr.Usage)
	}
	if len(cr.Content) != 2 || cr.Content[1].ID != "call_a" || string(cr.Content[0].Extra["phase"]) != `"final_answer"` {
		t.Fatalf("output: %+v", cr)
	}
	out, err := CanonicalToResponse(cr)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got["usage"]), `"input_tokens":100`) || !strings.Contains(string(got["output"]), `"call_id":"call_a"`) {
		t.Fatalf("roundtrip: %s", out)
	}
}

func TestResponseRejectsMissingInvalidUsageAndFailure(t *testing.T) {
	for _, tail := range []string{
		``, `,"usage":null`, `,"usage":{}`, `,"usage":{"input_tokens":1}`,
		`,"usage":{"input_tokens":-1,"output_tokens":2}`,
		`,"usage":{"input_tokens":1,"output_tokens":2,"input_tokens_details":{"cached_tokens":3}}`,
		`,"usage":{"input_tokens":1.5,"output_tokens":2}`,
	} {
		if _, err := ResponseToCanonical([]byte(`{"id":"r","status":"completed","output":[]` + tail + `}`)); err == nil {
			t.Fatalf("accepted missing/invalid usage: %s", tail)
		}
	}
	for _, status := range []string{"failed", "in_progress", "queued", "cancelled"} {
		_, err := ResponseToCanonical([]byte(`{"status":"` + status + `","error":{"message":"SECRET"},"output":[],"usage":{"input_tokens":1,"output_tokens":1}}`))
		if err == nil || strings.Contains(err.Error(), "SECRET") {
			t.Fatalf("status %s: %v", status, err)
		}
	}
	cr, err := ResponseToCanonical([]byte(`{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[],"usage":{"input_tokens":0,"output_tokens":0}}`))
	if err != nil || cr.StopReason == nil || *cr.StopReason != "max_tokens" {
		t.Fatalf("incomplete with real zero usage: %+v %v", cr, err)
	}
}

func TestCustomToolResponseRestoration(t *testing.T) {
	req, err := RequestToCanonical([]byte(`{"model":"m","input":"Patch","tools":[{"type":"custom","name":"apply_patch"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	n := int64(1)
	resp := &schema.ChatResponse{ID: "r", Model: "m", Content: []schema.ContentBlock{
		{Type: "tool_use", ID: "c", Name: "apply_patch", Input: json.RawMessage(`{"input":"*** Begin Patch\n*** End Patch"}`)},
	}, Usage: &schema.Usage{InputTokens: &n, OutputTokens: &n}}
	out, err := CanonicalToResponseForRequest(resp, req)
	if err != nil || !strings.Contains(string(out), `"type":"custom_tool_call"`) || !strings.Contains(string(out), `"input":"*** Begin Patch\n*** End Patch"`) {
		t.Fatalf("custom output: %s %v", out, err)
	}
	resp.Content[0].Input = json.RawMessage(`{"wrong":"shape"}`)
	if _, err := CanonicalToResponseForRequest(resp, req); err == nil {
		t.Fatal("invalid custom arguments silently accepted")
	}
}

func TestConversionRejectsToolExecutionSemantics(t *testing.T) {
	for _, extra := range []string{
		`"parallel_tool_calls":false`,
		`"input":[{"type":"function_call","name":"f","call_id":"c","arguments":"{}","namespace":"remote"}]`,
		`"input":[{"type":"custom_tool_call","name":"f","call_id":"c","input":"x","async":true}]`,
		`"tools":[{"type":"function","name":"f","parameters":null}]`,
	} {
		raw := []byte(`{"model":"m",` + extra + `}`)
		if _, err := RequestToCanonical(raw); err != nil {
			t.Fatalf("native observation rejected valid native surface: %v", err)
		}
		if err := ValidateConversion(raw); err == nil {
			t.Fatalf("silently dropped execution semantics: %s", raw)
		}
	}
}

func TestCacheWritesAndZeroUsageAreNotDoubleCounted(t *testing.T) {
	resp, err := ResponseToCanonical([]byte(`{"status":"completed","output":[],"usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":50,"cache_write_tokens":30},"output_tokens":4}}`))
	if err != nil || *resp.Usage.InputTokens != 20 || *resp.Usage.CacheReadInputTokens != 50 || *resp.Usage.CacheCreationInputTokens != 30 {
		t.Fatalf("usage: %+v %v", resp, err)
	}
	raw, err := CanonicalToResponse(resp)
	if err != nil || !strings.Contains(string(raw), `"input_tokens":100`) || !strings.Contains(string(raw), `"total_tokens":104`) {
		t.Fatalf("write roundtrip: %s %v", raw, err)
	}
	if _, err := ResponseToCanonical([]byte(`{"status":"completed","output":[],"usage":{"input_tokens":0,"output_tokens":0}}`)); err != nil {
		t.Fatal(err)
	}
}
