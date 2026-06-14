package auditapi

import (
	"bytes"
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
	Handler(paths).ServeHTTP(rec, httptest.NewRequest("GET", "/admin/audit/verify", nil))
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
	Handler(nil).ServeHTTP(rec, httptest.NewRequest("POST", "/admin/audit/verify", nil))
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET" {
		t.Fatalf("POST = %d Allow=%q, want 405 + Allow: GET", rec.Code, rec.Header().Get("Allow"))
	}
}
