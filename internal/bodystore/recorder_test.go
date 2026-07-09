package bodystore

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func testRecorder(t *testing.T) (*Recorder, Store) {
	t.Helper()
	sq, err := OpenSQLite(filepath.Join(t.TempDir(), "bodies.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sq.Close() })
	key := testKey(t, 3)
	rec := NewRecorder(sq, key, time.Hour, 1<<20) // 1 MiB per-body cap
	t.Cleanup(rec.Close)
	return rec, sq
}

func TestRecorder_CaptureAndFetchRoundTrip(t *testing.T) {
	rec, _ := testRecorder(t)
	ref := rec.Capture("rec-1", "acme", []byte("the request"), []byte("the response"))
	if ref == "" {
		t.Fatal("Capture returned empty ref")
	}
	rec.Close() // drain: the async encrypt+write must finish before Fetch

	body, err := rec.Fetch(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if body.RecordID != "rec-1" || body.Team != "acme" ||
		string(body.Request) != "the request" || string(body.Response) != "the response" {
		t.Fatalf("fetched body mismatch: %+v", body)
	}
}

func TestRecorder_RequestOnlyCapture_StreamingResponse(t *testing.T) {
	rec, _ := testRecorder(t)
	ref := rec.Capture("rec-stream", "acme", []byte("the request"), nil)
	rec.Close()

	body, err := rec.Fetch(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if body.Response != nil {
		t.Fatalf("streaming capture must have nil Response, got %q", body.Response)
	}
}

func TestRecorder_OversizeDropped(t *testing.T) {
	sq, err := OpenSQLite(filepath.Join(t.TempDir(), "bodies.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sq.Close() })
	rec := NewRecorder(sq, testKey(t, 3), time.Hour, 4) // tiny 4-byte cap
	t.Cleanup(rec.Close)

	if ref := rec.Capture("rec-big", "acme", []byte("this is way over the cap"), nil); ref != "" {
		t.Fatalf("oversize capture must return empty ref, got %q", ref)
	}
}

func TestRecorder_NilSafe(t *testing.T) {
	var rec *Recorder
	if ref := rec.Capture("x", "y", []byte("z"), nil); ref != "" {
		t.Fatal("nil Recorder.Capture must return empty ref")
	}
	rec.Close() // must not panic
}

func TestRecorder_FetchAbsentReturnsErrGone(t *testing.T) {
	rec, _ := testRecorder(t)
	rec.Close()
	if _, err := rec.Fetch(context.Background(), "never-captured"); err != ErrGone {
		t.Fatalf("Fetch on absent ref = %v, want ErrGone", err)
	}
}

func TestRecorder_FetchWrongMasterKeyFailsClosed(t *testing.T) {
	sq, err := OpenSQLite(filepath.Join(t.TempDir(), "bodies.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sq.Close() })

	writer := NewRecorder(sq, testKey(t, 3), time.Hour, 1<<20)
	ref := writer.Capture("rec-1", "acme", []byte("secret request"), nil)
	writer.Close()

	// A second Recorder, same store, DIFFERENT master key — simulates a
	// rotated/misconfigured key. Fetch must fail closed, never return
	// plaintext, never distinguish the reason.
	reader := NewRecorder(sq, testKey(t, 99), time.Hour, 1<<20)
	if _, err := reader.Fetch(context.Background(), ref); err != ErrGone {
		t.Fatalf("Fetch with wrong master key = %v, want ErrGone", err)
	}
}

func TestRecorder_EraseThenFetchReturnsErrGone(t *testing.T) {
	rec, _ := testRecorder(t)
	ref := rec.Capture("rec-1", "acme", []byte("request"), []byte("response"))
	rec.Close()

	if err := rec.Erase(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	if _, err := rec.Fetch(context.Background(), ref); err != ErrGone {
		t.Fatalf("Fetch after Erase = %v, want ErrGone (tombstone)", err)
	}
	// Erase is idempotent.
	if err := rec.Erase(context.Background(), ref); err != nil {
		t.Fatalf("second Erase must be a no-op, got: %v", err)
	}
}
