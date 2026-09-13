package responses

import (
	"encoding/json"
	"math"
	"time"

	"github.com/inferplane/inferplane/pkg/schema"
)

type wireUsage struct {
	Input   *int64 `json:"input_tokens"`
	Output  *int64 `json:"output_tokens"`
	Details *struct {
		Cached *int64 `json:"cached_tokens"`
		Write  *int64 `json:"cache_write_tokens"`
	} `json:"input_tokens_details"`
}

func usageToCanonical(raw json.RawMessage) (*schema.Usage, error) {
	fields, err := object(raw)
	if err != nil || knownCase(fields, "input_tokens", "output_tokens", "input_tokens_details") != nil {
		return nil, ErrMissingUsage
	}
	if present(fields["input_tokens_details"]) {
		details, err := object(fields["input_tokens_details"])
		if err != nil || knownCase(details, "cached_tokens", "cache_write_tokens") != nil {
			return nil, ErrMissingUsage
		}
	}
	var u wireUsage
	if !present(raw) || json.Unmarshal(raw, &u) != nil || u.Input == nil || u.Output == nil || *u.Input < 0 || *u.Output < 0 {
		return nil, ErrMissingUsage
	}
	cached, write := int64(0), int64(0)
	if u.Details != nil {
		if u.Details.Cached != nil {
			cached = *u.Details.Cached
		}
		if u.Details.Write != nil {
			write = *u.Details.Write
		}
	}
	if cached < 0 || write < 0 || cached > *u.Input || write > *u.Input-cached || *u.Output > math.MaxInt64-*u.Input {
		return nil, ErrMissingUsage
	}
	// Responses input_tokens is inclusive; canonical input is the uncached
	// portion. Output already includes reasoning tokens: never add them twice.
	out := &schema.Usage{InputTokens: ptr(*u.Input - cached - write), OutputTokens: u.Output, CacheReadInputTokens: ptr(cached)}
	if write > 0 {
		out.CacheCreationInputTokens = ptr(write)
	}
	return out, nil
}

// ResponseUsage validates and extracts accounting independently of response
// status and output decoding. Failed or incompatible output can still be
// billable. Invalid JSON, ambiguous fields and invalid counts return no usage.
func ResponseUsage(raw []byte) (*schema.Usage, error) {
	fields, err := object(raw)
	if err != nil {
		return nil, err
	}
	if err := knownCase(fields, "usage"); err != nil {
		return nil, err
	}
	return usageToCanonical(fields["usage"])
}

func usageFromCanonical(u *schema.Usage) (map[string]any, error) {
	if u == nil || u.InputTokens == nil || u.OutputTokens == nil || *u.InputTokens < 0 || *u.OutputTokens < 0 {
		return nil, ErrMissingUsage
	}
	read := int64(0)
	if u.CacheReadInputTokens != nil {
		read = *u.CacheReadInputTokens
	}
	w5, w1 := u.CacheWriteTiers()
	total := *u.InputTokens
	for _, n := range []int64{read, w5, w1, *u.OutputTokens} {
		if n < 0 || n > math.MaxInt64-total {
			return nil, ErrMissingUsage
		}
		total += n
	}
	return map[string]any{
		"input_tokens": total - *u.OutputTokens, "output_tokens": *u.OutputTokens,
		"total_tokens":          total,
		"input_tokens_details":  map[string]int64{"cached_tokens": read, "cache_write_tokens": w5 + w1},
		"output_tokens_details": map[string]int64{"reasoning_tokens": 0},
	}, nil
}

// ResponseToCanonical refuses nonterminal successes and missing usage. Unknown
// native output items remain opaque observation blocks; callers tee RawBody.
func ResponseToCanonical(raw []byte) (*schema.ChatResponse, error) {
	m, err := object(raw)
	if err != nil {
		return nil, err
	}
	if err := knownCase(m, "id", "model", "status", "output", "usage"); err != nil {
		return nil, err
	}
	status := optionalText(m["status"])
	if status != "completed" && status != "incomplete" {
		return nil, ErrFailed
	}
	if present(m["error"]) {
		return nil, ErrFailed
	}
	u, err := usageToCanonical(m["usage"])
	if err != nil {
		return nil, err
	}
	out := &schema.ChatResponse{ID: optionalText(m["id"]), Type: "message", Role: "assistant", Model: optionalText(m["model"]), Content: []schema.ContentBlock{}, Usage: u, StopReason: ptr("end_turn")}
	if status == "incomplete" {
		out.StopReason = ptr("max_tokens")
		var details struct {
			Reason string `json:"reason"`
		}
		if json.Unmarshal(m["incomplete_details"], &details) != nil || details.Reason != "max_output_tokens" {
			out.StopReason = ptr("refusal")
		}
	}
	var items []json.RawMessage
	if !present(m["output"]) || json.Unmarshal(m["output"], &items) != nil {
		return nil, ErrInvalid
	}
	for _, rawItem := range items {
		item, err := object(rawItem)
		if err != nil {
			return nil, err
		}
		switch optionalText(item["type"]) {
		case "message":
			blocks, err := contentToCanonical(item["content"])
			if err != nil {
				return nil, err
			}
			for i := range blocks {
				if present(item["phase"]) {
					if blocks[i].Extra == nil {
						blocks[i].Extra = map[string]json.RawMessage{}
					}
					blocks[i].Extra["phase"] = item["phase"]
				}
			}
			out.Content = append(out.Content, blocks...)
		case "function_call", "custom_tool_call":
			name, id := optionalText(item["name"]), optionalText(item["call_id"])
			if name == "" || id == "" {
				return nil, ErrInvalid
			}
			b := schema.ContentBlock{Type: "tool_use", ID: id, Name: name}
			if optionalText(item["type"]) == "custom_tool_call" {
				input, err := text(item["input"])
				if err != nil {
					return nil, err
				}
				b.Input = marshal(map[string]string{"input": input})
				b.Extra = map[string]json.RawMessage{"responses_custom_tool": json.RawMessage("true")}
			} else {
				args, err := text(item["arguments"])
				if err != nil {
					return nil, err
				}
				if _, err := object([]byte(args)); err != nil {
					// Native incomplete calls can have partial JSON arguments.
					if status != "incomplete" {
						return nil, ErrInvalid
					}
				}
				b.Input = json.RawMessage(args)
			}
			if present(item["namespace"]) {
				namespace, err := text(item["namespace"])
				if err != nil || namespace == "" {
					return nil, ErrInvalid
				}
				if b.Extra == nil {
					b.Extra = map[string]json.RawMessage{}
				}
				b.Extra["responses_namespace"] = item["namespace"]
			}
			out.Content = append(out.Content, b)
			if status == "completed" {
				out.StopReason = ptr("tool_use")
			}
		default:
			out.Content = append(out.Content, schema.ContentBlock{Type: "responses_opaque", Extra: map[string]json.RawMessage{"item": rawItem}})
		}
	}
	return out, nil
}

