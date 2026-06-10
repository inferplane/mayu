package audit

import (
	"crypto/sha256"
	"encoding/hex"
)

const genesisHash = "sha256:genesis"

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
}

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
		flushedAll := true
		for _, s := range w.sinks {
			if err := s.Write(canon); err != nil {
				if s.Required() {
					writeFailuresTotal.Add(1)
					bufferedRecords.Add(1)
					flushedAll = false
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
	}
}

// Close drains the queue and closes the WAL.
func (w *Writer) Close() error {
	close(w.queue)
	<-w.done
	return w.wal.Close()
}
