// Package limiter implements instance-local rate limiting (token bucket, TPM/
// RPM, pre-block) and quota windows (daily/monthly, two-phase optimistic check
// + post-debit). LimiterStore is the swappable backend; M5 ships in-memory,
// Redis (shared, multi-replica) is v0.2. Per §5.3 the in-memory limiter is
// per-instance, so multi-replica effective limits scale with replica count —
// documented, not hidden.
package limiter

import (
	"sync"
	"time"
)

type Decision int

const (
	Allow Decision = iota
	Block
)

type LimiterStore interface {
	// AllowRate token-bucket: cost tokens against ratePerMin (refill) with the
	// given burst. Returns false when the bucket lacks `cost` tokens.
	AllowRate(key string, cost, ratePerMin, burst int64) bool
	// CheckQuota optimistic: would used+estimate exceed limit in the window?
	CheckQuota(key string, estimate, limit int64, window time.Duration) Decision
	// DebitQuota records actual usage in the current window.
	DebitQuota(key string, actual int64, window time.Duration)
	// QuotaUsed reports tokens used in the current window (0 if none) — for the
	// quota-utilization observability gauge.
	QuotaUsed(key string, window time.Duration) int64
	// RateUsed reports how much of the bucket's burst capacity is currently
	// consumed (0 if the bucket has never been touched) — a read-only
	// projection of the same refill AllowRate computes, for usage reporting;
	// never writes back to the bucket (unlike AllowRate, which always
	// refills/debits on every call).
	RateUsed(key string, ratePerMin, burst int64) int64
	// AdjustRate corrects an AllowRate charge after actual usage is known
	// (post-response true-up, e.g. TPM billed on a coarse pre-request byte
	// estimate but settled against real token counts). delta is added to the
	// bucket: positive credits back an over-charge, negative debits an
	// under-charge. The result is capped at burst on the high side but is
	// deliberately NOT floored at zero on the low side — a large negative
	// correction is allowed to push the bucket into debt that only refill
	// time repays, so a chronic under-estimate can't be laundered into a
	// free true-up every time. A no-op if the bucket has never been touched
	// (nothing to correct — the request never actually charged this key).
	AdjustRate(key string, delta, burst int64)
}

type bucket struct {
	tokens float64
	last   time.Time
	// ratePerMin/burst are remembered from the last AllowRate so the sweep can
	// project whether this bucket has refilled to capacity WITHOUT the caller's
	// parameters in hand. They are the enforcement inputs for this key, stable
	// for as long as the team's policy is.
	ratePerMin int64
	burst      int64
}

type quotaWin struct {
	used      int64
	windowEnd time.Time
}

// sweepEvery amortizes the reclaim scan over calls, same rationale (and value)
// as budget.sweepEvery: deterministic so a test can pin it, and it cannot
// disagree with the maps because the maps are the only structures there are.
const sweepEvery = 256

// defaultMaxEntries caps live entries (buckets + quota windows) per store.
// Keys are per (team) AND per (key_id) — governance.go builds "rate:<team>",
// "tpm:<team>", "quota:<key_id>", … — so an unbounded map grows with the
// number of DISTINCT authenticated keys ever seen inside one refill window,
// which for a long-lived gateway is attacker-influenced. Reaching this cap
// means ~100k such keys were live at once, far past the node-local profile.
const defaultMaxEntries = 100_000

type Memory struct {
	mu         sync.Mutex
	buckets    map[string]*bucket
	quotas     map[string]*quotaWin
	now        func() time.Time
	maxEntries int
	sweepTick  int
	rejected   int64
}

func NewMemory() *Memory {
	return &Memory{
		buckets:    map[string]*bucket{},
		quotas:     map[string]*quotaWin{},
		now:        time.Now,
		maxEntries: defaultMaxEntries,
	}
}

// Rejections reports how many store OPERATIONS were refused because the store
// was at capacity. Non-zero means some request was rate-limited or quota-denied
// by the capacity fail-safe rather than by a real limit — the same
// operator-visible condition budget.Memory.Rejections exposes, and the same
// reason it is a counter and not a metric (a key-dimensioned label is
// forbidden — see internal/CLAUDE.md Invariants).
func (m *Memory) Rejections() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rejected
}

// admit reports whether a NEW entry may be created, sweeping first and
// counting a rejection if not. Caller holds mu.
//
// Fail-closed on a new key, never on an existing one — budget.Memory's posture,
// for the same reason: at capacity the store cannot enforce the limit it was
// asked to enforce, and an unbounded map ends in an OOM that takes the whole
// data plane (every team) down, which is strictly worse than a 429 for one new
// key. An entry already being tracked is always honoured, so a bucket refilling
// or a quota window rolling over is never refused.
func (m *Memory) admit(t time.Time) bool {
	if len(m.buckets)+len(m.quotas) < m.maxEntries {
		return true
	}
	m.sweepReclaimable(t) // last chance: reclaim what carries no state
	if len(m.buckets)+len(m.quotas) < m.maxEntries {
		return true
	}
	m.rejected++
	return false
}

// tick runs the amortized sweep. Caller holds mu.
func (m *Memory) tick(t time.Time) {
	m.sweepTick++
	if m.sweepTick >= sweepEvery {
		m.sweepTick = 0
		m.sweepReclaimable(t)
	}
}

