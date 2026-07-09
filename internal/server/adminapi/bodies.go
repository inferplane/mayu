package adminapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/inferplane/inferplane/internal/audit"
	"github.com/inferplane/inferplane/internal/bodystore"
	"github.com/inferplane/inferplane/internal/principal"
	"github.com/inferplane/inferplane/pkg/ulid"
)

// bodyAccessDedupeWindow bounds how often the SAME (viewer, ref) pair emits a
// fresh body_accessed record — §4.7 anti-flood: a drawer left open or
// repeated polling must not grow the audit chain unboundedly.
const bodyAccessDedupeWindow = 5 * time.Minute

// BodiesHandler serves /admin/bodies/{ref} (D4, ADR-018): full-admin-only
// (mounted with requireAdmin in server.go) fetch/erase of a captured body.
// GET emits a body_accessed record (deduped); DELETE emits body_deleted.
// Both events carry RecordRef (the request_completed record the body belongs
// to) and NEVER BodyRef — enforced structurally: this file never sets
// audit.Record.BodyRef, only RecordRef (§4.7 anti-recursion).
type BodiesHandler struct {
	rec  *bodystore.Recorder
	emit func(audit.Record)

	mu   sync.Mutex
	seen map[string]time.Time // "<viewer>\x00<ref>" -> last body_accessed emit
	now  func() time.Time
}

// NewBodiesHandler builds the handler. emit (nil-safe) receives
// body_accessed / body_deleted records.
func NewBodiesHandler(rec *bodystore.Recorder, emit func(audit.Record)) *BodiesHandler {
	return &BodiesHandler{rec: rec, emit: emit, seen: map[string]time.Time{}, now: time.Now}
}

func (h *BodiesHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id, ok := principal.AdminFrom(r.Context())
	if !ok {
		http.Error(w, `{"error":"no admin identity"}`, http.StatusForbidden)
		return
	}
	ref := strings.TrimPrefix(r.URL.Path, "/admin/bodies/")
	if ref == "" {
		http.Error(w, `{"error":"body ref required"}`, http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodGet:
		h.get(w, r, id, ref)
	case http.MethodDelete:
		h.delete(w, r, id, ref)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *BodiesHandler) get(w http.ResponseWriter, r *http.Request, id principal.AdminIdentity, ref string) {
	body, err := h.rec.Fetch(r.Context(), ref)
	if err != nil {
		// Absent, purged, erased, or undecryptable — all the same tombstone to
		// the caller (fail-closed: never distinguish why, never 500).
		http.Error(w, `{"error":"body purged, erased, or unavailable"}`, http.StatusGone)
		return
	}
	if h.shouldEmitAccess(id.Subject, ref) {
		h.adminEvent("body_accessed", id, body.RecordID)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"record_id":  body.RecordID,
		"request":    rawOrString(body.Request),
		"response":   rawOrString(body.Response), // null when not captured (streaming)
		"created_ts": body.CreatedTS,
		"expires_ts": body.ExpiresTS,
	})
}

func (h *BodiesHandler) delete(w http.ResponseWriter, r *http.Request, id principal.AdminIdentity, ref string) {
	// Best-effort metadata lookup (no decryption) so the body_deleted event
	// can carry RecordRef; a miss here just means the ref was already gone —
	// Erase below is still idempotent and still emits the event.
	recordID, _ := h.rec.Meta(r.Context(), ref)
	if err := h.rec.Erase(r.Context(), ref); err != nil {
		http.Error(w, `{"error":"delete failed"}`, http.StatusInternalServerError)
		return
	}
	h.adminEvent("body_deleted", id, recordID)
	w.WriteHeader(http.StatusNoContent)
}

// shouldEmitAccess reports whether a body_accessed record should be emitted
// for this (viewer, ref) pair right now, and records the attempt either way.
func (h *BodiesHandler) shouldEmitAccess(viewer, ref string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	key := viewer + "\x00" + ref
	now := h.now()
	if last, ok := h.seen[key]; ok && now.Sub(last) < bodyAccessDedupeWindow {
		return false
	}
	h.seen[key] = now
	// Opportunistic prune, bounding memory for long-running instances with
	// heavy Logs-drawer traffic. ponytail: O(n) scan on a cache miss; revisit
	// with a proper eviction structure if this measurably shows up.
	if len(h.seen) > 10000 {
		for k, t := range h.seen {
			if now.Sub(t) >= bodyAccessDedupeWindow {
				delete(h.seen, k)
			}
		}
	}
	return true
}

func (h *BodiesHandler) adminEvent(event string, id principal.AdminIdentity, recordID string) {
	if h.emit == nil {
		return
	}
	sub, method := id.Subject, id.AuthMethod
	rec := audit.Record{
		SchemaVersion: 1,
		Event:         event,
		ID:            ulid.New(),
		TS:            time.Now().UTC().Format(time.RFC3339Nano),
		Principal:     audit.PrincipalRef{User: &sub, AuthMethod: &method},
		Request:       audit.RequestRef{Ingress: "admin"},
	}
	if recordID != "" {
		rec.RecordRef = &recordID
	}
	h.emit(rec)
}

// rawOrString renders captured body bytes as parsed JSON when possible (the
// common case — these are JSON API bodies) and falls back to a plain string
// otherwise, so a malformed/non-JSON capture can never break the response
// encoder. nil (not captured) renders as JSON null.
func rawOrString(b []byte) any {
	if b == nil {
		return nil
	}
	if json.Valid(b) {
		return json.RawMessage(b)
	}
	return string(b)
}
