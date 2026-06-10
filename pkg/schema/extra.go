// Package schema defines inferplane's canonical types — an
// Anthropic-Messages-shaped, protocol-neutral superset (design doc §2.2).
// Invariant: same-protocol round trip is lossless. Unknown fields are
// preserved via Extra maps; fields the pipeline does not yet interpret
// stay json.RawMessage ("minimal typing, maximal preservation").
package schema

import (
	"bytes"
	"encoding/json"
	"reflect"
)

// unmarshalWithExtra decodes data into v (standard json tags), then returns
// every top-level key NOT in known as raw bytes for lossless re-emission.
//
// Constraint: known entries must exactly match the json tag casing.
// Go decodes struct fields case-insensitively, but the strip here is
// exact-case — a mixed-case incoming key (e.g. "Known") would populate
// the typed field AND survive into extra, duplicating on re-marshal.
// Anthropic/OpenAI wire formats are lowercase snake_case, so exact
// lowercase known lists are correct by construction.
func unmarshalWithExtra(data []byte, v any, known ...string) (map[string]json.RawMessage, error) {
	if err := json.Unmarshal(data, v); err != nil {
		return nil, err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return nil, err
	}
	for _, k := range known {
		delete(all, k)
	}
	if len(all) == 0 {
		return nil, nil
	}
	return all, nil
}

// marshalWithExtra marshals v (its json tags decide known fields), then
// overlays extra keys. Known keys always win over stale extra entries.
//
// Constraint: extra must never contain a known key. A known field tagged
// omitempty that is later zeroed disappears from base, and a stale extra
// entry under the same key would silently resurrect the old value.
// Callers that mutate typed fields after unmarshal (M2+ filters) must
// not hold keys in extra that the type also models.
func marshalWithExtra(v any, extra map[string]json.RawMessage) ([]byte, error) {
	base, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	if len(extra) == 0 {
		return base, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(base, &m); err != nil {
		return nil, err
	}
	for k, raw := range extra {
		if _, exists := m[k]; !exists {
			m[k] = raw
		}
	}
	return json.Marshal(m)
}

// jsonSemanticEqual compares two JSON documents ignoring key order,
// using json.Number to avoid float64 precision loss on token counts.
func jsonSemanticEqual(a, b []byte) bool {
	var va, vb any
	da := json.NewDecoder(bytes.NewReader(a))
	da.UseNumber()
	db := json.NewDecoder(bytes.NewReader(b))
	db.UseNumber()
	if da.Decode(&va) != nil || db.Decode(&vb) != nil {
		return false
	}
	return reflect.DeepEqual(va, vb)
}
