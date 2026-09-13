package responses

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"strconv"
	"strings"
	"time"

	"github.com/inferplane/inferplane/pkg/schema"
	"github.com/inferplane/inferplane/providers"
)

type Event struct {
	Type string
	Data json.RawMessage
}

func WriteEvent(w io.Writer, event Event) error {
	if event.Type == "" || strings.ContainsAny(event.Type, "\r\n") || !json.Valid(event.Data) {
		return ErrInvalid
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, event.Data); err != nil {
		return ErrInvalid
	}
	_, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Type, compact.Bytes())
	return err
}

type streamBlock struct {
	block    schema.ContentBlock
	output   int
	argument strings.Builder
	closed   bool
	itemDone bool
}

// StreamState renders a single canonical response, preserving block order and
// cumulative usage. It is request-local and must not be shared across streams.
type StreamState struct {
	model, id         string
	created           int64
	custom            map[string]bool
	origins           map[string]toolOrigin
	blocks            map[int]*streamBlock
	order             []int
	usage             *schema.Usage
	stop              string
	sequence          int
	started, terminal bool
	hasTool           bool
}

func NewStreamState(model string, req ...*schema.ChatRequest) *StreamState {
	var request *schema.ChatRequest
	if len(req) > 0 {
		request = req[0]
	}
	var id [16]byte
	_, _ = rand.Read(id[:])
	return &StreamState{model: model, id: "resp_" + hex.EncodeToString(id[:]), created: time.Now().Unix(), custom: customTools(request), origins: toolOrigins(request), blocks: map[int]*streamBlock{}}
}

func itemID(responseID string, index int) string {
	return responseID + "_item_" + strconv.Itoa(index)
}

func (s *StreamState) event(kind string, fields map[string]any) Event {
	fields["type"] = kind
	fields["sequence_number"] = s.sequence
	s.sequence++
	return Event{Type: kind, Data: marshal(fields)}
}

func (s *StreamState) skeleton(status string) map[string]any {
	return map[string]any{"id": s.id, "object": "response", "created_at": s.created, "model": s.model, "status": status, "output": []any{}, "usage": nil, "error": nil, "incomplete_details": nil}
}

func (s *StreamState) start() []Event {
	if s.started {
		return nil
	}
	s.started = true
	return []Event{
		s.event("response.created", map[string]any{"response": s.skeleton("in_progress")}),
		s.event("response.in_progress", map[string]any{"response": s.skeleton("in_progress")}),
	}
}

