package tier

import (
	"math/bits"
	"time"

	"github.com/inferplane/inferplane/api/v1alpha1"
)

// UtilizedPercent floors (spent+outstanding)*100/limit without overflowing,
// saturating at 100. Invalid accounting (negative values or nonpositive limit)
// conservatively reports exhaustion. Callers handle unlimited rules separately.
func UtilizedPercent(spent, outstanding, limit int64) int {
	if spent < 0 || outstanding < 0 || limit <= 0 || spent >= limit || outstanding >= limit-spent {
		return 100
	}
	// The guards prove this sum fits int64 and the quotient fits [0,99].
	hi, lo := bits.Mul64(uint64(spent+outstanding), 100)
	percent, _ := bits.Div64(hi, lo, uint64(limit))
	return int(percent)
}

// WindowKeyForPeriod follows the caller's timezone: UTC for control-plane
// evaluation, the configured budget timezone for local evaluation. Empty period
// retains the default monthly window. Schema validation rejects unknown periods.
func WindowKeyForPeriod(now time.Time, period v1alpha1.BudgetPeriod) string {
	if period == v1alpha1.PeriodCalendarDay {
		return now.Format("2006-01-02")
	}
	return now.Format("2006-01")
}
