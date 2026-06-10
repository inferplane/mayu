package audit

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func writeChain(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	w, _ := NewWriter("inst-1", filepath.Join(t.TempDir(), "a.wal"), []Sink{NewWriterSink("buf", &buf, true)})
	w.Append(Record{SchemaVersion: 1, Event: "request_started", ID: "01A"})
	w.Append(Record{SchemaVersion: 1, Event: "request_completed", ID: "01B"})
	w.Append(Record{SchemaVersion: 1, Event: "request_started", ID: "01C"})
	w.Close()
	return &buf
}

func TestVerifyAcceptsIntactChain(t *testing.T) {
	buf := writeChain(t)
	res, err := Verify(strings.NewReader(buf.String()))
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.Records != 3 {
		t.Fatalf("intact chain rejected: %+v", res)
	}
}

func TestVerifyDetectsTampering(t *testing.T) {
	buf := writeChain(t)
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	// tamper: flip a byte in the first record's team-less body by replacing the ID.
	// The first record (01A) is line index 0; its bytes feed record 2's prev_hash,
	// so the break surfaces at the NEXT record (1-based BrokenAt == 2).
	lines[0] = strings.Replace(lines[0], `"01A"`, `"XXX"`, 1)
	tampered := strings.Join(lines, "\n") + "\n"
	res, _ := Verify(strings.NewReader(tampered))
	if res.OK {
		t.Fatal("tampering with a chained record must fail verification")
	}
	if res.BrokenAt != 2 {
		t.Fatalf("expected break detected at record 2 (the one after the tampered one), got %d", res.BrokenAt)
	}
}
