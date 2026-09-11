package openai

import (
	"bufio"
	"encoding/json"
	"io"
	"iter"
	"maps"
	"slices"
	"strings"

	"github.com/inferplane/inferplane/pkg/schema"
	"github.com/inferplane/inferplane/providers"
)

// ReadChatSSE parses an OpenAI Chat Completions SSE stream (sequences of
// `data: {...}` lines terminated by `data: [DONE]`). Each event's Raw is the
// provider-native OpenAI SSE bytes ("data: {...}\n\n") so an OpenAI ingress can
// tee them verbatim; Chunk is the canonical (Anthropic) view parsed via
// ChunkToCanonical for observation and cross-protocol re-serialization
// (a cross-protocol ingress IGNORES Raw and re-renders from Chunk). The [DONE]
// terminator yields Raw="data: [DONE]\n\n" with Chunk=nil. Shared by the
// openaicompat provider (vLLM/Ollama) and the Bedrock Mantle chat-completions
// routes.
//
// model is the PUBLIC model name (req.Model) stamped into the synthesized
// message_start: the OpenAI wire has no message_start/message_stop, but the
// canonical consumers re-render the Anthropic frame vocabulary, whose contract
// opens with message_start and ends with message_stop — a strict client may
// reject or mistrack a stream that starts at a bare delta. Both frames are
// synthesized here (Chunk-only, Raw nil — an OpenAI-wire ingress tees Raw and
// must see no invented lines): message_start lazily before the FIRST parsed
// chunk, so a stream with zero parseable frames still emits nothing and the
// callers' fail-closed no-frames check stays intact; message_stop at [DONE],
// preceded by a synthesized stop-bearing message_delta when the upstream sent
// no message-level frame of its own (no finish_reason, no usage-only chunk).
func ReadChatSSE(r io.Reader, model string) iter.Seq2[*providers.StreamEvent, error] {
	return func(yield func(*providers.StreamEvent, error) bool) {
		br := bufio.NewReader(r)
		// Content blocks open at each index that no frame has closed yet.
		// The OpenAI wire opens a TEXT block implicitly with its first
		// delta and has no close event for anything — but the Anthropic
		// frame vocabulary the canonical consumers re-render requires an
		// explicit content_block_start before any delta at an index (a
		// strict client buffers per opened block and may discard deltas
		// with no opener) and a content_block_stop per opened block before
		// the message-level frames (an Anthropic client finalizes
		// accumulated partial_json there). ChunkToCanonical is stateless
		// per chunk — tool openers come from the wire's id/name fragment,
		// text openers and every close are synthesized here, where the
		// whole stream passes through.
		openBlocks := map[int]bool{}
		started := false         // message_start emitted (implies ≥1 parsed chunk)
		sawMessageDelta := false // any message_delta reached the consumer
		startMessage := func() bool {
			if started {
				return true
			}
			started = true
			msg := &schema.ChatResponse{Type: "message", Role: "assistant", Model: model, Content: []schema.ContentBlock{}}
			return yield(&providers.StreamEvent{Chunk: &schema.ChatChunk{Type: "message_start", Message: msg}}, nil)
		}
		closeOpenBlocks := func() bool {
			for _, idx := range slices.Sorted(maps.Keys(openBlocks)) {
				i := idx
				// Raw stays nil: an OpenAI-wire ingress tees Raw verbatim and
				// its wire has no such frame; only canonical consumers see it.
				if !yield(&providers.StreamEvent{Chunk: &schema.ChatChunk{Type: "content_block_stop", Index: &i}}, nil) {
					return false
				}
			}
			clear(openBlocks)
			return true
		}
		for {
			line, err := br.ReadString('\n')
			trimmed := strings.TrimRight(line, "\r\n")
			if strings.HasPrefix(trimmed, "data:") {
				payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
				if payload == "[DONE]" {
					// A stream that ended without a finish_reason still must
					// not leave blocks open, and a started message must close.
					if !closeOpenBlocks() {
						return
					}
					// An upstream that never sent a finish_reason (nor a
					// usage-only frame) leaves the canonical stream with no
					// message_delta at all — the ONE frame carrying
					// stop_reason on the Anthropic wire, which a client reads
					// to tell a completed turn from a truncated one. Synthesize
					// the terminal delta rather than jumping straight to
					// message_stop; Raw stays nil like the other synthesized
					// frames, so an OpenAI-wire ingress never sees it.
					if started && !sawMessageDelta {
						delta := json.RawMessage(`{"stop_reason":"end_turn"}`)
						if !yield(&providers.StreamEvent{Chunk: &schema.ChatChunk{Type: "message_delta", Delta: delta}}, nil) {
							return
						}
					}
					if started && !yield(&providers.StreamEvent{Chunk: &schema.ChatChunk{Type: "message_stop"}}, nil) {
						return
					}
					if !yield(&providers.StreamEvent{Raw: []byte("data: [DONE]\n\n")}, nil) {
						return
					}
				} else if payload != "" {
					raw := []byte("data: " + payload + "\n\n")
					chunks, cerr := ChunkToCanonical([]byte(payload))
					if cerr != nil || len(chunks) == 0 {
						if !yield(&providers.StreamEvent{Raw: raw}, nil) {
							return
						}
					} else {
						// One SSE line can carry several canonical frames
						// (parallel tool calls). Raw rides the FIRST event
						// only: an OpenAI-wire ingress tees Raw, so repeating
						// it would duplicate the line on the client's stream.
						// The else is structural — it must stay impossible to
						// emit raw AND frames for one line.
						for i, c := range chunks {
							if !startMessage() {
								return
							}
							if c.Type == "content_block_start" && c.Index != nil {
								openBlocks[*c.Index] = true
							}
							// A text_delta at an index no opener claimed:
							// synthesize the text opener the OpenAI wire
							// implies but never sends.
							if c.Type == "content_block_delta" && c.Index != nil && !openBlocks[*c.Index] && isTextDelta(c.Delta) {
								openBlocks[*c.Index] = true
								idx := *c.Index
								empty := ""
								blk := &schema.ContentBlock{Type: "text", Text: &empty}
								if !yield(&providers.StreamEvent{Chunk: &schema.ChatChunk{Type: "content_block_start", Index: &idx, ContentBlock: blk}}, nil) {
									return
								}
							}
							// Close every open tool block BEFORE any
							// message_delta — the stop-bearing one AND the
							// usage-only include_usage frame: Anthropic
							// ordering puts every content_block_stop before
							// message-level frames, and a consumer may treat
							// the first message_delta as end-of-message. The
							// OpenAI wire only emits usage-only chunks at the
							// end of a stream, so there is nothing left to
							// close early.
							if c.Type == "message_delta" {
								sawMessageDelta = true
								if !closeOpenBlocks() {
									return
								}
							}
							ev := &providers.StreamEvent{Chunk: c}
							if i == 0 {
								ev.Raw = raw
							}
							if !yield(ev, nil) {
								return
							}
						}
					}
				}
			}
			if err == io.EOF {
				return
			}
			if err != nil {
				yield(nil, err)
				return
			}
		}
	}
}

// isTextDelta reports whether a content_block_delta payload is a text_delta —
// the one delta kind whose block the OpenAI wire opens implicitly.
func isTextDelta(delta json.RawMessage) bool {
	var d struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(delta, &d) == nil && d.Type == "text_delta"
}
