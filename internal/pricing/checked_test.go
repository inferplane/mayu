package pricing

import (
	"math"
	"testing"
)

func TestCheckedCostRejectsClassAndSumOverflow(t *testing.T) {
	table := New(OnMissingBlock, map[Key]Rate{{"p", "m"}: {InputPerMTok: 4_000_000, OutputPerMTok: 4_000_000}})
	for _, u := range []Usage{{Output: 1 << 62}, {Input: math.MaxInt64 / 4, Output: math.MaxInt64 / 4}, {Input: -1}} {
		if _, err := table.CostUSDMicrosChecked("p", "m", u); err == nil {
			t.Fatalf("invalid cost accepted: %+v", u)
		}
	}
	table = New(OnMissingBlock, map[Key]Rate{{"p", "m"}: {InputPerMTok: 500000, OutputPerMTok: 500000}})
	for _, u := range []Usage{{Input: 1, Output: 1}, {Input: 3, Output: 5}, {Input: 10001}} {
		want, _ := table.CostUSDMicros("p", "m", u)
		got, err := table.CostUSDMicrosChecked("p", "m", u)
		if err != nil || got != want {
			t.Fatalf("round-half-even differs: got=%d want=%d err=%v", got, want, err)
		}
	}
}
