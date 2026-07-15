package bedrockapi

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	"github.com/inferplane/inferplane/pkg/schema"
)

func headerStr(t *testing.T, msg eventstream.Message, name string) string {
	t.Helper()
	for _, h := range msg.Headers {
		if h.Name == name {
			if sv, ok := h.Value.(eventstream.StringValue); ok {
				return string(sv)
			}
			t.Fatalf("header %q is not a string value: %#v", name, h.Value)
		}
	}
	t.Fatalf("header %q missing: %#v", name, msg.Headers)
	return ""
}

func decodeAll(t *testing.T, r io.Reader) []eventstream.Message {
	t.Helper()
	dec := eventstream.NewDecoder()
	var out []eventstream.Message
	for {
		m, err := dec.Decode(r, nil)
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatalf("decode (frame %d): %v", len(out)+1, err)
		}
		out = append(out, m.Clone())
	}
}

// The correctness oracle for our encoder is the AWS SDK's OWN decoder: a
// frame the SDK cannot decode would break every real Bedrock-mode client.
func TestChunkFrameRoundTrip(t *testing.T) {
	in, out := int64(3), int64(9)
	chunks := []*schema.ChatChunk{
		{Type: "message_start"},
		{Type: "content_block_delta"},
		{Type: "message_delta", Usage: &schema.Usage{InputTokens: &in, OutputTokens: &out}},
		{Type: "message_stop"},
	}
	for _, c := range chunks {
		chunkJSON, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		enc := eventstream.NewEncoder()
		if err := writeChunkFrame(&buf, enc, chunkJSON); err != nil {
			t.Fatalf("%s: %v", c.Type, err)
		}
		msgs := decodeAll(t, &buf)
		if len(msgs) != 1 {
			t.Fatalf("%s: %d frames, want 1", c.Type, len(msgs))
		}
		m := msgs[0]
		if got := headerStr(t, m, ":message-type"); got != "event" {
			t.Fatalf("%s: :message-type = %q", c.Type, got)
		}
		if got := headerStr(t, m, ":event-type"); got != "chunk" {
			t.Fatalf("%s: :event-type = %q", c.Type, got)
		}
		if got := headerStr(t, m, ":content-type"); got != "application/json" {
			t.Fatalf("%s: :content-type = %q", c.Type, got)
		}
		var payload struct {
			Bytes string `json:"bytes"`
		}
		if err := json.Unmarshal(m.Payload, &payload); err != nil || payload.Bytes == "" {
			t.Fatalf("%s: payload not {\"bytes\": b64}: %s", c.Type, m.Payload)
		}
		decoded, err := base64.StdEncoding.DecodeString(payload.Bytes)
		if err != nil {
			t.Fatalf("%s: bytes not base64: %v", c.Type, err)
		}
		if string(decoded) != string(chunkJSON) {
			t.Fatalf("%s: payload bytes are not the bare event JSON:\n got: %s\nwant: %s", c.Type, decoded, chunkJSON)
		}
		if strings.Contains(string(decoded), "event:") || strings.Contains(string(decoded), "data:") {
			t.Fatalf("%s: SSE framing text leaked into the frame payload: %s", c.Type, decoded)
		}
	}
}

func TestExceptionFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	enc := eventstream.NewEncoder()
	if err := writeExceptionFrame(&buf, enc, "throttlingException", "please slow down"); err != nil {
		t.Fatal(err)
	}
	msgs := decodeAll(t, &buf)
	if len(msgs) != 1 {
		t.Fatalf("%d frames, want 1", len(msgs))
	}
	m := msgs[0]
	if got := headerStr(t, m, ":message-type"); got != "exception" {
		t.Fatalf(":message-type = %q", got)
	}
	if got := headerStr(t, m, ":exception-type"); got != "throttlingException" {
		t.Fatalf(":exception-type = %q", got)
	}
	var payload struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(m.Payload, &payload); err != nil || payload.Message != "please slow down" {
		t.Fatalf("payload not {\"message\":...}: %s", m.Payload)
	}
}
