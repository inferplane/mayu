package adminapi

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/inferplane/inferplane/internal/audit"
	"github.com/inferplane/inferplane/internal/bodystore"
	"github.com/inferplane/inferplane/internal/principal"
)

func testBodyKey(seed byte) [32]byte {
	var k [32]byte
	for i := range k {
		k[i] = seed
	}
	return k
}

func newTestBodyRecorder(t *testing.T) *bodystore.Recorder {
	t.Helper()
	store, err := bodystore.OpenSQLite(filepath.Join(t.TempDir(), "bodies.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	rec := bodystore.NewRecorder(store, testBodyKey(1), time.Hour, 1<<20)
	t.Cleanup(rec.Close)
	return rec
}

func doAsBodies(h *BodiesHandler, id *principal.AdminIdentity, method, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if id != nil {
		req = req.WithContext(principal.WithAdmin(req.Context(), *id))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestBodiesHandler_noIdentityDenied(t *testing.T) {
	h := NewBodiesHandler(newTestBodyRecorder(t), nil)
	rec := doAsBodies(h, nil, "GET", "/admin/bodies/ref-1")
	if rec.Code != 403 {
		t.Fatalf("no identity: got %d, want 403 (fail-closed)", rec.Code)
	}
}

func TestBodiesHandler_GetRoundTripEmitsBodyAccessed(t *testing.T) {
	bodies := newTestBodyRecorder(t)
	ref := bodies.Capture("rec-1", "acme", []byte("the request"), []byte("the response"))
	bodies.Close() // drain before Fetch

	var got []audit.Record
	h := NewBodiesHandler(bodies, func(r audit.Record) { got = append(got, r) })
	id := &principal.AdminIdentity{IsAdmin: true, Subject: "viewer-1", AuthMethod: "oidc"}
	rec := doAsBodies(h, id, "GET", "/admin/bodies/"+ref)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "the request") {
		t.Fatalf("GET body = %d %s", rec.Code, rec.Body.String())
	}
	if len(got) != 1 || got[0].Event != "body_accessed" {
		t.Fatalf("expected one body_accessed record, got %+v", got)
	}
	if got[0].RecordRef == nil || *got[0].RecordRef != "rec-1" {
		t.Fatalf("body_accessed missing/wrong RecordRef: %+v", got[0])
	}
	if got[0].BodyRef != nil {
		t.Fatalf("body_accessed must NEVER carry BodyRef, got %q", *got[0].BodyRef)
	}
}

func TestBodiesHandler_GetDedupesWithinWindow(t *testing.T) {
	bodies := newTestBodyRecorder(t)
	ref := bodies.Capture("rec-1", "acme", []byte("req"), nil)
	bodies.Close()

	var got []audit.Record
	h := NewBodiesHandler(bodies, func(r audit.Record) { got = append(got, r) })
	id := &principal.AdminIdentity{IsAdmin: true, Subject: "viewer-1", AuthMethod: "oidc"}

	doAsBodies(h, id, "GET", "/admin/bodies/"+ref)
	doAsBodies(h, id, "GET", "/admin/bodies/"+ref)
	if len(got) != 1 {
		t.Fatalf("second GET within the dedupe window must not re-emit, got %d records", len(got))
	}

	// A different viewer gets its own record.
	id2 := &principal.AdminIdentity{IsAdmin: true, Subject: "viewer-2", AuthMethod: "oidc"}
	doAsBodies(h, id2, "GET", "/admin/bodies/"+ref)
	if len(got) != 2 {
		t.Fatalf("a different viewer must get its own body_accessed record, got %d", len(got))
	}
}

func TestBodiesHandler_GetAbsentRefReturns410NeverPlaintext(t *testing.T) {
	h := NewBodiesHandler(newTestBodyRecorder(t), nil)
	id := &principal.AdminIdentity{IsAdmin: true, Subject: "viewer-1"}
	rec := doAsBodies(h, id, "GET", "/admin/bodies/never-existed")
	if rec.Code != 410 {
		t.Fatalf("absent ref: got %d, want 410 (tombstone, never 500)", rec.Code)
	}
}

func TestBodiesHandler_DeleteThenGetIsTombstone(t *testing.T) {
	bodies := newTestBodyRecorder(t)
	ref := bodies.Capture("rec-1", "acme", []byte("req"), []byte("resp"))
	bodies.Close()

	var got []audit.Record
	h := NewBodiesHandler(bodies, func(r audit.Record) { got = append(got, r) })
	id := &principal.AdminIdentity{IsAdmin: true, Subject: "admin-1", AuthMethod: "oidc"}

	rec := doAsBodies(h, id, "DELETE", "/admin/bodies/"+ref)
	if rec.Code != 204 {
		t.Fatalf("DELETE = %d, want 204", rec.Code)
	}
	if len(got) != 1 || got[0].Event != "body_deleted" {
		t.Fatalf("expected one body_deleted record, got %+v", got)
	}
	if got[0].RecordRef == nil || *got[0].RecordRef != "rec-1" {
		t.Fatalf("body_deleted missing/wrong RecordRef: %+v", got[0])
	}
	if got[0].BodyRef != nil {
		t.Fatal("body_deleted must NEVER carry BodyRef")
	}

	// idempotent second delete: still 204, still emits (no error on a miss).
	rec2 := doAsBodies(h, id, "DELETE", "/admin/bodies/"+ref)
	if rec2.Code != 204 {
		t.Fatalf("second DELETE = %d, want 204 (idempotent)", rec2.Code)
	}

	// GET after delete: tombstone.
	rec3 := doAsBodies(h, id, "GET", "/admin/bodies/"+ref)
	if rec3.Code != 410 {
		t.Fatalf("GET after DELETE = %d, want 410", rec3.Code)
	}
}

func TestBodiesHandler_EmptyRefRejected(t *testing.T) {
	h := NewBodiesHandler(newTestBodyRecorder(t), nil)
	id := &principal.AdminIdentity{IsAdmin: true, Subject: "viewer-1"}
	rec := doAsBodies(h, id, "GET", "/admin/bodies/")
	if rec.Code != 400 {
		t.Fatalf("empty ref: got %d, want 400", rec.Code)
	}
}

func TestBodiesHandler_RejectsUnsupportedMethod(t *testing.T) {
	h := NewBodiesHandler(newTestBodyRecorder(t), nil)
	id := &principal.AdminIdentity{IsAdmin: true, Subject: "viewer-1"}
	rec := doAsBodies(h, id, "POST", "/admin/bodies/ref-1")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST: got %d, want 405", rec.Code)
	}
}

func TestBodiesHandler_StreamingCaptureResponseIsNull(t *testing.T) {
	bodies := newTestBodyRecorder(t)
	ref := bodies.Capture("rec-stream", "acme", []byte("req only"), nil)
	bodies.Close()

	h := NewBodiesHandler(bodies, nil)
	id := &principal.AdminIdentity{IsAdmin: true, Subject: "viewer-1"}
	rec := doAsBodies(h, id, "GET", "/admin/bodies/"+ref)
	if rec.Code != 200 {
		t.Fatalf("GET = %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"response":null`) {
		t.Fatalf(`expected "response":null for a streaming capture, got %s`, rec.Body.String())
	}
}