// sweepReclaimable deletes every entry whose removal is unobservable:
//
//   - a token bucket that has refilled to burst — AllowRate creates an absent
//     bucket with exactly `tokens: burst`, so a full bucket and a missing one
//     decide identically. A bucket in DEBT (negative tokens, from AdjustRate)
//     is never swept: dropping it would launder the debt into a free reset.
//   - a quota window that has already ended — curWindow discards it on the next
//     touch anyway.
//
// Caller holds mu.
func (m *Memory) sweepReclaimable(t time.Time) {
	for k, b := range m.buckets {
		if b.ratePerMin <= 0 {
			continue // never enforced; nothing to project
		}
		tokens := b.tokens + t.Sub(b.last).Seconds()*(float64(b.ratePerMin)/60.0)
		if tokens >= float64(b.burst) {
			delete(m.buckets, k)
		}
	}
	for k, q := range m.quotas {
		if !t.Before(q.windowEnd) {
			delete(m.quotas, k)
		}
	}
}

func (m *Memory) AllowRate(key string, cost, ratePerMin, burst int64) bool {
	if ratePerMin <= 0 {
		return true // unlimited
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.now()
	m.tick(t)
	b := m.buckets[key]
	if b == nil {
		if !m.admit(t) {
			return false
		}
		b = &bucket{tokens: float64(burst), last: t}
		m.buckets[key] = b
	}
	b.ratePerMin, b.burst = ratePerMin, burst
	// refill at ratePerMin/60 tokens per second, capped at burst
	elapsed := t.Sub(b.last).Seconds()
	b.tokens += elapsed * (float64(ratePerMin) / 60.0)
	if b.tokens > float64(burst) {
		b.tokens = float64(burst)
	}
	b.last = t
	if b.tokens >= float64(cost) {
		b.tokens -= float64(cost)
		return true
	}
	return false
}

func (m *Memory) CheckQuota(key string, estimate, limit int64, window time.Duration) Decision {
	if limit <= 0 {
		return Allow
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	q := m.curWindow(key, window)
	if q == nil {
		return Block // at capacity with a real limit to enforce: fail closed
	}
	if q.used+estimate > limit {
		return Block
	}
	return Allow
}

func (m *Memory) DebitQuota(key string, actual int64, window time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	q := m.curWindow(key, window)
	if q == nil {
		return // at capacity: nothing to record against
	}
	q.used += actual
}

// QuotaUsed reports tokens used in the current window (0 if none/elapsed).
// A pure peek: unlike CheckQuota/DebitQuota it never creates or resets a
// window, so an observability read can neither grow the store nor move an
// enforcement boundary (same posture as RateUsed).
func (m *Memory) QuotaUsed(key string, window time.Duration) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	q := m.quotas[key]
	if q == nil || !m.now().Before(q.windowEnd) {
		return 0 // absent, or the window already elapsed
	}
	return q.used
}

// RateUsed peeks at the bucket's projected token level (same refill math as
// AllowRate) without writing it back, so a status read never perturbs
// enforcement. A never-touched bucket reports 0 used (full capacity).
func (m *Memory) RateUsed(key string, ratePerMin, burst int64) int64 {
	if ratePerMin <= 0 {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.buckets[key]
	if b == nil {
		return 0
	}
	elapsed := m.now().Sub(b.last).Seconds()
	tokens := b.tokens + elapsed*(float64(ratePerMin)/60.0)
	if tokens > float64(burst) {
		tokens = float64(burst)
	}
	used := float64(burst) - tokens
	if used < 0 {
		used = 0
	}
	// Round rather than truncate: the sub-token refill that accrues during
	// the microseconds between a debit and this read would otherwise always
	// bias the reported figure down by one (e.g. 199.9998 -> 199, not 200).
	return int64(used + 0.5)
}

// AdjustRate applies a post-hoc correction directly to a bucket's token
// count — no refill math, deliberately: the caller (Settle) runs moments
// after the AllowRate call it is correcting, so treating this as a pure
// balance adjustment rather than a second time-based refill keeps the two
// operations from double-counting elapsed time. Re-caps at burst on the high
// side, but — unlike AllowRate's refill cap — never floors at zero: a
// bucket driven negative by a debit stays blocked until real refill time
// repays it, which is the point (see the interface doc).
func (m *Memory) AdjustRate(key string, delta, burst int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.buckets[key]
	if b == nil {
		return // never charged; nothing to correct
	}
	b.tokens += float64(delta)
	if b.tokens > float64(burst) {
		b.tokens = float64(burst)
	}
}

// curWindow returns the live window for key, resetting if elapsed. Returns nil
// only when a NEW window would have to be created and the store is at capacity
// (see admit). An existing key's rollover is always honoured. Caller holds mu.
func (m *Memory) curWindow(key string, window time.Duration) *quotaWin {
	t := m.now()
	// Look up BEFORE the amortized sweep: the sweep can delete this key's
	// elapsed window, and that must not turn a rollover (always honoured) into
	// a new-entry admission (refusable at capacity).
	q := m.quotas[key]
	m.tick(t)
	if q == nil || !t.Before(q.windowEnd) {
		if q == nil && !m.admit(t) {
			return nil
		}
		q = &quotaWin{windowEnd: t.Add(window)}
		m.quotas[key] = q
	}
	return q
}

var _ LimiterStore = (*Memory)(nil)
