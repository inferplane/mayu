package schema

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Message is one turn. Anthropic accepts content as a bare string or a
// block array; contentIsString remembers the original form so re-emission
// is shape-identical (a normalized form would still be semantically equal,
// but byte-shape fidelity keeps diffs and caching analysis trivial).
type Message struct {
	Role    string         `json:"role"`
	Content []ContentBlock `json:"-"`

	contentIsString bool
	Extra           map[string]json.RawMessage `json:"-"`
}

func (m *Message) UnmarshalJSON(data []byte) error {
	var head struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return err
	}
	m.Role = head.Role
	if len(head.Content) > 0 && head.Content[0] == '"' {
		var s string
		if err := json.Unmarshal(head.Content, &s); err != nil {
			return err
		}
		m.Content = []ContentBlock{{Type: "text", Text: &s}}
		m.contentIsString = true
	} else if len(head.Content) > 0 {
		if err := json.Unmarshal(head.Content, &m.Content); err != nil {
			return err
		}
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return err
	}
	delete(all, "role")
	delete(all, "content")
	// Reject case-variant collisions: Go decodes case-insensitively but the
	// strip above is exact-case, so a key like "Role" would populate the typed
	// field AND survive into Extra, emitting duplicate keys on re-marshal — the
	// parser-differential smuggling vector f8969bb closed in unmarshalWithExtra.
	for k := range all {
		switch strings.ToLower(k) {
		case "role", "content":
			return fmt.Errorf("schema: case-variant of known key is not allowed: %q", k)
		}
	}
	if len(all) > 0 {
		m.Extra = all
	}
	return nil
}

func (m Message) MarshalJSON() ([]byte, error) {
	out := map[string]json.RawMessage{}
	roleRaw, _ := json.Marshal(m.Role)
	out["role"] = roleRaw
	var contentRaw []byte
	var err error
	if m.contentIsString && len(m.Content) == 1 && m.Content[0].Type == "text" && m.Content[0].Text != nil {
		contentRaw, err = json.Marshal(*m.Content[0].Text)
	} else {
		contentRaw, err = json.Marshal(m.Content)
	}
	if err != nil {
		return nil, err
	}
	out["content"] = contentRaw
	for k, raw := range m.Extra {
		if _, exists := out[k]; !exists {
			out[k] = raw
		}
	}
	return json.Marshal(out)
}
