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

// TestExtraNumberPrecision pins that a large int64 in an unknown field
// round-trips exactly. Decoding via float64 would corrupt this value;
// UseNumber in jsonSemanticEqual (and json.RawMessage in extra) is the
// safeguard.
func TestExtraNumberPrecision(t *testing.T) {
	in := []byte(`{"known":"a","big_count":9007199254740993}`)
	var s sample
	extra, err := unmarshalWithExtra(in, &s, "known")
	if err != nil {
		t.Fatal(err)
	}
	s.Extra = extra
	out, err := marshalWithExtra(s, s.Extra)
	if err != nil {
		t.Fatal(err)
	}
	assertJSONSemanticEqual(t, in, out)
}

// TestExtraNestedStructures pins that an unknown field holding nested
// arrays-of-objects (including null) round-trips intact.
func TestExtraNestedStructures(t *testing.T) {
	in := []byte(`{"known":"a","items":[{"a":[1,2,{"b":"c"}]},{"d":null}]}`)
	var s sample
	extra, err := unmarshalWithExtra(in, &s, "known")
	if err != nil {
		t.Fatal(err)
	}
	s.Extra = extra
	out, err := marshalWithExtra(s, s.Extra)
	if err != nil {
		t.Fatal(err)
	}
	assertJSONSemanticEqual(t, in, out)
}

// TestExtraEmptyPassthrough pins that input with only known fields yields
// a nil extra map, and marshalWithExtra(v, nil) reproduces the input.
func TestExtraEmptyPassthrough(t *testing.T) {
	in := []byte(`{"known":"a"}`)
	var s sample
	extra, err := unmarshalWithExtra(in, &s, "known")
	if err != nil {
		t.Fatal(err)
	}
	if extra != nil {
		t.Fatalf("expected nil extra, got %v", extra)
	}
	s.Extra = extra
	out, err := marshalWithExtra(s, s.Extra)
	if err != nil {
		t.Fatal(err)
	}
	assertJSONSemanticEqual(t, in, out)
}

// TestExtraKnownKeyNotOverlaid pins the guard: when extra incorrectly
// holds a known key with a different value, the typed field wins in the
// marshaled output and the stale extra entry is dropped.
func TestExtraKnownKeyNotOverlaid(t *testing.T) {
	s := sample{Known: "typed"}
	extra := map[string]json.RawMessage{"known": json.RawMessage(`"stale"`)}
	out, err := marshalWithExtra(s, extra)
	if err != nil {
		t.Fatal(err)
	}
	assertJSONSemanticEqual(t, []byte(`{"known":"typed"}`), out)
}

func TestExtraRejectsCaseCollision(t *testing.T) {
	// Adversarial: a case-variant of a known key would populate the typed
	// field (Go decodes case-insensitively) AND survive into Extra, emitting
	// duplicate keys on re-marshal — a parser-differential smuggling vector.
	in := []byte(`{"known":"cheap","Known":"expensive"}`)
	var s sample
	_, err := unmarshalWithExtra(in, &s, "known")
	if err == nil {
		t.Fatal("expected case-collision to be rejected, got nil error")
	}
}

func TestExtraAllowsDistinctKeys(t *testing.T) {
	// Non-colliding unknown keys must still pass (regression guard).
	in := []byte(`{"known":"a","unrelated":"b"}`)
	var s sample
	extra, err := unmarshalWithExtra(in, &s, "known")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := extra["unrelated"]; !ok {
		t.Fatal("distinct unknown key should be preserved in extra")
	}
}

// assertJSONSemanticEqual: 키 순서 무시, 숫자 정밀도 보존 비교.
func assertJSONSemanticEqual(t *testing.T, want, got []byte) {
	t.Helper()
	if !jsonSemanticEqual(want, got) {
		t.Fatalf("JSON mismatch\nwant: %s\ngot:  %s", want, got)
	}
}
