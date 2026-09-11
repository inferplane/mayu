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

func TestQuotaUsedReportsCurrentWindow(t *testing.T) {
	m := NewMemory()
	if u := m.QuotaUsed("q:t", time.Hour); u != 0 {
		t.Fatalf("fresh QuotaUsed = %d, want 0", u)
	}
	m.DebitQuota("q:t", 300, time.Hour)
	m.DebitQuota("q:t", 200, time.Hour)
	if u := m.QuotaUsed("q:t", time.Hour); u != 500 {
		t.Fatalf("QuotaUsed = %d, want 500", u)
	}
}

func TestRateUsedPeeksWithoutMutating(t *testing.T) {
	l := NewMemory()
	now := time.Unix(1_700_000_000, 0)
	l.now = func() time.Time { return now }
	key := "key:rate"
	// never touched → 0 used (full capacity), and unlimited (ratePerMin<=0) → 0.
	if u := l.RateUsed(key, 60, 100); u != 0 {
		t.Fatalf("fresh RateUsed = %d, want 0", u)
	}
	if u := l.RateUsed(key, 0, 100); u != 0 {
		t.Fatalf("unlimited RateUsed = %d, want 0", u)
	}
	l.AllowRate(key, 40, 60, 100) // debit 40 of burst 100
	if u := l.RateUsed(key, 60, 100); u != 40 {
		t.Fatalf("RateUsed after debit = %d, want 40", u)
	}
	// a peek must not itself consume any tokens: repeated reads are stable.
	if u := l.RateUsed(key, 60, 100); u != 40 {
		t.Fatalf("RateUsed peek must be idempotent, got %d", u)
	}
	// advance 30s at 60/min=1/s → 30 tokens refill → used drops to 10.
	now = now.Add(30 * time.Second)
	if u := l.RateUsed(key, 60, 100); u != 10 {
		t.Fatalf("RateUsed after refill = %d, want 10", u)
	}
}

func TestAdjustRateCreditsBackAnOvercharge(t *testing.T) {
	l := NewMemory()
	key := "team:tpm"
	l.AllowRate(key, 50, 60, 100) // charged 50 of burst 100 (an estimate)
	l.AdjustRate(key, 30, 100)    // actual usage was 20; credit back the 30-token overcharge
	if u := l.RateUsed(key, 60, 100); u != 20 {
		t.Fatalf("RateUsed after credit = %d, want 20", u)
	}
}

func TestAdjustRateDebitsAnUndercharge(t *testing.T) {
	l := NewMemory()
	key := "team:tpm"
	l.AllowRate(key, 20, 60, 100) // charged 20 of burst 100 (an estimate)
	l.AdjustRate(key, -30, 100)   // actual usage was 50; debit the 30-token undercharge
	if u := l.RateUsed(key, 60, 100); u != 50 {
		t.Fatalf("RateUsed after debit = %d, want 50", u)
	}
}

func TestAdjustRateCapsAtBurstButNeverFloorsAtZero(t *testing.T) {
	l := NewMemory()
	key := "team:tpm"
	l.AllowRate(key, 10, 60, 100) // 90 tokens remain
	l.AdjustRate(key, 1000, 100)  // an over-large credit must still cap at burst
	if u := l.RateUsed(key, 60, 100); u != 0 {
		t.Fatalf("RateUsed after capped credit = %d, want 0 (full bucket)", u)
	}
	l.AdjustRate(key, -500, 100) // a large debit is allowed to drive the bucket into debt
	if u := l.RateUsed(key, 60, 100); u != 500 {
		t.Fatalf("RateUsed after debt-driving debit = %d, want 500 (not floored at burst)", u)
	}
}

func TestAdjustRateNoopOnUntouchedBucket(t *testing.T) {
	l := NewMemory()
	// A key that was never charged (e.g. TPM disabled for this team) must not
	// spring into existence just because Settle tries to correct it.
	l.AdjustRate("never:touched", -50, 100)
	if u := l.RateUsed("never:touched", 60, 100); u != 0 {
		t.Fatalf("RateUsed on an uncreated bucket = %d, want 0", u)
	}
}

// --- bounded-memory tests (the limiter's counterpart to budget.Memory's cap) ---

