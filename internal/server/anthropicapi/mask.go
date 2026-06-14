package anthropicapi

import (
	"encoding/json"
	"fmt"

	"github.com/inferplane/inferplane/internal/filter"
)

// maskBody applies a RequestFilter to the TEXT of an Anthropic-ingress request
// body and returns the masked bytes + the redaction count (ADR-009 T3). It masks
// only `messages[].content` — the string form, and the `text` field of `text`
// blocks. It NEVER touches `system` (spec §302), `tool_use`/`tool_result`,
// `thinking`/`redacted_thinking`, `cache_control`, or any other field: those are
// re-emitted verbatim. The masked body is semantically equivalent JSON (key
// order may change, which is irrelevant — masked traffic has already abandoned
// verbatim/cache-safe forwarding). On any malformed-JSON error it returns the
// error so the caller can fail closed (never forward unmasked).
func maskBody(raw []byte, f filter.RequestFilter) ([]byte, int, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, 0, fmt.Errorf("maskBody: %w", err)
	}
	msgsRaw, ok := top["messages"]
	if !ok {
		return raw, 0, nil // nothing to mask
	}
	var msgs []json.RawMessage
	if err := json.Unmarshal(msgsRaw, &msgs); err != nil {
		return nil, 0, fmt.Errorf("maskBody messages: %w", err)
	}

	total := 0
	for i, mRaw := range msgs {
		var msg map[string]json.RawMessage
		if err := json.Unmarshal(mRaw, &msg); err != nil {
			return nil, 0, fmt.Errorf("maskBody message[%d]: %w", i, err)
		}
		content, ok := msg["content"]
		if !ok {
			continue
		}
		masked, n, err := maskContent(content, f)
		if err != nil {
			return nil, 0, err
		}
		if n > 0 {
			msg["content"] = masked
			remarshaled, err := json.Marshal(msg)
			if err != nil {
				return nil, 0, fmt.Errorf("maskBody remarshal message[%d]: %w", i, err)
			}
			msgs[i] = remarshaled
			total += n
		}
	}
	if total == 0 {
		return raw, 0, nil // unchanged — caller may still choose to forward verbatim
	}
	newMsgs, err := json.Marshal(msgs)
	if err != nil {
		return nil, 0, fmt.Errorf("maskBody remarshal messages: %w", err)
	}
	top["messages"] = newMsgs
	out, err := json.Marshal(top)
	if err != nil {
		return nil, 0, fmt.Errorf("maskBody remarshal: %w", err)
	}
	return out, total, nil
}

// maskContent masks a message's content, which is either a JSON string or an
// array of content blocks. For the array form only `text` blocks have their
// `text` field masked; every other block (tool_use/tool_result/thinking/…) and
// every other field (incl. cache_control) is preserved verbatim.
func maskContent(content json.RawMessage, f filter.RequestFilter) (json.RawMessage, int, error) {
	// string form
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		masked, n := f.Mask(s)
		if n == 0 {
			return content, 0, nil
		}
		b, err := json.Marshal(masked)
		return b, n, err
	}
	// array-of-blocks form
	var blocks []json.RawMessage
	if err := json.Unmarshal(content, &blocks); err != nil {
		return nil, 0, fmt.Errorf("maskContent: %w", err)
	}
	total := 0
	for i, bRaw := range blocks {
		var block map[string]json.RawMessage
		if err := json.Unmarshal(bRaw, &block); err != nil {
			return nil, 0, fmt.Errorf("maskContent block[%d]: %w", i, err)
		}
		var typ string
		_ = json.Unmarshal(block["type"], &typ)
		if typ != "text" {
			continue // tool_use / tool_result / thinking / … untouched
		}
		var text string
		if err := json.Unmarshal(block["text"], &text); err != nil {
			continue // non-string text field — leave the block alone
		}
		masked, n := f.Mask(text)
		if n == 0 {
			continue
		}
		mb, err := json.Marshal(masked)
		if err != nil {
			return nil, 0, err
		}
		block["text"] = mb // preserves cache_control and any sibling fields
		nb, err := json.Marshal(block)
		if err != nil {
			return nil, 0, err
		}
		blocks[i] = nb
		total += n
	}
	if total == 0 {
		return content, 0, nil
	}
	out, err := json.Marshal(blocks)
	return out, total, err
}