func (s *StreamState) Convert(chunk *schema.ChatChunk) ([]Event, error) {
	if chunk == nil || chunk.Type == "ping" {
		return nil, nil
	}
	if s.terminal {
		return nil, ErrInvalid
	}
	if chunk.Type == "message_start" && chunk.Message != nil {
		if s.started {
			return nil, ErrInvalid
		}
		if chunk.Message.ID != "" {
			s.id = chunk.Message.ID
		}
		s.usage = schema.MergeUsage(s.usage, chunk.Message.Usage)
	}
	out := s.start()
	s.usage = schema.MergeUsage(s.usage, chunk.Usage)
	switch chunk.Type {
	case "message_start":
		return out, nil
	case "content_block_start":
		if chunk.Index == nil || *chunk.Index < 0 || len(s.blocks) >= 4096 || chunk.ContentBlock == nil {
			return nil, ErrInvalid
		}
		index := *chunk.Index
		if s.blocks[index] != nil {
			return nil, ErrInvalid
		}
		b := *chunk.ContentBlock
		if b.Type != "text" && b.Type != "tool_use" {
			return nil, ErrUnsupported
		}
		if b.Type == "text" && b.Text == nil {
			b.Text = ptr("")
		}
		sb := &streamBlock{block: b, output: len(s.order)}
		s.blocks[index] = sb
		s.order = append(s.order, index)
		id := itemID(s.id, sb.output)
		var item map[string]any
		if b.Type == "text" {
			phase := explicitPhase(b)
			if phase == "" {
				// A later block can still call a tool. Do not announce a
				// final answer before the canonical response has finished.
				phase = "commentary"
			}
			item = map[string]any{"type": "message", "id": id, "role": "assistant", "status": "in_progress", "phase": phase, "content": []any{}}
		} else {
			if b.ID == "" || b.Name == "" {
				return nil, ErrInvalid
			}
			s.hasTool = true
			pending, err := s.finishPendingText()
			if err != nil {
				return nil, err
			}
			out = append(out, pending...)
			kind, field := "function_call", "arguments"
			if s.custom[b.Name] || string(b.Extra["responses_custom_tool"]) == "true" {
				kind, field = "custom_tool_call", "input"
			}
			item = map[string]any{"type": kind, "id": id, "call_id": b.ID, "name": b.Name, "status": "in_progress", field: ""}
			restoreToolOrigin(item, b, s.origins)
			if present(b.Input) && string(b.Input) != "{}" {
				sb.argument.Write(b.Input)
			}
		}
		out = append(out, s.event("response.output_item.added", map[string]any{"output_index": sb.output, "item": item}))
		if b.Type == "text" {
			out = append(out, s.event("response.content_part.added", map[string]any{"output_index": sb.output, "item_id": id, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}}))
			if *b.Text != "" {
				out = append(out, s.event("response.output_text.delta", map[string]any{"output_index": sb.output, "item_id": id, "content_index": 0, "delta": *b.Text}))
			}
		}
	case "content_block_delta":
		if chunk.Index == nil {
			return nil, ErrInvalid
		}
		b := s.blocks[*chunk.Index]
		if b == nil || b.closed {
			return nil, ErrInvalid
		}
		d, err := object(chunk.Delta)
		if err != nil {
			return nil, err
		}
		id := itemID(s.id, b.output)
		switch optionalText(d["type"]) {
		case "text_delta":
			delta, err := text(d["text"])
			if err != nil || b.block.Type != "text" {
				return nil, ErrInvalid
			}
			b.block.Text = ptr(*b.block.Text + delta)
			out = append(out, s.event("response.output_text.delta", map[string]any{"output_index": b.output, "item_id": id, "content_index": 0, "delta": delta}))
		case "input_json_delta":
			delta, err := text(d["partial_json"])
			if err != nil || b.block.Type != "tool_use" {
				return nil, ErrInvalid
			}
			if b.argument.Len()+len(delta) > maxEventBytes {
				return nil, ErrInvalid
			}
			b.argument.WriteString(delta)
			// Custom freeform input is inside a JSON string. Buffer until
			// complete JSON exists rather than leaking JSON wrapper/escapes.
			if !s.custom[b.block.Name] && string(b.block.Extra["responses_custom_tool"]) != "true" {
				out = append(out, s.event("response.function_call_arguments.delta", map[string]any{"output_index": b.output, "item_id": id, "delta": delta}))
			}
		default:
			return nil, ErrUnsupported
		}
	case "content_block_stop":
		if chunk.Index == nil {
			return nil, ErrInvalid
		}
		closed, err := s.closeBlock(*chunk.Index, false)
		if err != nil {
			return nil, err
		}
		out = append(out, closed...)
	case "message_delta":
		if present(chunk.Delta) {
			d, err := object(chunk.Delta)
			if err != nil {
				return nil, err
			}
			if reason := optionalText(d["stop_reason"]); reason != "" {
				s.stop = reason
			}
		}
	case "message_stop":
		if _, err := usageFromCanonical(s.usage); err != nil {
			return nil, err
		}
		for _, index := range s.order {
			if !s.blocks[index].closed {
				closed, err := s.closeBlock(index, true)
				if err != nil {
					return nil, err
				}
				out = append(out, closed...)
			}
		}
		pending, err := s.finishPendingText()
		if err != nil {
			return nil, err
		}
		out = append(out, pending...)
		resp := &schema.ChatResponse{ID: s.id, Model: s.model, Usage: s.usage, StopReason: ptr(s.stop), Content: []schema.ContentBlock{}}
		for _, index := range s.order {
			resp.Content = append(resp.Content, s.blocks[index].block)
		}
		m, err := responseObject(resp, s.custom, s.origins)
		if err != nil {
			return nil, err
		}
		m["created_at"] = s.created
		kind := "response.completed"
		if m["status"] == "incomplete" {
			kind = "response.incomplete"
		}
		out = append(out, s.event(kind, map[string]any{"response": m}))
		s.terminal = true
	case "error":
		m := s.skeleton("failed")
		m["error"] = map[string]string{"code": "server_error", "message": "upstream response failed"}
		if u, err := usageFromCanonical(s.usage); err == nil {
			m["usage"] = u
		}
		out = append(out, s.event("response.failed", map[string]any{"response": m}))
		s.terminal = true
	default:
		return nil, ErrUnsupported
	}
	return out, nil
}

