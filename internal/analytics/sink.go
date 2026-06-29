package analytics

import (
	"encoding/json"
	"sync"

	"github.com/inferplane/inferplane/internal/audit"
)

// sinkQueueCap bounds the decoupling buffer between the audit writer's fan-out
// goroutine and analytics ingestion. When full, Write DROPS (never blocks) —
// the audit chain and data plane must never wait on the derived index. Dropped
// records are recovered on the next boot by Replay (idempotent), so the index
// is eventually consistent with the authoritative audit log.
const sinkQueueCap = 4096

type sink struct {
	ix   *Index
	ch   chan audit.Record
	done chan struct{}
	once sync.Once
}

// NewSink adapts the index to the audit.Sink fan-out, ingesting ASYNCHRONOUSLY
// on a dedicated worker goroutine so SQLite latency (disk I/O, checkpoint, lock)
// can never stall the single audit-writer goroutine (isolation invariant, §4).
// It is best-effort (Required()==false). Close() drains the queue and stops the
// worker; the assembly closes the Index afterwards.
func NewSink(ix *Index) audit.Sink {
	s := &sink{ix: ix, ch: make(chan audit.Record, sinkQueueCap), done: make(chan struct{})}
	go s.worker()
	return s
}

func (s *sink) worker() {
	for rec := range s.ch {
		_ = s.ix.Ingest(rec) // best-effort; a derived-index error never propagates
	}
	close(s.done)
}

func (s *sink) Write(line []byte) error {
	var rec audit.Record
	if json.Unmarshal(line, &rec) != nil {
		return nil // malformed → skip, never error the chain
	}
	if !billable(rec) {
		return nil // don't spend queue slots on non-billable events
	}
	// Non-blocking enqueue: drop rather than block the audit fan-out goroutine.
	select {
	case s.ch <- rec:
	default: // queue full → drop (recovered on next boot via Replay)
	}
	return nil
}

func (s *sink) Name() string   { return "analytics" }
func (s *sink) Required() bool { return false }

// Close stops the worker after it drains the queue. Idempotent. Must be called
// AFTER the audit writer's Close (so all in-flight records have been enqueued)
// and BEFORE the Index is closed.
func (s *sink) Close() error {
	s.once.Do(func() { close(s.ch) })
	<-s.done
	return nil
}
