// Package sensitivity inspects borrowed request bytes locally without modifying
// them or retaining their text. Its finite detectors are signals, not a guarantee
// that every kind of sensitive data can be recognized.
package sensitivity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"unicode/utf8"
)

// Result contains only bounded categories and aggregate request signals.
// Complete means the submitted content was inspectable, not that it is PII-free.
type Result struct {
	Complete            bool
	Categories          []string
	InputTokens         int64
	OutputTokens        int64
	UserTurns           int
	HasHistory          bool
	HasTools            bool
	HasVision           bool
	HasReasoning        bool
	HasStructuredOutput bool
}

// Inspector accepts only anthropic, openai, and bedrock ingress protocols.
// Bedrock token-count callers supply the already-decoded inner request body.
type Inspector interface {
	Inspect(context.Context, string, []byte) (Result, error)
}

type inspector struct{}

// NewInspector returns a stateless inspector safe for concurrent use. Returned
// category slices belong to the caller; no request data survives the call.
func NewInspector() Inspector { return inspector{} }

var (
	// Never wrap decoder/number errors: they can contain submitted text.
	errMalformed = errors.New("sensitivity: malformed JSON")
	errDuplicate = errors.New("sensitivity: duplicate JSON key")
	errLimit     = errors.New("sensitivity: inspection limit exceeded")
	errProtocol  = errors.New("sensitivity: unsupported protocol")
	errBudget    = errors.New("sensitivity: invalid output token budget")
)

const (
	maxBodyBytes    = 64 << 20
	maxDecodedBytes = 128 << 20
	maxNodes        = 1 << 18
	maxJSONDepth    = 64
	maxEncodedDepth = 8
)