func (s *StreamState) closeBlock(index int, final bool) ([]Event, error) {
	b := s.blocks[index]
	if b == nil || b.closed {
		return nil, ErrInvalid
	}
	id := itemID(s.id, b.output)
	if b.block.Type == "tool_use" {
		if b.argument.Len() == 0 {
			b.block.Input = json.RawMessage("{}")
		} else {
			b.block.Input = json.RawMessage(b.argument.String())
		}
	}
	item, err := outputItem(b.block, s.id, b.output, s.custom, s.defaultPhase(), s.origins)
	if err != nil {
		return nil, err
	}
	var events []Event
	switch item["type"] {
	case "message":
		events = append(events,
			s.event("response.output_text.done", map[string]any{"output_index": b.output, "item_id": id, "content_index": 0, "text": *b.block.Text}),
			s.event("response.content_part.done", map[string]any{"output_index": b.output, "item_id": id, "content_index": 0, "part": map[string]any{"type": "output_text", "text": *b.block.Text, "annotations": []any{}}}),
		)
	case "custom_tool_call":
		events = append(events,
			s.event("response.custom_tool_call_input.delta", map[string]any{"output_index": b.output, "item_id": id, "delta": item["input"]}),
			s.event("response.custom_tool_call_input.done", map[string]any{"output_index": b.output, "item_id": id, "input": item["input"]}),
		)
	default:
		events = append(events, s.event("response.function_call_arguments.done", map[string]any{"output_index": b.output, "item_id": id, "arguments": item["arguments"]}))
	}
	// Text content can finish before a later tool block begins. Keep streaming
	// text/content events, but defer the phase-bearing item completion until
	// either a tool is known or the whole response has ended. Explicit phases
	// remain authoritative and do not need this inference.
	if b.block.Type != "text" || explicitPhase(b.block) != "" || s.hasTool || final {
		events = append(events, s.event("response.output_item.done", map[string]any{"output_index": b.output, "item": item}))
		b.itemDone = true
	}
	b.closed = true
	return events, nil
}

func (s *StreamState) defaultPhase() string {
	if s.hasTool {
		return "commentary"
	}
	return "final_answer"
}

func (s *StreamState) finishPendingText() ([]Event, error) {
	var events []Event
	for _, index := range s.order {
		b := s.blocks[index]
		if !b.closed || b.itemDone {
			continue
		}
		item, err := outputItem(b.block, s.id, b.output, s.custom, s.defaultPhase(), s.origins)
		if err != nil {
			return nil, err
		}
		events = append(events, s.event("response.output_item.done", map[string]any{"output_index": b.output, "item": item}))
		b.itemDone = true
	}
	return events, nil
}

func (s *StreamState) Finish() ([]Event, error) {
	if !s.terminal {
		return nil, ErrTruncated
	}
	return nil, nil
}

// ParseEvent parses a native Responses JSON event for observation. It never
// echoes remote error messages. ReadSSE adds stream-level terminal validation.
func ParseEvent(data []byte) (*schema.ChatChunk, error) {
	m, err := object(data)
	if err != nil {
		return nil, err
	}
	kind := optionalText(m["type"])
	if kind == "" {
		return nil, ErrInvalid
	}
	index := 0
	if present(m["output_index"]) && (json.Unmarshal(m["output_index"], &index) != nil || index < 0) {
		return nil, ErrInvalid
	}
	switch kind {
	case "response.created":
		resp, err := object(m["response"])
		if err != nil {
			return nil, err
		}
		return &schema.ChatChunk{Type: "message_start", Message: &schema.ChatResponse{ID: optionalText(resp["id"]), Type: "message", Role: "assistant", Model: optionalText(resp["model"]), Content: []schema.ContentBlock{}}}, nil
	case "response.output_text.delta":
		delta, err := text(m["delta"])
		if err != nil {
			return nil, err
		}
		return &schema.ChatChunk{Type: "content_block_delta", Index: &index, Delta: marshal(map[string]string{"type": "text_delta", "text": delta})}, nil
	case "response.function_call_arguments.delta":
		delta, err := text(m["delta"])
		if err != nil {
			return nil, err
		}
		return &schema.ChatChunk{Type: "content_block_delta", Index: &index, Delta: marshal(map[string]string{"type": "input_json_delta", "partial_json": delta})}, nil
	case "response.custom_tool_call_input.done":
		input, err := text(m["input"])
		if err != nil {
			return nil, err
		}
		return &schema.ChatChunk{Type: "content_block_delta", Index: &index, Delta: marshal(map[string]string{"type": "input_json_delta", "partial_json": string(marshal(map[string]string{"input": input}))})}, nil
	case "response.output_item.added":
		item, err := object(m["item"])
		if err != nil {
			return nil, err
		}
		switch optionalText(item["type"]) {
		case "function_call", "custom_tool_call":
			b := &schema.ContentBlock{Type: "tool_use", ID: optionalText(item["call_id"]), Name: optionalText(item["name"]), Input: json.RawMessage("{}")}
			if optionalText(item["type"]) == "custom_tool_call" {
				b.Extra = map[string]json.RawMessage{"responses_custom_tool": json.RawMessage("true")}
			}
			if present(item["namespace"]) {
				if b.Extra == nil {
					b.Extra = map[string]json.RawMessage{}
				}
				b.Extra["responses_namespace"] = item["namespace"]
			}
			return &schema.ChatChunk{Type: "content_block_start", Index: &index, ContentBlock: b}, nil
		case "message":
			b := &schema.ContentBlock{Type: "text", Text: ptr("")}
			if present(item["phase"]) {
				b.Extra = map[string]json.RawMessage{"phase": item["phase"]}
			}
			return &schema.ChatChunk{Type: "content_block_start", Index: &index, ContentBlock: b}, nil
		}
	case "response.output_item.done":
		return &schema.ChatChunk{Type: "content_block_stop", Index: &index}, nil
	case "response.completed", "response.incomplete":
		resp, err := ResponseToCanonical(m["response"])
		if err != nil {
			return nil, err
		}
		if optionalText(rawField(m["response"], "status")) != strings.TrimPrefix(kind, "response.") {
			return nil, ErrInvalid
		}
		return &schema.ChatChunk{Type: "message_delta", Usage: resp.Usage, Delta: marshal(map[string]any{"stop_reason": resp.StopReason})}, nil
	case "response.failed", "error":
		c := &schema.ChatChunk{Type: "error"}
		if resp, err := object(m["response"]); err == nil {
			c.Usage, _ = usageToCanonical(resp["usage"])
		}
		return c, ErrFailed
	}
	return nil, nil // native features stay verbatim; never pretend to convert them
}

