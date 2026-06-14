package audit

import (
	"context"
	"sync"
	"testing"
)

// fakeAnchorer records calls (test double for the S3 anchorer).
type fakeAnchorer struct {
	mu     sync.Mutex
	points []AnchorPoint
	err    error
}

func (f *fakeAnchorer) Anchor(_ context.Context, p AnchorPoint) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.points = append(f.points, p)
	return nil
}

func (f *fakeAnchorer) calls() []AnchorPoint {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]AnchorPoint(nil), f.points...)
}

func TestFakeAnchorerCaptures(t *testing.T) {
	var a Anchorer = &fakeAnchorer{}
	p := AnchorPoint{Instance: "inst-1", HeadHash: "sha256:abc", Count: 7, TS: "2026-06-14T00:00:00Z"}
	if err := a.Anchor(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	got := a.(*fakeAnchorer).calls()
	if len(got) != 1 || got[0] != p {
		t.Fatalf("anchor not captured: %+v", got)
	}
}
