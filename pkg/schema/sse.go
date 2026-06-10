package schema

import (
	"encoding/json"
	"io"
)

// WriteAnthropicSSE serializes a canonical ChatChunk into one Anthropic SSE
// event ("event: <type>\ndata: <json>\n\n"). Used by providers that receive a
// non-SSE wire format (e.g. Bedrock's event stream) and must re-emit canonical
// Anthropic SSE for the ingress tee, and (M5) by cross-protocol conversion.
func WriteAnthropicSSE(w io.Writer, chunk *ChatChunk) error {
	data, err := json.Marshal(chunk)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(w, "event: "+chunk.Type+"\n"); err != nil {
		return err
	}
	if _, err := w.Write(append([]byte("data: "), data...)); err != nil {
		return err
	}
	_, err = io.WriteString(w, "\n\n")
	return err
}
