// Package bodystore implements D4 (ADR-018): opt-in request/response body
// capture for the admin console's Logs drawer. Bodies live OUTSIDE the audit
// hash chain (a separate mutable, deletable, TTL/size-capped, encrypted
// store) — the chain only ever carries an opaque body_ref. Two backends
// (SQLite default / Postgres for HA) share this file's contract; neither
// needs a lease or fencing (unlike analytics Mode B) because every replica
// mints its own collision-free ULID ref and purge deletes are idempotent.
package bodystore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/inferplane/inferplane/pkg/ulid"
)

// ErrGone means the ref never existed, was purged/erased, or failed to
// decrypt — callers (the admin body-fetch endpoint) map this to a 410
// tombstone response, never a 500 and never a plaintext fallback (fail-closed).
var ErrGone = errors.New("bodystore: body purged, erased, or unavailable")

// Row is the ciphertext-in/ciphertext-out shape both backends store. Encryption
// happens in the Recorder's worker (crypto.go), never in a Store implementation.
type Row struct {
	Ref, RecordID, Team           string
	CreatedTS, ExpiresTS          string
	Size                          int64
	WrappedKeyNonce, WrappedKeyCT []byte
	ReqNonce, ReqCT               []byte
	RespNonce, RespCT             []byte // nil = response not captured (e.g. streaming)
}

// Store is implemented by the sqlite and postgres backends.
type Store interface {
	Put(ctx context.Context, row Row) error
	// Get returns ErrGone if ref is absent (never found, purged, or erased).
	Get(ctx context.Context, ref string) (Row, error)
	// Delete is a hard, idempotent delete — no error if ref is already absent.
	Delete(ctx context.Context, ref string) error
	// Purge deletes expired rows (expires_ts <= now), then — if the store's
	// total body size still exceeds maxBytes — deletes the oldest remaining
	// rows until it doesn't. Returns the number of rows deleted.
	Purge(ctx context.Context, now time.Time, maxBytes int64) (int, error)
	Close() error
}

// Body is a decrypted record returned by Fetch.
type Body struct {
	Ref, RecordID, Team  string
	Request              []byte
	Response             []byte // nil = not captured (e.g. a streaming response)
	CreatedTS, ExpiresTS string
}

// captureJob is the copy-free handoff from the request path to the worker:
// req/resp are the SAME slices the ingress handler read (RawBody /
// resp.RawBody), never mutated after this point (they are forwarded verbatim
// then discarded) — so passing them by reference to the worker for async
// encryption is safe without an extra copy.
type captureJob struct {
	ref, recordID, team  string
	req, resp            []byte
	createdTS, expiresTS string
}

// recorderQueueCap bounds the decoupling buffer between the request path and
// the encryption+write worker. Bodies are megabyte-scale, so this is a small
// cap; Capture DROPS (never blocks) when full — never adds request latency.
// ponytail: fixed cap; make configurable if drops show up in practice.
const recorderQueueCap = 64

// Recorder is the request-path-facing handle: Capture is cheap and never
// blocks (size check + non-blocking enqueue); encryption and the store write
// happen on a dedicated worker goroutine.
type Recorder struct {
	store        Store
	masterKey    [32]byte
	ttl          time.Duration
	maxBodyBytes int64

	ch   chan captureJob
	done chan struct{}
	once sync.Once
}

// NewRecorder starts the worker goroutine. Close drains the queue before
// returning.
func NewRecorder(store Store, masterKey [32]byte, ttl time.Duration, maxBodyBytes int64) *Recorder {
	r := &Recorder{
		store: store, masterKey: masterKey, ttl: ttl, maxBodyBytes: maxBodyBytes,
		ch: make(chan captureJob, recorderQueueCap), done: make(chan struct{}),
	}
	go r.worker()
	return r
}

