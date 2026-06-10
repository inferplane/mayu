package anthropic

import (
	"strings"
	"testing"
)

func TestReadSSEEventsByteExact(t *testing.T) {
	body := "event: message_start\ndata: {\"type\":\"message_start\"}\n\n" +
		"event: ping\ndata: {\"type\":\"ping\"}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":7}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	var rawConcat strings.Builder
	var types []string
	var sawUsage bool
	for ev, err := range readSSE(strings.NewReader(body)) {
		if err != nil {
			t.Fatal(err)
		}
		rawConcat.Write(ev.Raw)
		if ev.Chunk != nil {
			types = append(types, ev.Chunk.Type)
			if ev.Chunk.Usage != nil {
				sawUsage = true
			}
		}
	}
	if rawConcat.String() != body {
		t.Fatalf("raw passthrough not byte-exact:\n got: %q\nwant: %q", rawConcat.String(), body)
	}
	want := []string{"message_start", "ping", "message_delta", "message_stop"}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("event types: %v", types)
	}
	if !sawUsage {
		t.Fatal("expected usage on message_delta")
	}
}