func rawField(raw json.RawMessage, key string) json.RawMessage {
	var m map[string]json.RawMessage
	_ = json.Unmarshal(raw, &m)
	return m[key]
}

const maxEventBytes = 8 << 20

// ReadSSE preserves exact native event bytes (including CRLF and keepalives).
// Successful terminal events must carry usable usage; EOF/[DONE] cannot create
// success. Consumers may stop iteration on cancellation without draining.
func ReadSSE(r io.Reader, model string) iter.Seq2[*providers.StreamEvent, error] {
	return func(yield func(*providers.StreamEvent, error) bool) {
		reader := bufio.NewReader(r)
		for {
			raw, err := readEvent(reader)
			if len(raw) == 0 {
				if err == io.EOF {
					yield(nil, ErrTruncated)
				} else if err != nil {
					yield(nil, err)
				}
				return
			}
			var payload []string
			name := ""
			for _, line := range strings.Split(string(raw), "\n") {
				line = strings.TrimSuffix(line, "\r")
				if strings.HasPrefix(line, "data:") {
					value := strings.TrimPrefix(line, "data:")
					payload = append(payload, strings.TrimPrefix(value, " "))
				} else if strings.HasPrefix(line, "event:") {
					name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
				}
			}
			if len(payload) == 0 {
				if !yield(&providers.StreamEvent{Raw: raw}, nil) {
					return
				}
			} else {
				data := []byte(strings.Join(payload, "\n"))
				m, perr := object(data)
				if perr != nil {
					yield(nil, ErrInvalid)
					return
				}
				kind := optionalText(m["type"])
				if name != "" && name != kind {
					yield(nil, ErrInvalid)
					return
				}
				chunk, perr := ParseEvent(data)
				if chunk != nil && chunk.Message != nil {
					chunk.Message.Model = model
				}
				if perr != nil {
					if perr == ErrFailed && chunk != nil {
						// Preserve observed usage for settlement before reporting
						// the failure, but do not echo a secret-bearing error body.
						if !yield(&providers.StreamEvent{Chunk: chunk}, nil) {
							return
						}
					}
					yield(nil, perr)
					return
				}
				if !yield(&providers.StreamEvent{Raw: raw, Chunk: chunk}, nil) {
					return
				}
				if kind == "response.completed" || kind == "response.incomplete" {
					yield(&providers.StreamEvent{Chunk: &schema.ChatChunk{Type: "message_stop"}}, nil)
					return
				}
			}
			if err != nil {
				if err == io.EOF {
					err = ErrTruncated
				}
				yield(nil, err)
				return
			}
		}
	}
}

func readEvent(r *bufio.Reader) ([]byte, error) {
	var event, line []byte
	for {
		part, err := r.ReadSlice('\n')
		if len(event)+len(line)+len(part) > maxEventBytes {
			return nil, ErrInvalid
		}
		line = append(line, part...)
		if err == bufio.ErrBufferFull {
			continue
		}
		event = append(event, line...)
		blank := bytes.Equal(line, []byte("\n")) || bytes.Equal(line, []byte("\r\n"))
		line = nil
		if blank || err != nil {
			return event, err
		}
	}
}
