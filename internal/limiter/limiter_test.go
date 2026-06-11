package limiter

import (
	"testing"
	"time"
)

func TestRateLimitBlocksOverBurst(t *testing.T) {
	l := NewMemory()
	// 60 rpm = 1/s, burst 2. clock injected for determinism.
	now := time.Unix(1_700_000_000, 0)
	l.now = func() time.Time { return now }
	key := "team:rps"
	if !l.AllowRate(key, 1, 60, 2) { // burst 2 → first allowed
		t.Fatal("first request should be allowed")
	}
	if !l.AllowRate(key, 1, 60, 2) {
		t.Fatal("second (within burst) should be allowed")
	}
	if l.AllowRate(key, 1, 60, 2) {
		t.Fatal("third should be blocked (burst exhausted, no refill yet)")
	}
	// advance 1s → 1 token refilled at 1/s
	now = now.Add(time.Second)
	if !l.AllowRate(key, 1, 60, 2) {
		t.Fatal("after 1s refill, should be allowed")
	}
}

func TestQuotaTwoPhase(t *testing.T) {
	l := NewMemory()
	now := time.Unix(1_700_000_000, 0)
	l.now = func() time.Time { return now }
	key := "team:daily"
	limit := int64(1000)
	// optimistic check with estimate 800 → ok (0 used)
	if d := l.CheckQuota(key, 800, limit, 24*time.Hour); d != Allow {
		t.Fatalf("first check: %v", d)
	}
	l.DebitQuota(key, 800, 24*time.Hour) // actual 800 used
	// next check estimate 300 → 800+300=1100 > 1000 → Block
	if d := l.CheckQuota(key, 300, limit, 24*time.Hour); d != Block {
		t.Fatalf("over-limit check should block: %v", d)
	}
	// estimate 100 → 800+100=900 ≤ 1000 → Allow
	if d := l.CheckQuota(key, 100, limit, 24*time.Hour); d != Allow {
		t.Fatalf("under-limit check should allow: %v", d)
	}
}

func TestQuotaWindowResets(t *testing.T) {
	l := NewMemory()
	now := time.Unix(1_700_000_000, 0)
	l.now = func() time.Time { return now }
	key := "team:win"
	l.DebitQuota(key, 1000, time.Hour)
	if d := l.CheckQuota(key, 1, 1000, time.Hour); d != Block {
		t.Fatal("at limit, should block")
	}
	now = now.Add(2 * time.Hour) // window elapsed
	if d := l.CheckQuota(key, 500, 1000, time.Hour); d != Allow {
		t.Fatal("after window reset, should allow")
	}
}
