package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/inferplane/inferplane/internal/audit"
	"github.com/inferplane/inferplane/internal/metrics"
)

type fakeAnchorer struct {
	mu     sync.Mutex
	points []audit.AnchorPoint
	failN  int // fail the first N calls
}

func (f *fakeAnchorer) Anchor(_ context.Context, p audit.AnchorPoint) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failN > 0 {
		f.failN--
		return context.DeadlineExceeded // any error
	}
	f.points = append(f.points, p)
	return nil
}
func (f *fakeAnchorer) calls() []audit.AnchorPoint {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]audit.AnchorPoint(nil), f.points...)
}

func newAnchorGateway(t *testing.T, fa audit.Anchorer) *gateway {
	t.Helper()
	dir := t.TempDir()
	aud, err := audit.NewWriter("inst-1", dir+"/a.wal", []audit.Sink{audit.NewStdoutSink()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { aud.Close() })
	return &gateway{aud: aud, anchorer: fa, anchorEvery: 10 * time.Millisecond, instance: "inst-1", metrics: metrics.New()}
}

func TestAnchorWorkerAnchorsOnTick(t *testing.T) {
	fa := &fakeAnchorer{}
	g := newAnchorGateway(t, fa)
	g.aud.Append(audit.Record{SchemaVersion: 1, Event: "e", ID: "1", TS: "t"})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go g.anchorWorker(ctx, done)
	deadline := time.After(3 * time.Second)
	for len(fa.calls()) == 0 {
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatal("no anchor within 3s")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	<-done
	got := fa.calls()
	if got[0].Instance != "inst-1" || got[0].Count < 1 || got[0].HeadHash == "" {
		t.Fatalf("anchor wrong: %+v", got[0])
	}
}

// TestAnchorWorkerRetriesOnFailure: a transient failure does not advance the
// success cursor, so the same head is retried (eventually anchored).
func TestAnchorWorkerRetriesOnFailure(t *testing.T) {
	fa := &fakeAnchorer{failN: 2}
	g := newAnchorGateway(t, fa)
	g.aud.Append(audit.Record{SchemaVersion: 1, Event: "e", ID: "1", TS: "t"})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go g.anchorWorker(ctx, done)
	deadline := time.After(3 * time.Second)
	for len(fa.calls()) == 0 {
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatal("failed anchor was not retried to success within 3s")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	<-done
}

// TestFinalAnchorWitnessesDrainedHead: finalAnchor (the shutdown path, run AFTER
// the audit writer drains) anchors the final head exactly once; a second call is
// a no-op (count not advanced) so it never re-PUTs an already-anchored WORM key.
func TestFinalAnchorWitnessesDrainedHead(t *testing.T) {
	fa := &fakeAnchorer{}
	dir := t.TempDir()
	aud, _ := audit.NewWriter("inst-1", dir+"/a.wal", []audit.Sink{audit.NewStdoutSink()})
	t.Cleanup(func() { aud.Close() })
	g := &gateway{aud: aud, anchorer: fa, anchorEvery: time.Hour, instance: "inst-1", metrics: metrics.New()}

	g.aud.Append(audit.Record{SchemaVersion: 1, Event: "e", ID: "1", TS: "t"})
	for i := 0; i < 200; i++ {
		if _, c := g.aud.HeadHash(); c == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	g.finalAnchor()
	if len(fa.calls()) != 1 || fa.calls()[0].Count != 1 {
		t.Fatalf("final anchor = %+v, want exactly 1 at count 1", fa.calls())
	}
	g.finalAnchor() // unchanged head → no re-anchor (no duplicate WORM key)
	if len(fa.calls()) != 1 {
		t.Fatalf("final anchor re-anchored an unchanged head: %d calls", len(fa.calls()))
	}
}
