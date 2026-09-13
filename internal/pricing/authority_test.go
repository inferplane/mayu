package pricing

import (
	"math"
	"testing"
)

func TestAuthorityBoundCoversEveryTokenClassAndRounding(t *testing.T) {
	table := New(OnMissingBlock, map[Key]Rate{{"p", "m"}: {
		InputPerMTok: 3, OutputPerMTok: 7, CacheReadPerMTok: 1,
		CacheWrite5mPerMTok: 11, CacheWrite1hPerMTok: 13,
	}})
	bound, err := table.AuthorityBound("p", "m", 123456, 65432)
	if err != nil || bound <= 0 {
		t.Fatalf("bound=%d error=%v", bound, err)
	}
	for _, u := range []Usage{
		{Input: 123456, Output: 65432},
		{CacheRead: 123456, Output: 65432},
		{CacheWrite5m: 123456, Output: 65432},
		{CacheWrite1h: 123456, Output: 65432},
		{Input: 123456, CacheRead: 123456, CacheWrite5m: 123456, CacheWrite1h: 123456, Output: 65432},
	} {
		actual, missing := table.CostUSDMicros("p", "m", u)
		if missing || actual > bound {
			t.Fatalf("bound %d did not cover cost %d", bound, actual)
		}
	}
}

func TestAuthorityBoundRejectsUnknownAndOverflow(t *testing.T) {
	table := New(OnMissingAllow, map[Key]Rate{{"p", "m"}: {InputPerMTok: math.MaxInt64}})
	for _, item := range []struct {
		model   string
		in, out int64
	}{{"missing", 1, 1}, {"m", 0, 1}, {"m", 1, -1}, {"m", math.MaxInt64, math.MaxInt64}} {
		if _, err := table.AuthorityBound("p", item.model, item.in, item.out); err == nil {
			t.Fatalf("unbounded authority accepted: %+v", item)
		}
	}
	free := New(OnMissingBlock, map[Key]Rate{{"p", "free"}: {}})
	if got, err := free.AuthorityBound("p", "free", 100, 100); err != nil || got != 0 {
		t.Fatalf("explicit free rate = %d %v", got, err)
	}
}