// CanonicalToResponse renders stateless text/function output. Use the
// request-aware variant when custom tools were adapted to canonical functions.
func CanonicalToResponse(resp *schema.ChatResponse) ([]byte, error) {
	return CanonicalToResponseForRequest(resp, nil)
}

func CanonicalToResponseForRequest(resp *schema.ChatResponse, req *schema.ChatRequest) ([]byte, error) {
	m, err := responseObject(resp, customTools(req), toolOrigins(req))
	if err != nil {
		return nil, err
	}
	return json.Marshal(m)
}

func responseObject(resp *schema.ChatResponse, custom map[string]bool, origins map[string]toolOrigin) (map[string]any, error) {
	if resp == nil {
		return nil, ErrInvalid
	}
	usage, err := usageFromCanonical(resp.Usage)
	if err != nil {
		return nil, err
	}
	id := resp.ID
	if id == "" {
		id = "resp_gateway"
	}
	items := make([]map[string]any, 0, len(resp.Content))
	phase := "final_answer"
	for _, block := range resp.Content {
		if block.Type == "tool_use" {
			phase = "commentary"
			break
		}
	}
	for i, block := range resp.Content {
		item, err := outputItem(block, id, i, custom, phase, origins)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	status := "completed"
	var incomplete any
	if resp.StopReason != nil && (*resp.StopReason == "max_tokens" || *resp.StopReason == "refusal") {
		status = "incomplete"
		reason := "max_output_tokens"
		if *resp.StopReason == "refusal" {
			reason = "content_filter"
		}
		incomplete = map[string]string{"reason": reason}
	}
	return map[string]any{
		"id": id, "object": "response", "created_at": time.Now().Unix(), "status": status,
		"model": resp.Model, "output": items, "usage": usage, "error": nil,
		"incomplete_details": incomplete, "parallel_tool_calls": true,
	}, nil
}

func outputItem(b schema.ContentBlock, responseID string, index int, custom map[string]bool, defaultPhase string, origins map[string]toolOrigin) (map[string]any, error) {
	id := itemID(responseID, index)
	switch b.Type {
	case "text":
		if b.Text == nil {
			return nil, ErrInvalid
		}
		part := map[string]any{"type": "output_text", "text": *b.Text, "annotations": []any{}}
		if string(b.Extra["responses_refusal"]) == "true" {
			part = map[string]any{"type": "refusal", "refusal": *b.Text}
		}
		out := map[string]any{"type": "message", "id": id, "role": "assistant", "status": "completed", "content": []any{part}}
		if phase := explicitPhase(b); phase != "" {
			out["phase"] = phase
		} else {
			out["phase"] = defaultPhase
		}
		return out, nil
	case "tool_use":
		if b.ID == "" || b.Name == "" {
			return nil, ErrInvalid
		}
		args, err := object(b.Input)
		if err != nil {
			return nil, ErrInvalid
		}
		if custom[b.Name] || string(b.Extra["responses_custom_tool"]) == "true" {
			s, err := text(args["input"])
			if err != nil || len(args) != 1 {
				return nil, ErrInvalid
			}
			item := map[string]any{"type": "custom_tool_call", "id": id, "call_id": b.ID, "name": b.Name, "input": s, "status": "completed"}
			restoreToolOrigin(item, b, origins)
			return item, nil
		}
		item := map[string]any{"type": "function_call", "id": id, "call_id": b.ID, "name": b.Name, "arguments": string(b.Input), "status": "completed"}
		restoreToolOrigin(item, b, origins)
		return item, nil
	default:
		return nil, ErrUnsupported
	}
}

func explicitPhase(b schema.ContentBlock) string {
	phase := optionalText(b.Extra["phase"])
	if phase == "commentary" || phase == "final_answer" {
		return phase
	}
	return ""
}
