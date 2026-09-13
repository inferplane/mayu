package responses

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/inferplane/inferplane/pkg/schema"
)

const originAnnotation = "x-inferplane-responses-tool-origin"

type toolOrigin struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

type toolEntry struct {
	wire                 map[string]json.RawMessage
	name                 string
	origin               toolOrigin
	namespaceDescription string
}

// namespaceAlias uses an unambiguous, domain-separated encoding. The readable
// prefix is advisory; the digest distinguishes even delimiter-colliding names.
// Every alias is <=64 ASCII characters and collisions are checked explicitly.
func namespaceAlias(namespace, name string) string {
	sum := sha256.Sum256(marshal([]string{"inferplane.responses.namespace.v1", namespace, name}))
	prefix := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			return r
		}
		return '_'
	}, namespace+"_"+name)
	if len(prefix) > 18 {
		prefix = prefix[:18]
	}
	return "ns_" + prefix + "_" + hex.EncodeToString(sum[:20])
}

func flattenTools(raw json.RawMessage, strict bool) ([]toolEntry, error) {
	if !present(raw) {
		return nil, nil
	}
	var tools []map[string]json.RawMessage
	if json.Unmarshal(raw, &tools) != nil {
		return nil, ErrInvalid
	}
	var entries []toolEntry
	namespaces := map[string]bool{}
	for _, tool := range tools {
		if err := knownCase(tool, "strict"); err != nil {
			return nil, err
		}
		if optionalText(tool["type"]) != "namespace" {
			entries = append(entries, toolEntry{wire: tool, name: optionalText(tool["name"])})
			continue
		}
		ns, err := text(tool["name"])
		if err != nil || ns == "" || namespaces[ns] {
			return nil, ErrInvalid
		}
		namespaces[ns] = true
		if _, exists := tool["strict"]; exists {
			return nil, ErrUnsupported // strictness belongs to function declarations
		}
		if strict {
			for k := range tool {
				if k != "type" && k != "name" && k != "description" && k != "tools" {
					return nil, ErrUnsupported
				}
			}
		}
		var nested []map[string]json.RawMessage
		if json.Unmarshal(tool["tools"], &nested) != nil || len(nested) == 0 {
			return nil, ErrUnsupported
		}
		for _, child := range nested {
			name := optionalText(child["name"])
			entries = append(entries, toolEntry{
				wire: child, name: namespaceAlias(ns, name),
				origin:               toolOrigin{Namespace: ns, Name: name},
				namespaceDescription: optionalText(tool["description"]),
			})
		}
	}
	if len(entries) > 4096 {
		return nil, ErrInvalid
	}
	seen := map[string]bool{}
	for _, entry := range entries {
		kind := optionalText(entry.wire["type"])
		if err := knownCase(entry.wire, "strict"); err != nil {
			return nil, err
		}
		if kind == "function" {
			if _, err := functionStrict(entry.wire); err != nil {
				return nil, err
			}
		} else if _, exists := entry.wire["strict"]; exists {
			return nil, ErrUnsupported
		}
		if kind != "function" && kind != "custom" {
			if strict {
				return nil, ErrUnsupported
			}
			continue
		}
		if optionalText(entry.wire["name"]) == "" || seen[entry.name] {
			return nil, ErrInvalid
		}
		seen[entry.name] = true
		if !strict {
			continue
		}
		if kind == "function" {
			if _, err := object(entry.wire["parameters"]); err != nil {
				return nil, ErrUnsupported
			}
		}
		for k := range entry.wire {
			switch k {
			case "type", "name", "description", "parameters", "strict", "format":
			default:
				return nil, ErrUnsupported
			}
		}
	}
	return entries, nil
}

