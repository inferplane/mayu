package responses

import (
	"encoding/json"
	"testing"

	"github.com/inferplane/inferplane/internal/openai"
)

func TestFunctionStrictnessReachesChatWire(t *testing.T) {
	for _, value := range []string{"true", "false", ""} {
		for _, namespace := range []bool{false, true} {
			t.Run(value+map[bool]string{false: "/root", true: "/namespace"}[namespace], func(t *testing.T) {
				tool := `{"type":"function","name":"read","parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}`
				if value != "" {
					tool += `,"strict":` + value
				}
				tool += `}`
				if namespace {
					tool = `{"type":"namespace","name":"local","tools":[` + tool + `]}`
				}
				raw := []byte(`{"model":"m","input":"read","tools":[` + tool + `]}`)
				if err := ValidateConversion(raw); err != nil {
					t.Fatal(err)
				}
				req, err := RequestToCanonical(raw)
				if err != nil {
					t.Fatal(err)
				}
				var sink struct {
					Tools []struct {
						Function struct {
							Strict     *bool           `json:"strict"`
							Parameters json.RawMessage `json:"parameters"`
						} `json:"function"`
					} `json:"tools"`
				}
				wire := openai.CanonicalToRequest(req)
				if err := json.Unmarshal(wire, &sink); err != nil {
					t.Fatal(err)
				}
				if len(sink.Tools) != 1 || sink.Tools[0].Function.Strict == nil || *sink.Tools[0].Function.Strict != (value != "false") {
					t.Fatalf("source strict=%q lost at Chat sink: %s", value, wire)
				}
			})
		}
	}
}

func TestFunctionStrictnessRejectsUnknownOrNonBooleanValues(t *testing.T) {
	for _, field := range []string{`"strict":null`, `"strict":"true"`, `"strict":0`, `"strict":{}`, `"strict":[]`, `"Strict":true`} {
		for _, namespace := range []bool{false, true} {
			t.Run(field+map[bool]string{false: "/root", true: "/namespace"}[namespace], func(t *testing.T) {
				tool := `{"type":"function","name":"read","parameters":{"type":"object"},` + field + `}`
				if namespace {
					tool = `{"type":"namespace","name":"local","tools":[` + tool + `]}`
				}
				raw := []byte(`{"model":"m","input":"read","tools":[` + tool + `]}`)
				if err := ValidateConversion(raw); err == nil {
					t.Fatalf("invalid strictness silently accepted: %s", raw)
				}
			})
		}
	}
}

func TestImplicitStrictnessNormalizesOnlySchemaLocations(t *testing.T) {
	raw := []byte(`{"model":"m","input":"read","tools":[{"type":"function","name":"read","parameters":{
		"type":"object","properties":{"paths":{"type":"array","items":{"type":"object","properties":{"path":{"type":"string"}}}}},
		"const":{"properties":{"literal":"data"},"additionalProperties":true}}}]}`)
	req, err := RequestToCanonical(raw)
	if err != nil {
		t.Fatal(err)
	}
	var sink struct {
		Tools []struct {
			Function struct {
				Parameters struct {
					Additional bool     `json:"additionalProperties"`
					Required   []string `json:"required"`
					Properties struct {
						Paths struct {
							Items struct {
								Additional bool     `json:"additionalProperties"`
								Required   []string `json:"required"`
							} `json:"items"`
						} `json:"paths"`
					} `json:"properties"`
					Const json.RawMessage `json:"const"`
				} `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
	}
	wire := openai.CanonicalToRequest(req)
	if err := json.Unmarshal(wire, &sink); err != nil || len(sink.Tools) != 1 {
		t.Fatalf("invalid sink: %s; %v", wire, err)
	}
	p := sink.Tools[0].Function.Parameters
	if string(marshal(p.Required)) != `["paths"]` || p.Additional ||
		string(marshal(p.Properties.Paths.Items.Required)) != `["path"]` || p.Properties.Paths.Items.Additional {
		t.Fatalf("implicit Responses strict schema was not normalized for Chat: %s", wire)
	}
	if string(p.Const) != `{"properties":{"literal":"data"},"additionalProperties":true}` {
		t.Fatalf("normalization modified schema application data: %s", p.Const)
	}
}

func TestStrictToolsProvidesConservativeTargetGate(t *testing.T) {
	for _, tc := range []struct {
		name, tools string
		want, fail  bool
	}{
		{"no tools", `[]`, false, false},
		{"explicit false", `[{"type":"function","name":"read","strict":false,"parameters":{}}]`, false, false},
		{"explicit true", `[{"type":"function","name":"read","strict":true,"parameters":{}}]`, true, false},
		{"implicit true", `[{"type":"function","name":"read","parameters":{}}]`, true, false},
		{"namespaced", `[{"type":"namespace","name":"local","tools":[{"type":"function","name":"read","strict":true,"parameters":{}}]}]`, true, false},
		{"custom", `[{"type":"custom","name":"apply_patch"}]`, false, false},
		{"nonboolean", `[{"type":"function","name":"read","strict":"false","parameters":{}}]`, false, true},
		{"null", `[{"type":"function","name":"read","strict":null,"parameters":{}}]`, false, true},
		{"invalid after true", `[{"type":"function","name":"a","strict":true},{"type":"function","name":"b","strict":0}]`, false, true},
		{"unknown custom strict", `[{"type":"custom","name":"apply_patch","strict":true}]`, false, true},
		{"unknown namespace strict", `[{"type":"namespace","name":"local","strict":true,"tools":[{"type":"function","name":"read"}]}]`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte(`{"model":"m","input":"hello","tools":` + tc.tools + `}`)
			got, err := StrictTools(raw)
			if (err != nil) != tc.fail || got != tc.want {
				t.Fatalf("strict target gate=%v err=%v, want %v fail=%v", got, err, tc.want, tc.fail)
			}
			if tc.fail {
				if _, err := RequestToCanonical(raw); err == nil {
					t.Fatal("invalid strictness survived observation")
				}
			}
		})
	}
}

func TestExplicitStrictFalsePreservesOptionalSchema(t *testing.T) {
	parameters := `{"type":"object","properties":{"optional":{"type":"string"}},"additionalProperties":true}`
	raw := []byte(`{"model":"m","input":"x","tools":[{"type":"function","name":"f","strict":false,"parameters":` + parameters + `}]}`)
	req, err := RequestToCanonical(raw)
	if err != nil {
		t.Fatal(err)
	}
	var sink struct {
		Tools []struct {
			Function struct {
				Parameters map[string]json.RawMessage `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
	}
	wire := openai.CanonicalToRequest(req)
	if err := json.Unmarshal(wire, &sink); err != nil || len(sink.Tools) != 1 {
		t.Fatalf("invalid Chat sink=%s err=%v", wire, err)
	}
	p := sink.Tools[0].Function.Parameters
	if _, required := p["required"]; required || string(p["additionalProperties"]) != "true" {
		t.Fatalf("explicit non-strict schema was narrowed: %s", wire)
	}
}
