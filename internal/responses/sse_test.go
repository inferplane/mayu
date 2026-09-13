package responses

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/inferplane/inferplane/pkg/schema"
)

func TestStreamLifecycleTextToolsAndUsage(t *testing.T) {
	req, _ := RequestToCanonical([]byte(`{"model":"m","input":"x","tools":[{"type":"custom","name":"apply_patch"}]}`))
	s := NewStreamState("m", req)
	idx := 0
	i, o, c := int64(2), int64(4), int64(8)
	chunks := []*schema.ChatChunk{
		{Type: "message_start", Message: &schema.ChatResponse{ID: "r", Usage: &schema.Usage{InputTokens: &i, CacheReadInputTokens: &c}}},
		{Type: "content_block_start", Index: &idx, ContentBlock: &schema.ContentBlock{Type: "tool_use", ID: "call_a", Name: "apply_patch"}},
		{Type: "content_block_delta", Index: &idx, Delta: json.RawMessage(`{"type":"input_json_delta","partial_json":"{\"input\":\"patch"}`)},
		{Type: "content_block_delta", Index: &idx, Delta: json.RawMessage(`{"type":"input_json_delta","partial_json":" text\"}"}`)},
		{Type: "content_block_stop", Index: &idx},
		{Type: "message_delta", Usage: &schema.Usage{OutputTokens: &o}, Delta: json.RawMessage(`{"stop_reason":"tool_use"}`)},
		{Type: "message_stop"},
	}
	var raw bytes.Buffer
	var types []string
	lastSeq := -1
	for _, chunk := range chunks {
		events, err := s.Convert(chunk)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range events {
			types = append(types, e.Type)
			var payload struct {
				Sequence int `json:"sequence_number"`
			}
			if err := json.Unmarshal(e.Data, &payload); err != nil || payload.Sequence <= lastSeq {
				t.Fatalf("event sequence: %s %v", e.Data, err)
			}
			lastSeq = payload.Sequence
			if err := WriteEvent(&raw, e); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := s.Finish(); err != nil {
		t.Fatal(err)
	}
	if types[0] != "response.created" || types[len(types)-1] != "response.completed" {
		t.Fatalf("lifecycle: %v", types)
	}
	if !strings.Contains(raw.String(), `"input":"patch text"`) || !strings.Contains(raw.String(), `"input_tokens":10`) || !strings.Contains(raw.String(), `"cached_tokens":8`) {
		t.Fatalf("stream lost tool or usage: %s", raw.String())
	}
}

func TestStreamMissingUsageTruncationAndError(t *testing.T) {
	s := NewStreamState("m")
	if _, err := s.Convert(&schema.ChatChunk{Type: "message_start", Message: &schema.ChatResponse{ID: "r"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Finish(); err == nil {
		t.Fatal("truncation reported as success")
	}
	if _, err := s.Convert(&schema.ChatChunk{Type: "message_stop"}); err == nil {
		t.Fatal("completion without usage accepted")
	}
	s = NewStreamState("m")
	ev, err := s.Convert(&schema.ChatChunk{Type: "error", Extra: map[string]json.RawMessage{"error": json.RawMessage(`{"message":"SECRET"}`)}})
	if err != nil || len(ev) == 0 || ev[len(ev)-1].Type != "response.failed" || bytes.Contains(ev[len(ev)-1].Data, []byte("SECRET")) {
		t.Fatalf("safe failure event: %+v %v", ev, err)
	}
}

func TestReadSSERawAndUsage(t *testing.T) {
	raw := ": keepalive\r\n\r\nevent: response.created\r\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r\",\"model\":\"u\",\"status\":\"in_progress\",\"output\":[]}}\r\n\r\n" +
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"delta\":\"hi\"}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":10,\"input_tokens_details\":{\"cached_tokens\":8},\"output_tokens\":2}}}\n\n"
	var tee bytes.Buffer
	var usage *schema.Usage
	for event, err := range ReadSSE(strings.NewReader(raw), "public") {
		if err != nil {
			t.Fatal(err)
		}
		tee.Write(event.Raw)
		if event.Chunk != nil {
			usage = schema.MergeUsage(usage, event.Chunk.Usage)
			if event.Chunk.Message != nil && event.Chunk.Message.Model != "public" {
				t.Fatal("observation leaked upstream model")
			}
		}
	}
	if tee.String() != raw || usage == nil || *usage.InputTokens != 2 || *usage.CacheReadInputTokens != 8 {
		t.Fatalf("raw/usage mismatch: %q %+v", tee.String(), usage)
	}
}

func TestReadSSERejectsTruncatedMalformedMissingUsage(t *testing.T) {
	for _, raw := range []string{
		"", "data: [DONE]\n\n", "data: not-json\n\n",
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n",
		"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[]}}\n\n",
		"data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"message\":\"SECRET\"}}}\n\n",
	} {
		var failed bool
		for ev, err := range ReadSSE(strings.NewReader(raw), "m") {
			if err != nil {
				failed = true
				if strings.Contains(err.Error(), "SECRET") {
					t.Fatal("secret in observation error")
				}
			}
			if ev != nil && ev.Chunk != nil && ev.Chunk.Type == "message_stop" && !failed {
				t.Fatal("false successful terminal")
			}
		}
		if !failed {
			t.Fatalf("accepted bad stream: %q", raw)
		}
	}
}

func TestNativeCustomToolEvents(t *testing.T) {
	start, err := ParseEvent([]byte(`{"type":"response.output_item.added","output_index":2,"item":{"type":"custom_tool_call","name":"apply_patch","call_id":"c","input":""}}`))
	if err != nil || start == nil || start.ContentBlock == nil || start.ContentBlock.ID != "c" || start.ContentBlock.Name != "apply_patch" {
		t.Fatalf("custom start: %+v %v", start, err)
	}
	done, err := ParseEvent([]byte(`{"type":"response.custom_tool_call_input.done","output_index":2,"input":"patch\ntext"}`))
	if err != nil || done == nil || !strings.Contains(string(done.Delta), `"partial_json"`) {
		t.Fatalf("custom arguments observation: %+v %v", done, err)
	}
	var d struct {
		Partial string `json:"partial_json"`
	}
	if err := json.Unmarshal(done.Delta, &d); err != nil || d.Partial != `{"input":"patch\ntext"}` {
		t.Fatalf("custom input roundtrip: %q %v", d.Partial, err)
	}
}

func TestNativeTerminalTypeMustMatchStatus(t *testing.T) {
	for _, raw := range []string{
		`{"type":"response.incomplete","response":{"status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`,
		`{"type":"response.completed","response":{"status":"incomplete","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`,
	} {
		if _, err := ParseEvent([]byte(raw)); err == nil {
			t.Fatalf("mismatched terminal accepted: %s", raw)
		}
	}
}

func TestStreamTextAndParallelFunctions(t *testing.T) {
	s := NewStreamState("m")
	i, o := int64(2), int64(3)
	var events []Event
	emit := func(c *schema.ChatChunk) {
		t.Helper()
		out, err := s.Convert(c)
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, out...)
	}
	emit(&schema.ChatChunk{Type: "message_start", Message: &schema.ChatResponse{Usage: &schema.Usage{InputTokens: &i}}})
	for index, name := range []string{"read", "list"} {
		idx := index
		emit(&schema.ChatChunk{Type: "content_block_start", Index: &idx, ContentBlock: &schema.ContentBlock{Type: "tool_use", Name: name, ID: name + "_call"}})
		emit(&schema.ChatChunk{Type: "content_block_delta", Index: &idx, Delta: json.RawMessage(`{"type":"input_json_delta","partial_json":"{}"}`)})
	}
	index := 2
	emit(&schema.ChatChunk{Type: "content_block_start", Index: &index, ContentBlock: &schema.ContentBlock{Type: "text", Text: ptr(""), Extra: map[string]json.RawMessage{"phase": json.RawMessage(`"commentary"`)}}})
	emit(&schema.ChatChunk{Type: "content_block_delta", Index: &index, Delta: json.RawMessage(`{"type":"text_delta","text":"Checking"}`)})
	emit(&schema.ChatChunk{Type: "message_delta", Usage: &schema.Usage{OutputTokens: &o}, Delta: json.RawMessage(`{"stop_reason":"max_tokens"}`)})
	emit(&schema.ChatChunk{Type: "message_stop"})
	last := events[len(events)-1]
	if last.Type != "response.incomplete" || !strings.Contains(string(last.Data), `"call_id":"read_call"`) || !strings.Contains(string(last.Data), `"call_id":"list_call"`) || !strings.Contains(string(last.Data), `"phase":"commentary"`) || !strings.Contains(string(last.Data), `"text":"Checking"`) {
		t.Fatalf("final state lost content: %s", last.Data)
	}
}

func TestReadSSEPreservesUsageBeforeTerminalFailure(t *testing.T) {
	raw := "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"message\":\"SECRET\"},\"usage\":{\"input_tokens\":10,\"output_tokens\":2}}}\n\n"
	usageSeen, failed := false, false
	for event, err := range ReadSSE(strings.NewReader(raw), "m") {
		if event != nil {
			usageSeen = event.Chunk != nil && event.Chunk.Usage != nil
			if bytes.Contains(event.Raw, []byte("SECRET")) {
				t.Fatal("error details leaked")
			}
		}
		if err != nil {
			failed = true
			if !usageSeen {
				t.Fatal("failure discarded observed usage")
			}
		}
	}
	if !usageSeen || !failed {
		t.Fatal("failed terminal not observed")
	}
}
