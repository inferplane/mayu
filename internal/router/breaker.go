package router

import (
	"sync"
	"time"
)

// breaker is an instance-local circuit breaker per provider. After
// `threshold` consecutive failures it opens for `baseBackoff`, doubling on
// each subsequent failure while open (capped). half-open after backoff allows
// one trial; success closes, failure re-opens with longer backoff.
type breaker struct {
	mu          sync.Mutex
	threshold   int
	baseBackoff time.Duration
	state       map[string]*brkState
	now         func() time.Time
}

type brkState struct {
	consecutiveFails int
	openUntil        time.Time
	backoff          time.Duration
}

func newBreaker(threshold int, baseBackoff time.Duration) *breaker {
	return &breaker{threshold: threshold, baseBackoff: baseBackoff, state: map[string]*brkState{}, now: time.Now}
}

func (b *breaker) Allow(provider string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.state[provider]
	if s == nil || s.openUntil.IsZero() {
		return true
	}
	return !b.now().Before(s.openUntil) // open window elapsed → half-open (allow a trial)
}

func (b *breaker) RecordFailure(provider string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.state[provider]
	if s == nil {
		s = &brkState{}
		b.state[provider] = s
	}
	s.consecutiveFails++
	if s.consecutiveFails >= b.threshold {
		if s.backoff == 0 {
			s.backoff = b.baseBackoff
		} else {
			s.backoff *= 2
			if s.backoff > 30*time.Second {
				s.backoff = 30 * time.Second
			}
		}
		s.openUntil = b.now().Add(s.backoff)
	}
}

// Retain drops breaker entries whose key is not in keep, so stale state for a
// removed/re-pointed provider does not linger across reloads. Guarded by the
// same mutex as every other breaker op, so it is safe against concurrent
// Allow/RecordResult.
func (b *breaker) Retain(keep map[string]bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for k := range b.state {
		if !keep[k] {
			delete(b.state, k)
		}
	}
}

func (b *breaker) RecordSuccess(provider string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if s := b.state[provider]; s != nil {
		s.consecutiveFails = 0
		s.openUntil = time.Time{}
		s.backoff = 0
	}
}

// State reports the circuit state for metrics: 0=closed, 1=half-open (open
// window elapsed, awaiting a trial), 2=open (within the backoff window).
func (b *breaker) State(provider string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.state[provider]
	if s == nil || s.openUntil.IsZero() {
		return 0 // closed
	}
	if b.now().Before(s.openUntil) {
		return 2 // open
	}
	return 1 // half-open (window elapsed; next call is a trial)
}
