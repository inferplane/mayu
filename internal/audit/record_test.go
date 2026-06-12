package audit

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
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

// preAuthMethodFixture is a raw JSONL chain captured byte-for-byte from the
// audit writer BEFORE the auth_method field existed (P2 gate r1: the
// mixed-version test must use real pre-change bytes, not re-serialized ones).
const preAuthMethodFixture = `{"schema_version":1,"event":"request_started","id":"01PREAUTHMETHODFIXTURE0000","ts":"2026-06-12T00:00:00Z","instance":"fixture-instance","principal":{"key_id":"ik_fixture","team":"demo"},"request":{"ingress":"anthropic","model_requested":"claude-test","stream":false},"trace_id":null,"prev_hash":"sha256:genesis"}
{"schema_version":1,"event":"request_started","id":"01PREAUTHMETHODFIXTURE0001","ts":"2026-06-12T00:00:00Z","instance":"fixture-instance","principal":{"key_id":"ik_fixture","team":"demo"},"request":{"ingress":"anthropic","model_requested":"claude-test","stream":false},"trace_id":null,"prev_hash":"sha256:c0cf159852fb1346c479a7211e1a6a773cb9600da3583c2c519f96d3b59e342d"}
{"schema_version":1,"event":"request_started","id":"01PREAUTHMETHODFIXTURE0002","ts":"2026-06-12T00:00:00Z","instance":"fixture-instance","principal":{"key_id":"ik_fixture","team":"demo"},"request":{"ingress":"anthropic","model_requested":"claude-test","stream":false},"trace_id":null,"prev_hash":"sha256:6a5fe318a9ed166a156d94c4a84fd58cf5add3de85b69b7211299da889f3e265"}
`

// TestMixedVersionChainVerifies pins hash-chain compatibility across the
// auth_method addition: pre-change records (raw fixture bytes) and new
// records carrying auth_method coexist in one file and the chain verifies;
// a one-byte tamper in either generation breaks it.
func TestMixedVersionChainVerifies(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "audit.jsonl")
	if err := os.WriteFile(out, []byte(preAuthMethodFixture), 0o600); err != nil {
		t.Fatal(err)
	}

	// Append new-generation records (auth_method set) as a second instance
	// segment, exactly as a post-upgrade gateway restart would.
	fs, err := NewFileSink(out, true)
	if err != nil {
		t.Fatal(err)
	}
	w, err := NewWriter("new-instance", filepath.Join(dir, "wal"), []Sink{fs})
	if err != nil {
		t.Fatal(err)
	}
	sub, method := "user-1", "oidc"
	w.Append(Record{
		SchemaVersion: 1, Event: "admin_key_created",
		ID: "01NEWGENAUTHMETHOD0000", TS: "2026-06-12T01:00:00Z",
		Principal: PrincipalRef{KeyID: "ik_new", Team: "alpha", User: &sub, AuthMethod: &method},
		Request:   RequestRef{Ingress: "admin"},
	})
	w.Close()

	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Verify(bytesReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.Records != 4 {
		t.Fatalf("mixed-version chain: %+v, want OK with 4 records", res)
	}
	if !containsBytes(raw, []byte(`"auth_method":"oidc"`)) {
		t.Fatal("new record missing auth_method")
	}

	// Tamper one byte in the OLD generation: chain must break.
	tampered := replaceOnce(raw, []byte(`"team":"demo"`), []byte(`"team":"DEMO"`))
	tres, err := Verify(bytesReader(tampered))
	if err != nil {
		t.Fatal(err)
	}
	if tres.OK {
		t.Fatal("tampered pre-change record verified OK")
	}
}

func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }
func containsBytes(h, n []byte) bool     { return bytes.Contains(h, n) }
func replaceOnce(b, old, new []byte) []byte {
	return bytes.Replace(b, old, new, 1)
}
