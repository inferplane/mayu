package piimask

import (
	"strings"
	"testing"

	"github.com/inferplane/inferplane/internal/filter"
)

func TestMaskEachDetector(t *testing.T) {
	m := New(Options{}) // all on by default
	cases := []struct {
		in, want string
	}{
		{"reach me at john.doe@example.com please", "reach me at ‹EMAIL› please"},
		{"card 4242 4242 4242 4242 ok", "card ‹CARD› ok"}, // Luhn-valid Visa test number
		{"ssn 123-45-6789 here", "ssn ‹SSN› here"},
		{"server 192.168.1.100 down", "server ‹IP› down"},
	}
	for _, c := range cases {
		got, n := m.Mask(c.in)
		if got != c.want {
			t.Errorf("Mask(%q) = %q, want %q", c.in, got, c.want)
		}
		if n != 1 {
			t.Errorf("Mask(%q) redactions = %d, want 1", c.in, n)
		}
	}
}

func TestNonLuhnCardNotMasked(t *testing.T) {
	m := New(Options{})
	// 16 digits that FAIL Luhn must NOT be masked (cuts false positives).
	in := "order 1234 5678 9012 3456 shipped"
	got, n := m.Mask(in)
	if strings.Contains(got, "‹CARD›") || n != 0 {
		t.Fatalf("non-Luhn 16-digit string masked: %q (n=%d)", got, n)
	}
}

func TestNoPIIUnchanged(t *testing.T) {
	m := New(Options{})
	in := "the quick brown fox jumps over 42 lazy dogs"
	got, n := m.Mask(in)
	if got != in || n != 0 {
		t.Fatalf("clean text changed: %q (n=%d)", got, n)
	}
}

func TestMultipleRedactionsCounted(t *testing.T) {
	m := New(Options{})
	got, n := m.Mask("a@b.com and c@d.org")
	if n != 2 || strings.Count(got, "‹EMAIL›") != 2 {
		t.Fatalf("Mask = %q (n=%d), want 2 emails", got, n)
	}
}

func TestPerDetectorToggle(t *testing.T) {
	// email off, everything else default-on
	m := New(Options{DisableEmail: true})
	got, n := m.Mask("x@y.com")
	if got != "x@y.com" || n != 0 {
		t.Fatalf("disabled email detector still masked: %q (n=%d)", got, n)
	}
}

// TestOverMaskingIsKnown pins the documented over-masking: a dotted-quad in
// prose masks as ‹IP› (regex + over-mask, safe side). Asserted so it is known,
// not surprising (ADR-009).
func TestOverMaskingIsKnown(t *testing.T) {
	m := New(Options{})
	got, _ := m.Mask("upgrade to version 10.20.30.40 today")
	if !strings.Contains(got, "‹IP›") {
		t.Fatalf("expected dotted-quad to over-mask as ‹IP›: %q", got)
	}
}

func TestRegisteredAsPIIMask(t *testing.T) {
	f, ok := filter.Get("pii-mask")
	if !ok {
		t.Fatal("pii-mask not registered in the filter registry")
	}
	if f.Name() != "pii-mask" {
		t.Fatalf("Name() = %q", f.Name())
	}
}
