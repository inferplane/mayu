package audit

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
)

func TestWriterChainsPrevHash(t *testing.T) {
	var buf bytes.Buffer
	wal := filepath.Join(t.TempDir(), "a.wal")
	w, err := NewWriter("inst-1", wal, []Sink{NewWriterSink("buf", &buf, true)})
	if err != nil {
		t.Fatal(err)
	}
	w.Append(Record{SchemaVersion: 1, Event: "request_started", ID: "01A", Instance: "inst-1"})
	w.Append(Record{SchemaVersion: 1, Event: "request_completed", ID: "01B", Instance: "inst-1"})
	w.Close() // flushes the queue

	lines := splitLines(buf.String())
	if len(lines) != 2 {
		t.Fatalf("expected 2 records, got %d", len(lines))
	}
	var first, second Record
	json.Unmarshal([]byte(lines[0]), &first)
	json.Unmarshal([]byte(lines[1]), &second)
	// second.prev_hash must equal sha256 of the first record's canonical bytes
	sum := sha256.Sum256([]byte(lines[0]))
	want := "sha256:" + hex.EncodeToString(sum[:])
	if second.PrevHash != want {
		t.Fatalf("chain broken:\n got: %s\nwant: %s", second.PrevHash, want)
	}
	if first.PrevHash == "" {
		t.Fatal("first record should carry the genesis prev_hash, not empty")
	}
}

func TestWriterSerializesConcurrentAppends(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	sink := NewWriterSink("buf", &lockedWriter{w: &buf, mu: &mu}, true)
	w, _ := NewWriter("i", filepath.Join(t.TempDir(), "a.wal"), []Sink{sink})
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			w.Append(Record{SchemaVersion: 1, Event: "request_started", ID: "x", Instance: "i"})
		}(i)
	}
	wg.Wait()
	w.Close()
	// every record's prev_hash must equal the hash of the literally-preceding
	// line — proving a single writer serialized them with no race.
	lines := splitLines(buf.String())
	for i := 1; i < len(lines); i++ {
		sum := sha256.Sum256([]byte(lines[i-1]))
		want := "sha256:" + hex.EncodeToString(sum[:])
		var rec Record
		json.Unmarshal([]byte(lines[i]), &rec)
		if rec.PrevHash != want {
			t.Fatalf("record %d prev_hash not chained to previous line", i)
		}
	}
}

type lockedWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func splitLines(s string) []string {
	var out []string
	for _, l := range bytes.Split([]byte(s), []byte{'\n'}) {
		if len(l) > 0 {
			out = append(out, string(l))
		}
	}
	return out
}