func TestSweepReclaimsRefilledBucketsButNotDebt(t *testing.T) {
	l := NewMemory()
	now := time.Unix(1_700_000_000, 0)
	l.now = func() time.Time { return now }

	// "full" spends its burst, then sits idle long enough to refill.
	if !l.AllowRate("full", 2, 60, 2) {
		t.Fatal("initial spend should be allowed")
	}
	// "debt" is driven negative by a true-up correction (AdjustRate never
	// floors at zero) — sweeping it would launder the debt into a free reset.
	l.AllowRate("debt", 1, 60, 2)
	l.AdjustRate("debt", -10, 2)

	now = now.Add(time.Hour) // both buckets project past burst, but debt is -10+60 > 2
	l.sweepReclaimable(now)

	if _, ok := l.buckets["full"]; ok {
		t.Error("a bucket refilled to burst should be reclaimed (absent == full)")
	}
	// After an hour a -8 bucket has also refilled past burst, so it is
	// reclaimable too; what must never happen is reclaiming it while still in
	// debt. Re-run the same check on a short elapse.
	l2 := NewMemory()
	n2 := time.Unix(1_700_000_000, 0)
	l2.now = func() time.Time { return n2 }
	l2.AllowRate("debt", 1, 60, 2)
	l2.AdjustRate("debt", -10, 2)
	n2 = n2.Add(time.Second) // -9 + 1 token = still deep in debt
	l2.sweepReclaimable(n2)
	if _, ok := l2.buckets["debt"]; !ok {
		t.Error("a bucket still in debt must NOT be reclaimed")
	}
}

func TestSweepReclaimsElapsedQuotaWindows(t *testing.T) {
	l := NewMemory()
	now := time.Unix(1_700_000_000, 0)
	l.now = func() time.Time { return now }
	l.DebitQuota("k", 5, time.Hour)
	if len(l.quotas) != 1 {
		t.Fatalf("want 1 quota window, got %d", len(l.quotas))
	}
	now = now.Add(2 * time.Hour)
	l.sweepReclaimable(now)
	if len(l.quotas) != 0 {
		t.Errorf("elapsed window should be reclaimed, %d left", len(l.quotas))
	}
}

func TestAtCapacityFailsClosedOnNewKeyAndCountsRejection(t *testing.T) {
	l := NewMemory()
	now := time.Unix(1_700_000_000, 0)
	l.now = func() time.Time { return now }
	l.maxEntries = 2

	// Two live buckets, each mid-burst so neither is reclaimable.
	if !l.AllowRate("a", 2, 60, 2) || !l.AllowRate("b", 2, 60, 2) {
		t.Fatal("first two keys should be admitted")
	}
	if l.AllowRate("c", 1, 60, 2) {
		t.Error("a NEW key at capacity must fail closed (rate-limited), not be admitted")
	}
	if got := l.Rejections(); got != 1 {
		t.Errorf("Rejections() = %d, want 1", got)
	}
	if _, ok := l.buckets["c"]; ok {
		t.Error("a refused key must not be stored")
	}
	// An ALREADY-TRACKED key keeps working: nothing about capacity may change
	// the enforcement answer for a bucket that carries live state.
	now = now.Add(time.Second)
	if !l.AllowRate("a", 1, 60, 2) {
		t.Error("existing key must still be served at capacity")
	}
}

func TestAtCapacityBlocksNewQuotaKey(t *testing.T) {
	l := NewMemory()
	now := time.Unix(1_700_000_000, 0)
	l.now = func() time.Time { return now }
	l.maxEntries = 1
	l.DebitQuota("live", 1, time.Hour)

	if got := l.CheckQuota("other", 1, 100, time.Hour); got != Block {
		t.Errorf("CheckQuota at capacity = %v, want Block (fail closed)", got)
	}
	if got := l.Rejections(); got == 0 {
		t.Error("a refused quota admission should count a rejection")
	}
	// The live key is unaffected, including its own rollover.
	if got := l.CheckQuota("live", 1, 100, time.Hour); got != Allow {
		t.Errorf("live key CheckQuota = %v, want Allow", got)
	}
	now = now.Add(2 * time.Hour)
	if got := l.CheckQuota("live", 1, 100, time.Hour); got != Allow {
		t.Errorf("live key rollover at capacity = %v, want Allow", got)
	}
}

func TestQuotaUsedDoesNotCreateAWindow(t *testing.T) {
	l := NewMemory()
	l.now = func() time.Time { return time.Unix(1_700_000_000, 0) }
	if got := l.QuotaUsed("never-seen", time.Hour); got != 0 {
		t.Errorf("QuotaUsed on an unknown key = %d, want 0", got)
	}
	if len(l.quotas) != 0 {
		t.Errorf("a read-only projection must not grow the store, %d entries created", len(l.quotas))
	}
}

func TestAmortizedSweepRunsWithoutUnboundedGrowth(t *testing.T) {
	l := NewMemory()
	now := time.Unix(1_700_000_000, 0)
	l.now = func() time.Time { return now }
	// Every key spends its whole burst and is then never touched again — the
	// unbounded-growth shape (one bucket per distinct key_id, forever).
	for i := 0; i < sweepEvery*3; i++ {
		key := "k" + time.Duration(i).String()
		l.AllowRate(key, 2, 60, 2)
		now = now.Add(time.Minute) // each prior bucket refills to burst
	}
	if len(l.buckets) > sweepEvery {
		t.Errorf("buckets = %d; the amortized sweep should keep refilled buckets from accumulating", len(l.buckets))
	}
}
