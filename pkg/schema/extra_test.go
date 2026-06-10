package schema

import (
	"encoding/json"
	"testing"
)

type sample struct {
	Known string                     `json:"known"`
	Extra map[string]json.RawMessage `json:"-"`
}

func TestExtraPreservesUnknownFields(t *testing.T) {
	in := []byte(`{"known":"a","future_field":{"x":1},"another":"y"}`)
	var s sample
	extra, err := unmarshalWithExtra(in, &s, "known")
	if err != nil {
		t.Fatal(err)
	}
	s.Extra = extra
	if s.Known != "a" {
		t.Fatalf("known = %q", s.Known)
	}
	out, err := marshalWithExtra(s, s.Extra)
	if err != nil {
		t.Fatal(err)
	}
	assertJSONSemanticEqual(t, in, out)
}

// assertJSONSemanticEqual: 키 순서 무시, 숫자 정밀도 보존 비교.
func assertJSONSemanticEqual(t *testing.T, want, got []byte) {
	t.Helper()
	if !jsonSemanticEqual(want, got) {
		t.Fatalf("JSON mismatch\nwant: %s\ngot:  %s", want, got)
	}
}
