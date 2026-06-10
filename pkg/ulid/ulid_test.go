package ulid

import (
	"sort"
	"testing"
	"time"
)

func TestNewIsLexicographicallyTimeOrdered(t *testing.T) {
	t0 := time.UnixMilli(1_000_000_000_000)
	t1 := time.UnixMilli(1_000_000_001_000)
	a := NewAt(t0)
	b := NewAt(t1)
	if !(a < b) {
		t.Fatalf("later timestamp must sort after: %q !< %q", a, b)
	}
	if len(a) != 26 {
		t.Fatalf("ULID must be 26 chars, got %d (%q)", len(a), a)
	}
}

func TestMonotonicWithinSameMillisecond(t *testing.T) {
	ts := time.UnixMilli(1_700_000_000_000)
	g := NewGenerator()
	var ids []string
	for i := 0; i < 1000; i++ {
		ids = append(ids, g.NewAt(ts))
	}
	if !sort.StringsAreSorted(ids) {
		t.Fatal("ids generated in the same millisecond must be monotonically increasing")
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			t.Fatalf("duplicate ULID: %s", id)
		}
		seen[id] = true
	}
}

func TestOnlyCrockfordAlphabet(t *testing.T) {
	id := New()
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	for _, c := range id {
		found := false
		for _, a := range alphabet {
			if c == a {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("char %q not in Crockford base32", c)
		}
	}
}
