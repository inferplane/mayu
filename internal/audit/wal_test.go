package audit

import (
	"path/filepath"
	"testing"
)

func TestWALAppendAndReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.wal")
	w, err := OpenWAL(path)
	if err != nil {
		t.Fatal(err)
	}
	w.Append([]byte(`{"a":1}`))
	w.Append([]byte(`{"a":2}`))
	w.Close()

	// reopen and replay
	w2, err := OpenWAL(path)
	if err != nil {
		t.Fatal(err)
	}
	defer w2.Close()
	var got []string
	if err := w2.Replay(func(rec []byte) error { got = append(got, string(rec)); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != `{"a":1}` || got[1] != `{"a":2}` {
		t.Fatalf("replay mismatch: %v", got)
	}
}

func TestWALTruncateAfterFlush(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.wal")
	w, _ := OpenWAL(path)
	defer w.Close()
	w.Append([]byte(`{"a":1}`))
	if err := w.Truncate(); err != nil {
		t.Fatal(err)
	}
	var n int
	w.Replay(func([]byte) error { n++; return nil })
	if n != 0 {
		t.Fatalf("truncate should empty the WAL, got %d records", n)
	}
}
