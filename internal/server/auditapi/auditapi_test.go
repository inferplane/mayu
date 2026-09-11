package auditapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inferplane/inferplane/internal/audit"
)

// writeChain produces a real valid hash-chain file with n records.
func writeChain(t *testing.T, n int) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	fs, err := audit.NewFileSink(path, true)
	if err != nil {
		t.Fatal(err)
	}
	w, err := audit.NewWriter("test-instance", filepath.Join(dir, "wal"), []audit.Sink{fs})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		w.Append(audit.Record{
			SchemaVersion: 1, Event: "request_completed",
			ID: "rec" + strings.Repeat("0", 3) + string(rune('0'+i)), TS: "2026-06-14T00:00:00Z",
			Principal: audit.PrincipalRef{KeyID: "ik_x", Team: "demo"},
		})
	}
	w.Close()
	return path
}

func get(t *testing.T, paths []string) (*httptest.ResponseRecorder, response) {
	t.Helper()
	rec := httptest.NewRecorder()
	Handler(paths, nil, "").ServeHTTP(rec, httptest.NewRequest("GET", "/admin/audit/verify", nil))
	var out response
	if rec.Code == 200 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v (%s)", err, rec.Body.String())
		}
	}
	return rec, out
}

func TestVerifyValidChain(t *testing.T) {
	_, out := get(t, []string{writeChain(t, 3)})
	if len(out.Sinks) != 1 || !out.Sinks[0].OK || out.Sinks[0].Records != 3 {
		t.Fatalf("valid chain: %+v", out.Sinks)
	}
	if out.Sinks[0].PartialTail {
		t.Fatal("complete file must not flag partial_tail")
	}
}

func TestVerifyTamperedChain(t *testing.T) {
	path := writeChain(t, 3)
	raw, _ := os.ReadFile(path)
	tampered := bytes.Replace(raw, []byte(`"team":"demo"`), []byte(`"team":"EVIL"`), 1)
	if bytes.Equal(raw, tampered) {
		t.Fatal("tamper target not found")
	}
	os.WriteFile(path, tampered, 0o600)
	_, out := get(t, []string{path})
	if out.Sinks[0].OK || out.Sinks[0].BrokenAt == 0 {
		t.Fatalf("tampered chain must report broken: %+v", out.Sinks[0])
	}
}

func TestVerifyPartialTrailingLine(t *testing.T) {
	path := writeChain(t, 3)
	raw, _ := os.ReadFile(path)
	// Append a half-written record (no trailing newline) — a live writer mid-flush.
	os.WriteFile(path, append(raw, []byte(`{"schema_version":1,"event":"request_comp`)...), 0o600)
	_, out := get(t, []string{path})
	if !out.Sinks[0].OK {
		t.Fatalf("partial tail must verify the complete prefix as OK: %+v", out.Sinks[0])
	}
	if !out.Sinks[0].PartialTail {
		t.Fatal("partial tail must be flagged")
	}
	if out.Sinks[0].Records != 3 {
		t.Fatalf("complete prefix should be 3 records, got %d", out.Sinks[0].Records)
	}
}

func TestVerifyOverCap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.jsonl")
	os.WriteFile(path, bytes.Repeat([]byte("x"), maxVerifyBytes+1), 0o600)
	_, out := get(t, []string{path})
	if out.Sinks[0].OK || !strings.Contains(out.Sinks[0].Reason, "too large") {
		t.Fatalf("over-cap must be refused: %+v", out.Sinks[0])
	}
}

func TestVerifyNonRegularFileSkipped(t *testing.T) {
	dir := t.TempDir() // a directory is not a regular file
	_, out := get(t, []string{dir})
	if out.Sinks[0].OK || !strings.Contains(out.Sinks[0].Reason, "not a regular file") {
		t.Fatalf("non-regular path: %+v", out.Sinks[0])
	}
}

func TestVerifyNoFileSink(t *testing.T) {
	rec, out := get(t, nil)
	if rec.Code != 200 || len(out.Sinks) != 0 {
		t.Fatalf("no file sink must be 200 with empty sinks: code=%d %+v", rec.Code, out.Sinks)
	}
}

func TestVerifyMethodNotAllowed(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler(nil, nil, "").ServeHTTP(rec, httptest.NewRequest("POST", "/admin/audit/verify", nil))
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET" {
		t.Fatalf("POST = %d Allow=%q, want 405 + Allow: GET", rec.Code, rec.Header().Get("Allow"))
	}
}

// fakeReader is an in-memory audit.AnchorReader.
type fakeReader struct {
	p   *audit.AnchorPoint
	err error
}

func (f *fakeReader) Latest(_ context.Context, _ string) (*audit.AnchorPoint, error) {
	return f.p, f.err
}

func getWithReader(t *testing.T, paths []string, r audit.AnchorReader) (*httptest.ResponseRecorder, response) {
	t.Helper()
	rec := httptest.NewRecorder()
	Handler(paths, r, "test-instance").ServeHTTP(rec, httptest.NewRequest("GET", "/admin/audit/verify", nil))
	var out response
	if rec.Code == 200 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v (%s)", err, rec.Body.String())
		}
	}
	return rec, out
}

