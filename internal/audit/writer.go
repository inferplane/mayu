package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"sync/atomic"

	"github.com/inferplane/inferplane/internal/metrics"
)

const genesisHash = "sha256:genesis"

// chainHead is the immutable {head hash, record count} snapshot published as ONE
// atomic value (ADR-012) so HeadHash() can never return a torn hash/count mix.
type chainHead struct {
	hash  string
	count int64
}

// Writer is the SINGLE writer goroutine for the audit chain. Handlers enqueue
// records via Append (non-blocking); the goroutine assigns prev_hash, persists
// to the WAL, and flushes to sinks — strictly serialized so concurrent
// started/completed records can't race on prev_hash (r4 implementation note).
type Writer struct {
	instance string
	queue    chan Record
	done     chan struct{}
	wal      *WAL
	sinks    []Sink
	prevHash string
	pending  int
	count    int64                     // records written (loop-local; published via head)
	head     atomic.Pointer[chainHead] // race-safe chain-head snapshot for anchoring (ADR-012)
	metrics  *metrics.Metrics          // nil-safe: no-op when nil
}

// HeadHash returns a race-safe snapshot of the current chain head (the last
// durably-written record's hash) and the number of records written. Before the
// first record it is (genesis, 0). Used by the audit anchor worker (ADR-012).
func (w *Writer) HeadHash() (string, int64) {
	h := w.head.Load()
	if h == nil {
		return genesisHash, 0
	}
	return h.hash, h.count
}

// SetMetrics attaches the Prometheus metrics sink. On a required-sink write
// failure the writer bumps audit_write_failures_total; the WAL buffer
// utilization gauge tracks records persisted-but-not-yet-delivered. Must be
// called before the writer is busy (e.g. right after NewWriter). nil disables.
func (w *Writer) SetMetrics(m *metrics.Metrics) { w.metrics = m }

func NewWriter(instance, walPath string, sinks []Sink) (*Writer, error) {
	wal, err := OpenWAL(walPath)
	if err != nil {
		return nil, err
	}
	w := &Writer{
		instance: instance,
		queue:    make(chan Record, 1024),
		done:     make(chan struct{}),
		wal:      wal,
		sinks:    sinks,
		prevHash: genesisHash,
	}
	go w.loop()
	return w, nil
}

// Append enqueues a record. Non-blocking unless the queue is full (back-pressure).
func (w *Writer) Append(rec Record) { w.queue <- rec }

func (w *Writer) loop() {
	defer close(w.done)
	for rec := range w.queue {
		rec.Instance = w.instance
		rec.PrevHash = w.prevHash
		canon, err := rec.Canonical()
		if err != nil {
			writeFailuresTotal.Add(1)
			continue
		}
		// chain advances on the canonical bytes actually emitted
		sum := sha256.Sum256(canon)
		w.prevHash = "sha256:" + hex.EncodeToString(sum[:])

		// durability first: WAL, then sinks. A required-sink failure leaves the
		// record in the WAL for replay (buffer_then_block; §5.4).
		_ = w.wal.Append(canon)
		w.pending++ // records persisted to the WAL, not yet confirmed delivered
		// Publish the chain head ONLY after the record is durable (ADR-012): an
		// anchor must never witness a hash for a record a crash could lose.
		w.count++
		w.head.Store(&chainHead{hash: w.prevHash, count: w.count})
		flushedAll := true
		for _, s := range w.sinks {
			if err := s.Write(canon); err != nil {
				if s.Required() {
					writeFailuresTotal.Add(1)
					bufferedRecords.Add(1)
					flushedAll = false
					w.metrics.IncAuditFailure(s.Name())
				}
			}
		}
		// Truncate ONLY when this record delivered to all required sinks AND it
		// is the sole record in the WAL — i.e. no earlier record is still
		// buffered-undelivered. A later success must never drop an earlier
		// buffered record (buffer_then_block durability). Once any record is
		// buffered, the WAL retains everything until a restart replays it
		// (replay-into-sinks + block-when-full are later milestones).
		if flushedAll && w.pending == 1 {
			_ = w.wal.Truncate()
			w.pending = 0
		}
		// Buffer utilization: records persisted to the WAL but not yet delivered
		// to all required sinks, over the in-flight queue capacity (observability
		// approximation of buffer_then_block pressure, §5.4).
		w.metrics.SetAuditBufferUtilization(float64(w.pending) / float64(cap(w.queue)))
	}
}

// Close drains the queue and closes the WAL.
func (w *Writer) Close() error {
	close(w.queue)
	<-w.done
	return w.wal.Close()
}