// Capture enqueues a copy-free capture job and returns the minted body_ref, or
// "" if the body was dropped (oversize, or the queue is full) — a "" ref means
// the caller must NOT set audit.Record.BodyRef (nothing will ever be stored
// for it). req is required; resp is nil for a streaming response (request-only
// capture, §4.7's stated streaming limitation).
func (r *Recorder) Capture(recordID, team string, req, resp []byte) (ref string) {
	if r == nil {
		return ""
	}
	size := int64(len(req) + len(resp))
	if size > r.maxBodyBytes {
		fmt.Fprintf(os.Stderr, "inferplane: body capture dropped (oversize, %d bytes) for record %s\n", size, recordID)
		return ""
	}
	now := time.Now().UTC()
	job := captureJob{
		ref: ulid.New(), recordID: recordID, team: team, req: req, resp: resp,
		createdTS: now.Format(time.RFC3339Nano), expiresTS: now.Add(r.ttl).Format(time.RFC3339Nano),
	}
	select {
	case r.ch <- job:
		return job.ref
	default:
		fmt.Fprintf(os.Stderr, "inferplane: body capture dropped (queue full) for record %s\n", recordID)
		return ""
	}
}

func (r *Recorder) worker() {
	defer close(r.done)
	for job := range r.ch {
		env, err := sealEnvelope(r.masterKey, job.req, job.resp)
		if err != nil {
			fmt.Fprintln(os.Stderr, "inferplane: body encrypt failed (dropped):", err)
			continue
		}
		row := Row{
			Ref: job.ref, RecordID: job.recordID, Team: job.team,
			CreatedTS: job.createdTS, ExpiresTS: job.expiresTS,
			Size:            int64(len(job.req) + len(job.resp)),
			WrappedKeyNonce: env.wrappedKey.nonce, WrappedKeyCT: env.wrappedKey.ct,
			ReqNonce: env.req.nonce, ReqCT: env.req.ct,
		}
		if env.resp != nil {
			row.RespNonce, row.RespCT = env.resp.nonce, env.resp.ct
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err = r.store.Put(ctx, row)
		cancel()
		if err != nil {
			fmt.Fprintln(os.Stderr, "inferplane: body store write failed (dropped):", err)
		}
	}
}

// Close drains the queue (finishing in-flight encrypt+write work) and stops
// the worker. Idempotent. Does NOT close the underlying Store — the caller
// closes that separately, after Close returns.
func (r *Recorder) Close() {
	if r == nil {
		return
	}
	r.once.Do(func() { close(r.ch) })
	<-r.done
}

// Fetch decrypts and returns the body for ref. Any failure — absent, purged,
// erased, or a decrypt error — returns ErrGone (fail-closed: never a plaintext
// fallback, never distinguishes the reason to the caller).
func (r *Recorder) Fetch(ctx context.Context, ref string) (Body, error) {
	row, err := r.store.Get(ctx, ref)
	if err != nil {
		return Body{}, ErrGone
	}
	env := envelope{
		wrappedKey: sealed{nonce: row.WrappedKeyNonce, ct: row.WrappedKeyCT},
		req:        sealed{nonce: row.ReqNonce, ct: row.ReqCT},
	}
	if row.RespCT != nil {
		env.resp = &sealed{nonce: row.RespNonce, ct: row.RespCT}
	}
	req, resp, err := openEnvelope(r.masterKey, env)
	if err != nil {
		return Body{}, ErrGone
	}
	return Body{
		Ref: row.Ref, RecordID: row.RecordID, Team: row.Team,
		Request: req, Response: resp,
		CreatedTS: row.CreatedTS, ExpiresTS: row.ExpiresTS,
	}, nil
}

// Erase hard-deletes ref. Idempotent (deleting an absent ref is not an error).
func (r *Recorder) Erase(ctx context.Context, ref string) error {
	return r.store.Delete(ctx, ref)
}

// Meta returns the record ID a body_ref is tied to, WITHOUT decrypting the
// body — used by the admin body-delete path so its body_deleted audit event
// can carry RecordRef without paying for a decrypt it discards. ErrGone if
// ref is absent.
func (r *Recorder) Meta(ctx context.Context, ref string) (recordID string, err error) {
	row, err := r.store.Get(ctx, ref)
	if err != nil {
		return "", ErrGone
	}
	return row.RecordID, nil
}
