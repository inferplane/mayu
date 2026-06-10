package router

import (
	"testing"
	"time"
)

func TestBreakerOpensAfterConsecutiveFailures(t *testing.T) {
	b := newBreaker(3, time.Second)
	now := time.Unix(1_700_000_000, 0)
	b.now = func() time.Time { return now }
	if !b.Allow("p") {
		t.Fatal("closed initially")
	}
	b.RecordFailure("p")
	b.RecordFailure("p")
	if !b.Allow("p") {
		t.Fatal("2 failures < threshold, still closed")
	}
	b.RecordFailure("p") // 3rd → open
	if b.Allow("p") {
		t.Fatal("should be open after 3 consecutive failures")
	}
	now = now.Add(2 * time.Second) // backoff elapsed → half-open
	if !b.Allow("p") {
		t.Fatal("half-open after backoff")
	}
	b.RecordSuccess("p") // close
	if !b.Allow("p") {
		t.Fatal("closed after success")
	}
}
