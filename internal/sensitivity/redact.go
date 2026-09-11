package sensitivity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
)

var errRedaction = errors.New("sensitivity: request cannot be completely redacted")

// Redactor implements complete, one-way request redaction over the same finite
// detector set as Inspector. It has no state or network dependencies.
type TextMasker interface {
	Mask(string) (string, int)
}

type Redactor struct{ additional TextMasker }

func NewRedactor() *Redactor { return &Redactor{} }

// NewRedactorWithMasker accumulates a separately configured legacy obligation.
// Its coverage may be broader than the finite inspector (for example compact
// phone numbers). Successful output must satisfy both sets of detectors.
func NewRedactorWithMasker(masker TextMasker) *Redactor {
	return &Redactor{additional: masker}
}

// Redact returns no usable bytes on failure. Structural identifiers, object
// keys, numeric scalars and opaque content are refused when they cannot be
// transformed without changing protocol/schema meaning. Successful redaction is
// re-inspected; completion is finite detector coverage, not universal PII safety.
func (r *Redactor) Redact(ctx context.Context, protocol string, raw []byte) ([]byte, error) {
	before, err := NewInspector().Inspect(ctx, protocol, raw)
	if err != nil {
		return nil, err
	}
	if !before.Complete {
		return nil, errRedaction
	}
	if len(before.Categories) == 0 && r.additional == nil {
		return bytes.Clone(raw), nil
	}
	w := walker{ctx: ctx, visit: func(string) error { return nil }}
	root, err := w.decode(raw, 0)
	if err != nil {
		return nil, err
	}
	out, changed, err := redactValue(ctx, root, redactionPosition{}, 0, r.additional)
	if err != nil {
		return nil, err
	}
	if !changed {
		if len(before.Categories) != 0 {
			return nil, errRedaction
		}
		return bytes.Clone(raw), nil
	}
	body, err := json.Marshal(out)
	if err != nil {
		return nil, errRedaction
	}
	after, err := NewInspector().Inspect(ctx, protocol, body)
	if err != nil {
		return nil, err
	}
	if !after.Complete || len(after.Categories) != 0 {
		return nil, errRedaction
	}
	if r.additional != nil {
		verify := walker{ctx: ctx, visit: func(text string) error {
			if additionalMatch(r.additional, text) {
				return errRedaction
			}
			return nil
		}}
		if _, err := verify.decode(body, 0); err != nil {
			return nil, errRedaction
		}
	}
	return body, nil
}

type redactionPosition struct {
	payload, text, schema, locked bool
}

func additionalMatch(masker TextMasker, text string) bool {
	if masker == nil {
		return false
	}
	masked, count := masker.Mask(text)
	return masked != text || count != 0
}

