package audit

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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

func TestVerifyAcceptsInstanceRestartSegments(t *testing.T) {
	// Two instances writing to the same file: each starts its own genesis-
	// anchored chain. A restart (new instance) must verify OK, not BROKEN.
	var buf bytes.Buffer
	w1, _ := NewWriter("inst-A", filepath.Join(t.TempDir(), "a.wal"), []Sink{NewWriterSink("b", &buf, true)})
	w1.Append(Record{SchemaVersion: 1, Event: "request_started", ID: "01A"})
	w1.Append(Record{SchemaVersion: 1, Event: "request_completed", ID: "01B"})
	w1.Close()
	w2, _ := NewWriter("inst-B", filepath.Join(t.TempDir(), "b.wal"), []Sink{NewWriterSink("b", &buf, true)})
	w2.Append(Record{SchemaVersion: 1, Event: "request_started", ID: "01C"})
	w2.Close()

	res, err := Verify(strings.NewReader(buf.String()))
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.Records != 3 {
		t.Fatalf("instance-segmented chain must verify OK across restart: %+v", res)
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

func TestVerifyReportsPerInstanceHeadAndCount(t *testing.T) {
	buf := writeChain(t) // 3 records, one instance "inst-1"
	res, err := Verify(strings.NewReader(buf.String()))
	if err != nil {
		t.Fatal(err)
	}
	st, ok := res.Instances["inst-1"]
	if !ok {
		t.Fatal("Instances missing \"inst-1\"")
	}
	if st.Count != 3 {
		t.Errorf("Count = %d, want 3", st.Count)
	}
	if st.HeadHash == "" || st.HeadHash == genesisHash {
		t.Errorf("HeadHash = %q, want the real chain head", st.HeadHash)
	}
}

func TestVerifyTracksInstancesSeparatelyAcrossRestart(t *testing.T) {
	var buf bytes.Buffer
	w1, _ := NewWriter("inst-A", filepath.Join(t.TempDir(), "a.wal"), []Sink{NewWriterSink("b", &buf, true)})
	w1.Append(Record{SchemaVersion: 1, Event: "request_started", ID: "01A"})
	w1.Append(Record{SchemaVersion: 1, Event: "request_completed", ID: "01B"})
	w1.Close()
	w2, _ := NewWriter("inst-B", filepath.Join(t.TempDir(), "b.wal"), []Sink{NewWriterSink("b", &buf, true)})
	w2.Append(Record{SchemaVersion: 1, Event: "request_started", ID: "01C"})
	w2.Close()

	res, err := Verify(strings.NewReader(buf.String()))
	if err != nil {
		t.Fatal(err)
	}
	if res.Instances["inst-A"].Count != 2 {
		t.Errorf("inst-A count = %d, want 2", res.Instances["inst-A"].Count)
	}
	if res.Instances["inst-B"].Count != 1 {
		t.Errorf("inst-B count = %d, want 1", res.Instances["inst-B"].Count)
	}
}

func TestHeadAtCountMatchesVerifysFinalHead(t *testing.T) {
	buf := writeChain(t)
	full, err := Verify(strings.NewReader(buf.String()))
	if err != nil {
		t.Fatal(err)
	}
	want := full.Instances["inst-1"]
	got, found, err := HeadAtCount(strings.NewReader(buf.String()), "inst-1", want.Count)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("want found=true when the count exists in the stream")
	}
	if got != want.HeadHash {
		t.Errorf("HeadAtCount = %q, want %q (Verify's own final head)", got, want.HeadHash)
	}
}

func TestHeadAtCountFindsAMidChainCheckpoint(t *testing.T) {
	buf := writeChain(t) // 3 records
	// A checkpoint at count=2 must equal the head hash immediately after the
	// SECOND record — recomputed independently of the file's current tail.
	got, found, err := HeadAtCount(strings.NewReader(buf.String()), "inst-1", 2)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("want found=true")
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	sum := sha256.Sum256([]byte(lines[1]))
	want := "sha256:" + hex.EncodeToString(sum[:])
	if got != want {
		t.Errorf("HeadAtCount(2) = %q, want %q", got, want)
	}
}

func TestHeadAtCountNotFoundWhenChainIsShorterThanAnchor(t *testing.T) {
	buf := writeChain(t) // 3 records
	// An anchor witnessing count=10 for a chain that only has 3 records must
	// report "not found" — the truncation signal an anchor exists to catch.
	_, found, err := HeadAtCount(strings.NewReader(buf.String()), "inst-1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Fatal("a chain shorter than the anchored count must report found=false")
	}
}

func TestHeadAtCountNotFoundOnBrokenChainBeforeReachingCount(t *testing.T) {
	buf := writeChain(t)
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	lines[0] = strings.Replace(lines[0], `"01A"`, `"XXX"`, 1)
	tampered := strings.Join(lines, "\n") + "\n"
	_, found, err := HeadAtCount(strings.NewReader(tampered), "inst-1", 3)
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Fatal("a chain broken before the target count must report found=false, not a wrong hash")
	}
}
