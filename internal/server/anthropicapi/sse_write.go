package anthropicapi

import (
	"encoding/json"
	"io"

	"github.com/inferplane/inferplane/pkg/schema"
)

// WriteSSEEvent serializes a canonical ChatChunk into one Anthropic SSE event
// ("event: <type>\ndata: <json>\n\n"). NOT used on the M2 same-protocol path
// (which tees original upstream bytes); it exists for M5 cross-protocol
// re-serialization (OpenAI ingress → Anthropic provider). Golden-validated now
// so M5 builds on a verified serializer.
func WriteSSEEvent(w io.Writer, chunk *schema.ChatChunk) error {
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
