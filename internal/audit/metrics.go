package audit

import "sync/atomic"

// M3 keeps audit counters as atomics; the Prometheus /metrics endpoint that
// exposes them is M6 (§6.2). These let M3 tests assert failure/buffer state.
var (
	writeFailuresTotal atomic.Int64 // inferplane_audit_write_failures_total
	bufferedRecords    atomic.Int64 // backs inferplane_audit_buffer_utilization_ratio
)

func WriteFailuresTotal() int64 { return writeFailuresTotal.Load() }
func BufferedRecords() int64    { return bufferedRecords.Load() }