func (inspector) Inspect(ctx context.Context, protocol string, raw []byte) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	switch protocol {
	case "anthropic", "openai", "bedrock":
	default:
		return Result{}, errProtocol
	}
	categories := make(map[string]bool)
	w := walker{ctx: ctx, visit: func(text string) error {
		return detect(ctx, text, categories)
	}}
	root, err := w.decode(raw, 0)
	if err != nil {
		return Result{}, err
	}
	result := Result{Complete: true, InputTokens: w.inputTokens}
	// One token per decoded UTF-8 byte, plus eight per JSON value, budgets
	// keys, tool definitions and framing conservatively. This is deliberately
	// not a tokenizer or a claim about an upstream's exact token accounting.
	if err := result.inspectShape(protocol, root); err != nil {
		return Result{}, err
	}
	if body, ok := root.(map[string]any); ok {
		result.OutputTokens, err = outputBudget(body)
		if err != nil {
			return Result{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	for category := range categories {
		result.Categories = append(result.Categories, category)
	}
	slices.Sort(result.Categories)
	return result, nil
}

// walker uses tokens instead of map unmarshalling so that no duplicate member
// can hide an earlier value. It scans keys too, including nested encoded JSON.
// Only shape.go interprets protocol content positions; application objects are
// never treated as protocol blocks merely because they have a "type" member.
type walker struct {
	ctx         context.Context
	visit       func(string) error
	nodes       int
	decoded     int64
	inputTokens int64
}

func (w *walker) decode(raw []byte, encodedDepth int) (any, error) {
	if err := w.ctx.Err(); err != nil {
		return nil, err
	}
	if len(raw) > maxBodyBytes || encodedDepth > maxEncodedDepth {
		return nil, errLimit
	}
	if !utf8.Valid(raw) {
		return nil, errMalformed
	}
	d := json.NewDecoder(&contextReader{ctx: w.ctx, reader: bytes.NewReader(raw)})
	d.UseNumber()
	value, err := w.value(d, 0, encodedDepth)
	if err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, w.decodeError(err)
	}
	return value, nil
}

func (w *walker) decodeError(_ error) error {
	if err := w.ctx.Err(); err != nil {
		return err
	}
	return errMalformed
}

func (w *walker) value(d *json.Decoder, depth, encodedDepth int) (any, error) {
	if err := w.ctx.Err(); err != nil {
		return nil, err
	}
	w.nodes++
	if depth > maxJSONDepth || w.nodes > maxNodes {
		return nil, errLimit
	}
	w.inputTokens += 8
	token, err := d.Token()
	if err != nil {
		return nil, w.decodeError(err)
	}
	switch token := token.(type) {
	case json.Delim:
		switch token {
		case '{':
			object := make(map[string]any)
			for d.More() {
				keyToken, err := d.Token()
				if err != nil {
					return nil, w.decodeError(err)
				}
				key, ok := keyToken.(string)
				if !ok {
					return nil, errMalformed
				}
				if _, duplicate := object[key]; duplicate {
					return nil, errDuplicate
				}
				if err := w.text(key, encodedDepth); err != nil {
					return nil, err
				}
				value, err := w.value(d, depth+1, encodedDepth)
				if err != nil {
					return nil, err
				}
				object[key] = value
			}
			if close, err := d.Token(); err != nil || close != json.Delim('}') {
				return nil, w.decodeError(err)
			}
			return object, nil
		case '[':
			var array []any
			for d.More() {
				value, err := w.value(d, depth+1, encodedDepth)
				if err != nil {
					return nil, err
				}
				array = append(array, value)
			}
			if close, err := d.Token(); err != nil || close != json.Delim(']') {
				return nil, w.decodeError(err)
			}
			return array, nil
		default:
			return nil, errMalformed
		}
	case string:
		if err := w.text(token, encodedDepth); err != nil {
			return nil, err
		}
	case json.Number:
		w.inputTokens += int64(len(token))
	}
	return token, nil
}

func (w *walker) text(text string, encodedDepth int) error {
	if err := w.ctx.Err(); err != nil {
		return err
	}
	w.decoded += int64(len(text))
	if w.decoded > maxDecodedBytes {
		return errLimit
	}
	w.inputTokens += int64(len(text))
	if err := w.visit(text); err != nil {
		return err
	}
	trimmed := strings.TrimSpace(text)
	if len(trimmed) == 0 || (trimmed[0] != '{' && trimmed[0] != '[' && trimmed[0] != '"') {
		return nil
	}
	// Ordinary prose can begin with a brace or quote. Only valid encoded JSON
	// is recursively inspected; ambiguity, cancellation and bounds still fail.
	// Stage visits and accounting in a copy: a malformed speculative document
	// must not publish partially decoded categories, keywords or token counts.
	probe := *w
	var pending []string
	probe.visit = func(text string) error {
		pending = append(pending, text)
		return nil
	}
	_, err := probe.decode([]byte(trimmed), encodedDepth+1)
	if errors.Is(err, errMalformed) {
		return nil
	}
	if err != nil {
		return err
	}
	w.nodes, w.decoded, w.inputTokens = probe.nodes, probe.decoded, probe.inputTokens
	for _, text := range pending {
		if err := w.ctx.Err(); err != nil {
			return err
		}
		if err := w.visit(text); err != nil {
			return err
		}
	}
	return nil
}

type contextReader struct {
	ctx    context.Context
	reader *bytes.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	// Check cancellation even while the JSON decoder reads one large string.
	if len(p) > 4096 {
		p = p[:4096]
	}
	return r.reader.Read(p)
}

func outputBudget(body map[string]any) (int64, error) {
	var max int64
	read := func(fields map[string]any, key string) error {
		value, exists := fields[key]
		if !exists {
			return nil
		}
		number, ok := value.(json.Number)
		if !ok {
			return errBudget
		}
		// JSON APIs specify integers. Reject fractions/exponents rather than
		// rounding them, and never expose strconv's input-bearing error.
		n, err := number.Int64()
		if err != nil || n < 0 {
			return errBudget
		}
		if n > max {
			max = n
		}
		return nil
	}
	for _, key := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
		if err := read(body, key); err != nil {
			return 0, err
		}
	}
	if cfg, ok := body["inferenceConfig"].(map[string]any); ok {
		if err := read(cfg, "maxTokens"); err != nil {
			return 0, err
		}
	}
	return max, nil
}
