package anthropic

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"iter"
	"strings"

	"github.com/inferplane/inferplane/pkg/schema"
	"github.com/inferplane/inferplane/providers"
)

// readSSE parses an Anthropic SSE response into a sequence of StreamEvents.
// Raw is the exact bytes of each event block (all lines up to and including
// the blank-line terminator) so the ingress can tee them to the client
// verbatim; Chunk is the parsed "data:" JSON (nil if the block has no data
// line). Byte-exactness is the tee guarantee.
func readSSE(r io.Reader) iter.Seq2[*providers.StreamEvent, error] {
	return func(yield func(*providers.StreamEvent, error) bool) {
		br := bufio.NewReader(r)
		var block bytes.Buffer
		var dataLine []byte
		flush := func() bool {
			if block.Len() == 0 {
				return true
			}
			ev := &providers.StreamEvent{Raw: append([]byte(nil), block.Bytes()...)}
			if dataLine != nil {
				var c schema.ChatChunk
				if json.Unmarshal(dataLine, &c) == nil {
					ev.Chunk = &c
				}
			}
			block.Reset()
			dataLine = nil
			return yield(ev, nil)
		}
		for {
			line, err := br.ReadBytes('\n')
			if len(line) > 0 {
				block.Write(line)
				trimmed := strings.TrimRight(string(line), "\r\n")
				if trimmed == "" {
					if !flush() {
						return
					}
				} else if strings.HasPrefix(trimmed, "data:") {
					dataLine = []byte(strings.TrimSpace(strings.TrimPrefix(trimmed, "data:")))
				}
			}
			if err == io.EOF {
				flush()
				return
			}
			if err != nil {
				yield(nil, err)
				return
			}
		}
	}
}