func canonicalTools(raw json.RawMessage) (json.RawMessage, error) {
	entries, err := flattenTools(raw, false)
	if err != nil || !present(raw) {
		return nil, err
	}
	canonical := make([]map[string]json.RawMessage, 0, len(entries))
	for _, entry := range entries {
		tool := entry.wire
		kind := optionalText(tool["type"])
		if kind != "function" && kind != "custom" {
			canonical = append(canonical, tool) // native observation only
			continue
		}
		t := map[string]json.RawMessage{"name": marshal(entry.name)}
		description := optionalText(tool["description"])
		if entry.origin.Namespace != "" {
			description = "Tool " + entry.origin.Namespace + "." + entry.origin.Name + ". " + entry.namespaceDescription + "\n" + description
		}
		var inputSchema map[string]json.RawMessage
		if kind == "custom" {
			inputSchema = map[string]json.RawMessage{
				"type":       marshal("object"),
				"properties": marshal(map[string]any{"input": map[string]string{"type": "string", "description": "The complete freeform input to this tool."}}),
				"required":   marshal([]string{"input"}), "additionalProperties": marshal(false),
			}
			if present(tool["format"]) {
				description += "\nThe input string must obey this tool format: " + string(tool["format"])
			}
		} else if !present(tool["parameters"]) {
			inputSchema = map[string]json.RawMessage{"type": marshal("object")}
		} else {
			inputSchema, err = object(tool["parameters"])
			if err != nil {
				return nil, err
			}
		}
		if kind == "function" {
			enabled, err := functionStrict(tool)
			if err != nil {
				return nil, err
			}
			t["strict"] = marshal(enabled)
			if _, explicit := tool["strict"]; !explicit {
				// Responses normalizes an omitted strict setting. Chat defaults
				// to non-strict, so carry the effective flag and schema together.
				if err := normalizeStrictSchema(inputSchema); err != nil {
					return nil, err
				}
			}
		}
		// These annotations are created from the actual wire declaration,
		// never trusted from user-supplied JSON Schema annotations.
		delete(inputSchema, customAnnotation)
		delete(inputSchema, originAnnotation)
		if kind == "custom" {
			inputSchema[customAnnotation] = marshal(true)
		}
		if entry.origin.Namespace != "" {
			inputSchema[originAnnotation] = marshal(entry.origin)
		}
		t["input_schema"] = marshal(inputSchema)
		if description != "" {
			t["description"] = marshal(description)
		}
		canonical = append(canonical, t)
	}
	return marshal(canonical), nil
}

func callName(item map[string]json.RawMessage) (string, error) {
	name, err := text(item["name"])
	if err != nil || name == "" {
		return "", ErrInvalid
	}
	if present(item["namespace"]) {
		ns, err := text(item["namespace"])
		if err != nil || ns == "" {
			return "", ErrInvalid
		}
		return namespaceAlias(ns, name), nil
	}
	return name, nil
}

func validateNamespacedCall(item map[string]json.RawMessage, entries []toolEntry) error {
	if !present(item["namespace"]) {
		return nil
	}
	name, err := callName(item)
	if err != nil {
		return err
	}
	kind := optionalText(item["type"])
	want := ""
	switch kind {
	case "function", "function_call":
		want = "function"
	case "custom", "custom_tool_call":
		want = "custom"
	default:
		return ErrUnsupported
	}
	for _, entry := range entries {
		if entry.origin.Namespace != "" && entry.name == name && optionalText(entry.wire["type"]) == want {
			return nil
		}
	}
	return ErrUnsupported
}

func toolOrigins(req *schema.ChatRequest) map[string]toolOrigin {
	out := map[string]toolOrigin{}
	if req == nil {
		return out
	}
	var tools []struct {
		Name   string                     `json:"name"`
		Schema map[string]json.RawMessage `json:"input_schema"`
	}
	_ = json.Unmarshal(req.Tools, &tools)
	for _, t := range tools {
		var origin toolOrigin
		if json.Unmarshal(t.Schema[originAnnotation], &origin) == nil && origin.Namespace != "" && origin.Name != "" {
			out[t.Name] = origin
		}
	}
	return out
}

func restoreToolOrigin(item map[string]any, b schema.ContentBlock, origins map[string]toolOrigin) {
	if origin, ok := origins[b.Name]; ok {
		item["name"], item["namespace"] = origin.Name, origin.Namespace
	} else if namespace := optionalText(b.Extra["responses_namespace"]); namespace != "" {
		item["namespace"] = namespace
	}
}
