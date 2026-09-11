package responses

import (
	"bytes"
	"encoding/json"
	"slices"
)

func functionStrict(tool map[string]json.RawMessage) (bool, error) {
	if err := knownCase(tool, "strict"); err != nil {
		return false, err
	}
	value, explicit := tool["strict"]
	if !explicit {
		return true, nil
	}
	switch string(bytes.TrimSpace(value)) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, ErrInvalid
	}
}

// StrictTools reports whether any Responses function declaration requires strict
// schema enforcement, including the implicit Responses default. It reads root
// and namespaced tools, validates strict values, and never interprets custom
// tool schemas as function declarations. A caller must refuse on error; false
// alone is not permission to convert any other unsupported request feature.
func StrictTools(raw []byte) (bool, error) {
	fields, err := object(raw)
	if err != nil {
		return false, err
	}
	if err := knownCase(fields, "tools"); err != nil {
		return false, err
	}
	entries, err := flattenTools(fields["tools"], false)
	if err != nil {
		return false, err
	}
	required := false
	for _, entry := range entries {
		if optionalText(entry.wire["type"]) == "function" {
			enabled, err := functionStrict(entry.wire)
			if err != nil {
				return false, err
			}
			required = required || enabled
		}
	}
	return required, nil
}

// normalizeStrictSchema mirrors the implicit Responses strict default at schema
// locations only. In particular, const/default/enum values are application data,
// even if their keys happen to be named properties, items or required.
func normalizeStrictSchema(schema map[string]json.RawMessage) error {
	var properties map[string]json.RawMessage
	for _, key := range []string{"properties", "$defs", "definitions", "patternProperties", "dependentSchemas"} {
		if raw, exists := schema[key]; exists {
			children, err := object(raw)
			if err != nil {
				return err
			}
			for name, child := range children {
				normalized, err := strictSubschema(child)
				if err != nil {
					return err
				}
				children[name] = normalized
			}
			schema[key] = marshal(children)
			if key == "properties" {
				properties = children
			}
		}
	}
	for _, key := range []string{"items", "additionalProperties", "contains", "propertyNames", "not", "if", "then", "else"} {
		if raw, exists := schema[key]; exists {
			normalized, err := strictSubschema(raw)
			if err != nil {
				return err
			}
			schema[key] = normalized
		}
	}
	for _, key := range []string{"allOf", "anyOf", "oneOf", "prefixItems"} {
		if raw, exists := schema[key]; exists {
			var children []json.RawMessage
			if json.Unmarshal(raw, &children) != nil || children == nil {
				return ErrInvalid
			}
			for i, child := range children {
				normalized, err := strictSubschema(child)
				if err != nil {
					return err
				}
				children[i] = normalized
			}
			schema[key] = marshal(children)
		}
	}
	objectType := optionalText(schema["type"]) == "object"
	var types []string
	if json.Unmarshal(schema["type"], &types) == nil {
		objectType = objectType || slices.Contains(types, "object")
	}
	if objectType || properties != nil {
		required := make([]string, 0, len(properties))
		for name := range properties {
			required = append(required, name)
		}
		slices.Sort(required)
		schema["required"] = marshal(required)
		schema["additionalProperties"] = marshal(false)
	}
	return nil
}

func strictSubschema(raw json.RawMessage) (json.RawMessage, error) {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("true")) || bytes.Equal(bytes.TrimSpace(raw), []byte("false")) {
		return raw, nil
	}
	schema, err := object(raw)
	if err != nil {
		return nil, err
	}
	if err := normalizeStrictSchema(schema); err != nil {
		return nil, err
	}
	return marshal(schema), nil
}
