package audit

import (
	"encoding/json"
	"testing"
)

func TestRecordCanonicalIsDeterministic(t *testing.T) {
	r := Record{
		SchemaVersion: 1, Event: "request_completed", ID: "01J", TS: "2026-06-10T00:00:00Z",
		Instance:  "inst-1",
		Principal: PrincipalRef{KeyID: "ik_abc", Team: "platform-eng"},
		Request:   RequestRef{Ingress: "anthropic", ModelRequested: "claude-sonnet-4-6", Provider: "anthropic-direct", Stream: true},
		PrevHash:  "sha256:00",
	}
	a, err := r.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := r.Canonical()
	if string(a) != string(b) {
		t.Fatal("canonical form must be byte-stable across calls")
	}
	var m map[string]any
	if err := json.Unmarshal(a, &m); err != nil {
		t.Fatalf("canonical not valid JSON: %v", err)
	}
	if m["event"] != "request_completed" {
		t.Fatalf("event missing: %v", m["event"])
	}
}

func TestStartedRecordOmitsCompletionFields(t *testing.T) {
	r := Record{SchemaVersion: 1, Event: "request_started", ID: "01J", TS: "t", Instance: "i",
		Principal: PrincipalRef{KeyID: "ik", Team: "t"}, Request: RequestRef{Ingress: "anthropic"}}
	b, _ := r.Canonical()
	s := string(b)
	if contains(s, `"usage"`) || contains(s, `"outcome"`) {
		t.Fatalf("started record must omit usage/outcome: %s", s)
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && (indexOf(s, sub) >= 0) }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
