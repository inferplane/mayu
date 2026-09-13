// Package responses adapts OpenAI Responses requests and events to the
// gateway's canonical schema. Observation does not authorize cross-wire
// conversion: callers must separately use ValidateConversion.
package responses

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

var (
	ErrInvalid      = errors.New("responses: invalid protocol payload")
	ErrUnsupported  = errors.New("responses: unsupported cross-protocol surface")
	ErrMissingUsage = errors.New("responses: terminal response has missing or invalid usage")
	ErrTruncated    = errors.New("responses: stream ended without a terminal response")
	ErrFailed       = errors.New("responses: upstream response failed")
)

// object validates the whole document, including duplicate keys and trailing
// values. Error messages deliberately contain no submitted data.
func object(raw []byte) (map[string]json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := uniqueValue(d, 0); err != nil {
		return nil, ErrInvalid
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, ErrInvalid
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return nil, ErrInvalid
	}
	return m, nil
}

func uniqueValue(d *json.Decoder, depth int) error {
	if depth > 128 {
		return ErrInvalid
	}
	t, err := d.Token()
	if err != nil {
		return ErrInvalid
	}
	v, ok := t.(json.Delim)
	if !ok {
		return nil
	}
	if v == '{' {
		seen := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			k, ok := key.(string)
			if err != nil || !ok || seen[k] {
				return ErrInvalid
			}
			seen[k] = true
			if err := uniqueValue(d, depth+1); err != nil {
				return err
			}
		}
	} else if v == '[' {
		for d.More() {
			if err := uniqueValue(d, depth+1); err != nil {
				return err
			}
		}
	} else {
		return ErrInvalid
	}
	_, err = d.Token()
	return err
}

func text(raw json.RawMessage) (string, error) {
	var s string
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil || string(raw) == "null" {
		return "", ErrInvalid
	}
	return s, nil
}

func optionalText(raw json.RawMessage) string {
	s, _ := text(raw)
	return s
}

func present(raw json.RawMessage) bool {
	return len(raw) != 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func marshal(v any) json.RawMessage {
	b, _ := json.Marshal(v) // used only with package-owned JSON-compatible values
	return b
}

func ptr[T any](v T) *T { return &v }

func knownCase(m map[string]json.RawMessage, names ...string) error {
	for k := range m {
		for _, n := range names {
			if strings.EqualFold(k, n) && k != n {
				return ErrInvalid
			}
		}
	}
	return nil
}