// payload means application data (e.g. decoded function arguments), where field
// names such as "name" are data rather than protocol identifiers. Object keys
// and numeric types remain immutable even there.
func redactValue(ctx context.Context, value any, pos redactionPosition, depth int, extra TextMasker) (any, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if depth > maxJSONDepth {
		return nil, false, errLimit
	}
	switch value := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(value))
		changed := false
		for key, child := range value {
			spans, err := redactionSpans(ctx, key)
			if err != nil {
				return nil, false, err
			}
			if len(spans) != 0 || additionalMatch(extra, key) {
				return nil, false, errRedaction
			}
			next := redactionPosition{
				payload: pos.payload || (key == "input" && value["type"] == "tool_use"),
				schema:  pos.schema || (!pos.payload && (key == "parameters" || key == "input_schema" || key == "schema")),
				locked:  pos.locked || (pos.schema && slices.Contains([]string{"const", "enum", "default", "examples"}, key)),
			}
			if !next.locked {
				if next.schema {
					next.text = key == "description" || key == "title"
				} else {
					next.text = next.payload || slices.Contains([]string{
						"text", "content", "system", "instructions", "description",
						"arguments", "output", "input", "refusal", "reasoning_content",
					}, key)
				}
			}
			v, didChange, err := redactValue(ctx, child, next, depth+1, extra)
			if err != nil {
				return nil, false, err
			}
			out[key], changed = v, changed || didChange
		}
		return out, changed, nil
	case []any:
		out := make([]any, len(value))
		changed := false
		for i, child := range value {
			v, didChange, err := redactValue(ctx, child, pos, depth+1, extra)
			if err != nil {
				return nil, false, err
			}
			out[i], changed = v, changed || didChange
		}
		return out, changed, nil
	case string:
		// The outer JSON string may itself carry encoded tool-result/argument
		// JSON. Recurse before masking so escape sequences cannot hide data.
		trimmed := strings.TrimSpace(value)
		if pos.text && (strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[")) && json.Valid([]byte(trimmed)) {
			w := walker{ctx: ctx, visit: func(string) error { return nil }}
			decoded, err := w.decode([]byte(trimmed), 0)
			if err != nil {
				return nil, false, err
			}
			out, changed, err := redactValue(ctx, decoded, redactionPosition{payload: true, text: true}, depth+1, extra)
			if err != nil {
				return nil, false, err
			}
			if changed {
				encoded, err := json.Marshal(out)
				if err != nil {
					return nil, false, errRedaction
				}
				return string(encoded), true, nil
			}
		}
		spans, err := redactionSpans(ctx, value)
		if err != nil {
			return nil, false, err
		}
		legacyMatch := additionalMatch(extra, value)
		if len(spans) == 0 && !legacyMatch {
			return value, false, nil
		}
		if !pos.text {
			return nil, false, errRedaction
		}
		var out strings.Builder
		start := 0
		for _, span := range spans {
			out.WriteString(value[start:span.start])
			out.WriteString("[REDACTED_PII]")
			start = span.end
		}
		out.WriteString(value[start:])
		masked := out.String()
		if extra != nil {
			masked, _ = extra.Mask(masked)
		}
		return masked, true, nil
	case json.Number:
		spans, err := redactionSpans(ctx, string(value))
		if err != nil {
			return nil, false, err
		}
		if len(spans) > 0 || additionalMatch(extra, string(value)) {
			return nil, false, errRedaction // a string placeholder would change its type
		}
	}
	return value, false, nil
}

type redactionSpan struct{ start, end int }

// Enumerate every match, unlike detect's category-only early exits. Use the
// exact same patterns, validators, boundaries and card candidate enumeration.
func redactionSpans(ctx context.Context, text string) ([]redactionSpan, error) {
	const chunk, overlap, maxSpans = 8192, 1024, 65536
	var spans []redactionSpan
	add := func(start, end int) error {
		if len(spans) >= maxSpans {
			return errLimit
		}
		spans = append(spans, redactionSpan{start, end})
		return nil
	}
	for start := 0; start < len(text); start += chunk {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		end := min(start+chunk+overlap, len(text))
		for _, detector := range detectors {
			for _, index := range detector.pattern.FindAllStringIndex(text[start:end], -1) {
				left, right := start+index[0], start+index[1]
				if end < len(text) && right == end {
					continue
				}
				if detector.category != "email" && !digitBoundary(text, left, right) {
					continue
				}
				if detector.valid == nil || detector.valid(text[left:right]) {
					if err := add(left, right); err != nil {
						return nil, err
					}
				}
			}
		}
		for left := start; left < end; left++ {
			if text[left] < '0' || text[left] > '9' ||
				(left > 0 && text[left-1] >= '0' && text[left-1] <= '9') {
				continue
			}
			right := left
			for count := 1; count <= 19 && right < end; count++ {
				if text[right] < '0' || text[right] > '9' {
					break
				}
				right++
				if count >= 13 && digitBoundary(text, left, right) && validCard(text[left:right]) {
					if err := add(left, right); err != nil {
						return nil, err
					}
				}
				if right < end && (text[right] == ' ' || text[right] == '-') {
					right++
				}
			}
		}
	}
	slices.SortFunc(spans, func(a, b redactionSpan) int {
		if a.start != b.start {
			return a.start - b.start
		}
		return b.end - a.end
	})
	merged := spans[:0]
	for _, span := range spans {
		if len(merged) > 0 && span.start <= merged[len(merged)-1].end {
			merged[len(merged)-1].end = max(merged[len(merged)-1].end, span.end)
		} else {
			merged = append(merged, span)
		}
	}
	return merged, nil
}
