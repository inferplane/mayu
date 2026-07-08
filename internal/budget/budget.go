// Package budget tracks per-team spend in integer micro-USD (§5.3). Same
// two-phase optimistic-check + post-debit shape as quota, but the unit is
// money (µUSD), fed by the pricing table. In-memory now; Redis v0.2.
package budget

import (
	"sync"
	"time"
)

type Decision int

const (
	Allow Decision = iota
	Block
)

type BudgetStore interface {
	Check(key string, estimateMicros, limitMicros int64, window time.Duration) Decision
	Debit(key string, actualMicros int64, window time.Duration)
	// Spent reports µUSD debited in the current window (0 if none or elapsed).
	// Used for the budget-utilization gauge and alert threshold evaluation
	// (D5b, ADR-017) — mirrors limiter.LimiterStore.QuotaUsed.
	Spent(key string, window time.Duration) int64
}

type win struct {
	spent     int64
	windowEnd time.Time
}

type Memory struct {
	mu  sync.Mutex
	m   map[string]*win
	now func() time.Time
}

func NewMemory() *Memory { return &Memory{m: map[string]*win{}, now: time.Now} }

func (b *Memory) Check(key string, estimateMicros, limitMicros int64, window time.Duration) Decision {
	if limitMicros <= 0 {
		return Allow
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	w := b.cur(key, window)
	if w.spent+estimateMicros > limitMicros {
		return Block
	}
	return Allow
}

func (b *Memory) Debit(key string, actualMicros int64, window time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cur(key, window).spent += actualMicros
}

func (b *Memory) Spent(key string, window time.Duration) int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cur(key, window).spent
}

func (b *Memory) cur(key string, window time.Duration) *win {
	t := b.now()
	w := b.m[key]
	if w == nil || !t.Before(w.windowEnd) {
		w = &win{windowEnd: t.Add(window)}
		b.m[key] = w
	}
	return w
}

var _ BudgetStore = (*Memory)(nil)