// headAndCount reads a chain file's per-instance state via audit.Verify — the
// same source of truth the handler uses — so anchors in these tests are
// derived, not hand-computed.
func headAndCount(t *testing.T, path string) (string, int64) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	vr, err := audit.Verify(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	st := vr.Instances["test-instance"]
	return st.HeadHash, st.Count
}

func TestVerifyAnchorMatchAtHead(t *testing.T) {
	path := writeChain(t, 3)
	head, count := headAndCount(t, path)
	_, out := getWithReader(t, []string{path}, &fakeReader{p: &audit.AnchorPoint{Instance: "test-instance", HeadHash: head, Count: count}})
	s := out.Sinks[0]
	if !s.OK || !s.AnchorChecked {
		t.Fatalf("anchored head must verify OK with anchor_checked: %+v", s)
	}
}

func TestVerifyAnchorDetectsTailTruncation(t *testing.T) {
	path := writeChain(t, 3)
	head, count := headAndCount(t, path)
	// Truncate the last record — internal consistency still holds, so before
	// the cross-check this verified OK (the S3 finding).
	data, _ := os.ReadFile(path)
	lines := bytes.Split(bytes.TrimRight(data, "\n"), []byte("\n"))
	if err := os.WriteFile(path, append(bytes.Join(lines[:len(lines)-1], []byte("\n")), '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	_, out := getWithReader(t, []string{path}, &fakeReader{p: &audit.AnchorPoint{Instance: "test-instance", HeadHash: head, Count: count}})
	s := out.Sinks[0]
	if s.OK {
		t.Fatalf("a tail-truncated chain must FAIL the anchor cross-check: %+v", s)
	}
	if !s.AnchorChecked || !strings.Contains(s.Reason, "truncation") {
		t.Fatalf("reason must name truncation with anchor_checked: %+v", s)
	}
}

func TestVerifyAnchorDetectsWholeFileReplacement(t *testing.T) {
	path := writeChain(t, 3)
	head, count := headAndCount(t, path)
	// Replace the whole file with a DIFFERENT freshly generated valid chain
	// (same instance, same record count, different content).
	dir := filepath.Dir(path)
	fs, err := audit.NewFileSink(path, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	w, err := audit.NewWriter("test-instance", filepath.Join(dir, "wal2"), []audit.Sink{fs})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		w.Append(audit.Record{SchemaVersion: 1, Event: "request_completed", ID: "forged", TS: "2026-06-15T00:00:00Z",
			Principal: audit.PrincipalRef{KeyID: "ik_y", Team: "demo"}})
	}
	w.Close()
	_, out := getWithReader(t, []string{path}, &fakeReader{p: &audit.AnchorPoint{Instance: "test-instance", HeadHash: head, Count: count}})
	s := out.Sinks[0]
	if s.OK {
		t.Fatalf("a replaced chain must FAIL the anchor cross-check: %+v", s)
	}
	if !s.AnchorChecked || !strings.Contains(s.Reason, "tampering") {
		t.Fatalf("reason must name tampering with anchor_checked: %+v", s)
	}
}

func TestVerifyAnchorOlderThanTailChecksMidChain(t *testing.T) {
	// Anchor at count=2, chain has 3 records (grew since the anchor): the
	// cross-check recomputes the chain state at record 2 and matches.
	path2 := writeChain(t, 2)
	head2, count2 := headAndCount(t, path2)
	path3 := writeChain(t, 3)
	// Splice: use the 3-record chain but an anchor derived from ITS OWN first
	// two records (the two chains differ — separate writers). So recompute
	// from path3's prefix instead.
	data3, _ := os.ReadFile(path3)
	lines := bytes.Split(bytes.TrimRight(data3, "\n"), []byte("\n"))
	prefix := append(bytes.Join(lines[:2], []byte("\n")), '\n')
	vr, err := audit.Verify(bytes.NewReader(prefix))
	if err != nil {
		t.Fatal(err)
	}
	st := vr.Instances["test-instance"]
	_, out := getWithReader(t, []string{path3}, &fakeReader{p: &audit.AnchorPoint{Instance: "test-instance", HeadHash: st.HeadHash, Count: st.Count}})
	s := out.Sinks[0]
	if !s.OK || !s.AnchorChecked {
		t.Fatalf("a chain that GREW past a valid anchor must still verify OK: %+v", s)
	}
	_ = head2
	_ = count2
}

func TestVerifyAnchorReaderErrorIsNotTamperEvidence(t *testing.T) {
	path := writeChain(t, 3)
	_, out := getWithReader(t, []string{path}, &fakeReader{err: context.DeadlineExceeded})
	s := out.Sinks[0]
	if !s.OK || s.AnchorChecked {
		t.Fatalf("an unreachable anchor store must not fail a clean chain (and must not claim anchor_checked): %+v", s)
	}
}

func TestVerifyNoAnchorYetIsNotTamperEvidence(t *testing.T) {
	path := writeChain(t, 3)
	_, out := getWithReader(t, []string{path}, &fakeReader{p: nil})
	s := out.Sinks[0]
	if !s.OK || s.AnchorChecked {
		t.Fatalf("no anchor witnessed yet must not fail a clean chain: %+v", s)
	}
}
