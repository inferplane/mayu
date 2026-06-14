package audit

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"
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

// flakySink fails the required-sink Write for the first N calls, then succeeds.
type flakySink struct {
	failFirst int
	calls     int
	written   [][]byte
}

func (s *flakySink) Write(rec []byte) error {
	s.calls++
	if s.calls <= s.failFirst {
		return errTestFail
	}
	s.written = append(s.written, append([]byte(nil), rec...))
	return nil
}
func (s *flakySink) Name() string   { return "flaky" }
func (s *flakySink) Required() bool { return true }
func (s *flakySink) Close() error   { return nil }

var errTestFail = errorsNew("sink down")

func errorsNew(s string) error { return &simpleErr{s} }

type simpleErr struct{ s string }

func (e *simpleErr) Error() string { return e.s }

func TestWriterDoesNotDropBufferedRecordOnLaterSuccess(t *testing.T) {
	walPath := filepath.Join(t.TempDir(), "a.wal")
	sink := &flakySink{failFirst: 1} // REC1 fails the required sink, REC2 succeeds
	w, err := NewWriter("inst-1", walPath, []Sink{sink})
	if err != nil {
		t.Fatal(err)
	}
	w.Append(Record{SchemaVersion: 1, Event: "request_started", ID: "01A", Instance: "inst-1"})
	w.Append(Record{SchemaVersion: 1, Event: "request_completed", ID: "01B", Instance: "inst-1"})
	w.Close()

	// The buffered REC1 must still be present in the WAL after REC2 succeeded —
	// a later success must NOT truncate away an earlier undelivered record.
	wal, _ := OpenWAL(walPath)
	defer wal.Close()
	var replayed []string
	wal.Replay(func(rec []byte) error {
		var r Record
		json.Unmarshal(rec, &r)
		replayed = append(replayed, r.ID)
		return nil
	})
	found01A := false
	for _, id := range replayed {
		if id == "01A" {
			found01A = true
		}
	}
	if !found01A {
		t.Fatalf("buffered REC1 (01A) was dropped from WAL; replayed=%v", replayed)
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

// TestHeadHashAdvances pins the ADR-012 chain-head snapshot: genesis before any
// record, then advancing hash + count after each durable Append.
func TestHeadHashAdvances(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWriter("inst-1", dir+"/a.wal", []Sink{NewStdoutSink()})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if h, c := w.HeadHash(); h != genesisHash || c != 0 {
		t.Fatalf("pre-append head = %q,%d want genesis,0", h, c)
	}
	w.Append(Record{SchemaVersion: 1, Event: "request_started", ID: "1", TS: "t"})
	w.Append(Record{SchemaVersion: 1, Event: "request_completed", ID: "2", TS: "t"})
	// poll until both records drained (single writer goroutine)
	var h string
	var c int64
	for i := 0; i < 200; i++ {
		h, c = w.HeadHash()
		if c == 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if c != 2 || h == genesisHash {
		t.Fatalf("after 2 appends head = %q,%d want advanced count 2", h, c)
	}
}

// TestHeadHashRaceClean reads the head concurrently with heavy appends (run
// under -race): the single-atomic snapshot must never tear.
func TestHeadHashRaceClean(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWriter("inst-1", dir+"/a.wal", []Sink{NewStdoutSink()})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	done := make(chan struct{})
	go func() {
		for i := 0; i < 500; i++ {
			h, c := w.HeadHash()
			_ = h
			_ = c
		}
		close(done)
	}()
	for i := 0; i < 500; i++ {
		w.Append(Record{SchemaVersion: 1, Event: "e", ID: "x", TS: "t"})
	}
	<-done
}
