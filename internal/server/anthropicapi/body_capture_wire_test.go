package anthropicapi

import (
	"bytes"
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inferplane/inferplane/internal/audit"
	"github.com/inferplane/inferplane/internal/bodystore"
)

func testKeyBody(seed byte) [32]byte {
	var k [32]byte
	for i := range k {
		k[i] = seed
	}
	return k
}

func testRecorder(t *testing.T) *bodystore.Recorder {
	t.Helper()
	store, err := bodystore.OpenSQLite(filepath.Join(t.TempDir(), "bodies.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	rec := bodystore.NewRecorder(store, testKeyBody(1), 0, 1<<20)
	t.Cleanup(rec.Close)
	return rec
}

// extractBodyRef pulls the body_ref of the (single) request_completed record
// out of a raw audit JSONL buffer, failing the test if absent.
func extractBodyRef(t *testing.T, raw []byte) string {
	t.Helper()
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if !bytes.Contains(line, []byte(`"event":"request_completed"`)) {
			continue
		}
		i := bytes.Index(line, []byte(`"body_ref":"`))
		if i < 0 {
			t.Fatalf("request_completed record missing body_ref: %s", line)
		}
		rest := line[i+len(`"body_ref":"`):]
		return string(rest[:bytes.IndexByte(rest, '"')])
	}
	t.Fatalf("no request_completed record found: %s", raw)
	return ""
}

// TestMessagesBodyCaptureRoundTrip proves the happy path end to end: the body
// is captured under the SAME ULID as the request_completed record, and is
// fetchable (masked-then-captured, in this unmasked case verbatim) afterward.
func TestMessagesBodyCaptureRoundTrip(t *testing.T) {
	rec := &recProvider{}
	h := NewMessagesHandler(recRouter(rec))
	bodies := testRecorder(t)
	h.SetBodyRecorder(bodies)

	var buf bytes.Buffer
	w, err := audit.NewWriter("i", filepath.Join(t.TempDir(), "a.wal"), []audit.Sink{audit.NewWriterSink("b", &buf, true)})
	if err != nil {
		t.Fatal(err)
	}
	h2 := NewMessagesHandlerWithAudit(recRouter(rec), w)
	h2.SetBodyRecorder(bodies)

	reqBody := `{"model":"m","messages":[{"role":"user","content":"hello"}]}`
	rr := httptest.NewRecorder()
	h2.ServeHTTP(rr, maskedReq("acme", reqBody))
	w.Close()
	bodies.Close() // drain: the async encrypt+write must finish before Fetch

	if rr.Code != 200 {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
	ref := extractBodyRef(t, buf.Bytes())

	got, err := bodies.Fetch(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if got.Team != "acme" {
		t.Fatalf("captured team = %q, want acme", got.Team)
	}
	if string(got.Request) != reqBody {
		t.Fatalf("captured request = %q, want %q", got.Request, reqBody)
	}
	wantResp := `{"id":"x","type":"message","role":"assistant","model":"m","content":[]}`
	if string(got.Response) != wantResp {
		t.Fatalf("captured response = %q, want %q", got.Response, wantResp)
	}
}

// TestMessagesBodyCapture_NilRecorderOmitsBodyRef proves the zero-overhead
// default: with no recorder set, a request_completed record never carries
// body_ref.
func TestMessagesBodyCapture_NilRecorderOmitsBodyRef(t *testing.T) {
	rec := &recProvider{}
	var buf bytes.Buffer
	w, err := audit.NewWriter("i", filepath.Join(t.TempDir(), "a.wal"), []audit.Sink{audit.NewWriterSink("b", &buf, true)})
	if err != nil {
		t.Fatal(err)
	}
	h := NewMessagesHandlerWithAudit(recRouter(rec), w)
	// No SetBodyRecorder call.

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, maskedReq("acme", `{"model":"m","messages":[]}`))
	w.Close()

	if strings.Contains(buf.String(), `"body_ref"`) {
		t.Fatalf("body_ref must be absent with no recorder configured: %s", buf.String())
	}
}

// TestMessagesBodyCapture_StreamingIsRequestOnly proves the stated streaming
// limitation (§4.7): a streaming response is captured request-only — Response
// is nil after Fetch, never a partially-buffered attempt.
func TestMessagesBodyCapture_StreamingIsRequestOnly(t *testing.T) {
	h := NewMessagesHandler(testRouter())
	bodies := testRecorder(t)
	h.SetBodyRecorder(bodies)

	var buf bytes.Buffer
	w, err := audit.NewWriter("i", filepath.Join(t.TempDir(), "a.wal"), []audit.Sink{audit.NewWriterSink("b", &buf, true)})
	if err != nil {
		t.Fatal(err)
	}
	h2 := NewMessagesHandlerWithAudit(testRouter(), w)
	h2.SetBodyRecorder(bodies)

	reqBody := `{"model":"claude-sonnet-4-6","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	rr := httptest.NewRecorder()
	h2.ServeHTTP(rr, allowAll(httptest.NewRequest("POST", "/v1/messages", strings.NewReader(reqBody))))
	w.Close()
	bodies.Close()

	if rr.Code != 200 {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
	ref := extractBodyRef(t, buf.Bytes())
	got, err := bodies.Fetch(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Request) != reqBody {
		t.Fatalf("captured request = %q, want %q", got.Request, reqBody)
	}
	if got.Response != nil {
		t.Fatalf("streaming capture must have nil Response, got %q", got.Response)
	}
}

// TestMessagesBodyCapture_RawBodyPassthroughByteForByte is the required §13
// regression test: body capture (copy-only, off the request path) must never
// mutate the upstream-forwarded request bytes or the client-received response
// bytes, with log_bodies enabled — the cache invariant (§4.4) must survive D4.
func TestMessagesBodyCapture_RawBodyPassthroughByteForByte(t *testing.T) {
	rec := &recProvider{}
	h := NewMessagesHandler(recRouter(rec))
	h.SetBodyRecorder(testRecorder(t))

	reqBody := `{"model":"m","messages":[{"role":"user","content":"byte for byte"}]}`
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, maskedReq("acme", reqBody))
	if rr.Code != 200 {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
	if rec.last == nil {
		t.Fatal("provider not called")
	}
	if string(rec.last.RawBody) != reqBody {
		t.Fatalf("upstream-forwarded RawBody mutated by body capture:\n got %q\nwant %q", rec.last.RawBody, reqBody)
	}
	wantResp := `{"id":"x","type":"message","role":"assistant","model":"m","content":[]}`
	if rr.Body.String() != wantResp {
		t.Fatalf("client-received response mutated by body capture:\n got %q\nwant %q", rr.Body.String(), wantResp)
	}
}
