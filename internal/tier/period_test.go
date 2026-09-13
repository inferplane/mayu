package tier

import (
	"math"
	"testing"
	"time"

	"github.com/inferplane/inferplane/api/v1alpha1"
)

func TestUtilizedPercentExactAndOverflowSafe(t *testing.T) {
	for _, tt := range []struct {
		spent, outstanding, limit int64
		want                      int
	}{
		{0, 0, 100, 0}, {79, 0, 100, 79}, {799, 0, 1000, 79},
		{79, 1, 100, 80}, {99, 1, 100, 100}, {101, 0, 100, 100},
		{math.MaxInt64, math.MaxInt64, math.MaxInt64, 100},
		{math.MaxInt64 - 1, 1, math.MaxInt64, 100},
		{math.MaxInt64 - 1, 0, math.MaxInt64, 99},
		{math.MaxInt64 / 2, math.MaxInt64 / 2, math.MaxInt64, 99},
		{math.MaxInt64 / 2, 0, math.MaxInt64, 49},
		{-1, 0, 100, 100}, {0, -1, 100, 100},
		{0, 0, 0, 100}, {0, 0, -1, 100}, {math.MinInt64, 1, 100, 100},
	} {
		if got := UtilizedPercent(tt.spent, tt.outstanding, tt.limit); got != tt.want {
			t.Errorf("UtilizedPercent(%d,%d,%d)=%d, want %d", tt.spent, tt.outstanding, tt.limit, got, tt.want)
		}
	}
}

func TestWindowKeyForPeriodUsesCallerTimezone(t *testing.T) {
	local := time.FixedZone("budget", -7*60*60)
	now := time.Date(2026, 9, 1, 0, 30, 0, 0, time.UTC).In(local)
	for _, tt := range []struct {
		period v1alpha1.BudgetPeriod
		want   string
	}{
		{v1alpha1.PeriodCalendarDay, "2026-08-31"},
		{v1alpha1.PeriodCalendarMonth, "2026-08"},
		{"", "2026-08"},
	} {
		if got := WindowKeyForPeriod(now, tt.period); got != tt.want {
			t.Errorf("period=%s: %s != %s", tt.period, got, tt.want)
		}
	}
	if WindowKey(now) != "2026-09" {
		t.Fatal("legacy WindowKey changed timezone semantics")
	}
	midnight := time.Date(2026, 9, 1, 0, 0, 0, 0, local)
	before := midnight.Add(-time.Nanosecond)
	if WindowKeyForPeriod(before, v1alpha1.PeriodCalendarDay) == WindowKeyForPeriod(midnight, v1alpha1.PeriodCalendarDay) ||
		WindowKeyForPeriod(before, v1alpha1.PeriodCalendarMonth) == WindowKeyForPeriod(midnight, v1alpha1.PeriodCalendarMonth) {
		t.Fatal("local day/month rollover did not change window key")
	}
	if WindowKeyForPeriod(midnight.Add(24*time.Hour), v1alpha1.PeriodCalendarMonth) != WindowKeyForPeriod(midnight, v1alpha1.PeriodCalendarMonth) {
		t.Fatal("daily rollover reset monthly tier latch")
	}
}
