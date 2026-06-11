package budget

import (
	"testing"
	"time"
)

func TestBudgetTwoPhaseMicros(t *testing.T) {
	b := NewMemory()
	now := time.Unix(1_700_000_000, 0)
	b.now = func() time.Time { return now }
	key := "team:month"
	limit := int64(5_000_000) // 5 USD in µUSD
	if d := b.Check(key, 4_000_000, limit, 30*24*time.Hour); d != Allow {
		t.Fatalf("under: %v", d)
	}
	b.Debit(key, 4_000_000, 30*24*time.Hour)
	if d := b.Check(key, 2_000_000, limit, 30*24*time.Hour); d != Block {
		t.Fatalf("4M+2M>5M should block: %v", d)
	}
	if d := b.Check(key, 500_000, limit, 30*24*time.Hour); d != Allow {
		t.Fatalf("4M+0.5M≤5M should allow: %v", d)
	}
}
