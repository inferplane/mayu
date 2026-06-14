package filter

import "testing"

type fakeFilter struct{ name string }

func (f fakeFilter) Name() string                { return f.name }
func (f fakeFilter) Mask(t string) (string, int) { return t, 0 }

func TestRegisterAndGet(t *testing.T) {
	Register(fakeFilter{name: "test-filter-a"})
	got, ok := Get("test-filter-a")
	if !ok || got.Name() != "test-filter-a" {
		t.Fatalf("Get(test-filter-a) = %v, %v", got, ok)
	}
	if _, ok := Get("nope"); ok {
		t.Fatal("Get(nope) should be false")
	}
}

func TestDuplicateRegisterPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate Register should panic")
		}
	}()
	Register(fakeFilter{name: "dup"})
	Register(fakeFilter{name: "dup"})
}

func TestNamesSorted(t *testing.T) {
	Register(fakeFilter{name: "zzz-filter"})
	Register(fakeFilter{name: "aaa-filter"})
	names := Names()
	// the two we just added must be present and the slice globally sorted
	for i := 1; i < len(names); i++ {
		if names[i-1] > names[i] {
			t.Fatalf("Names() not sorted: %v", names)
		}
	}
}
