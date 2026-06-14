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

// TestAnchorWorkerFinalOnCancel: a head appended just before cancel is still
// anchored by the final-on-shutdown anchor.
func TestAnchorWorkerFinalOnCancel(t *testing.T) {
	fa := &fakeAnchorer{}
	dir := t.TempDir()
	aud, _ := audit.NewWriter("inst-1", dir+"/a.wal", []audit.Sink{audit.NewStdoutSink()})
	t.Cleanup(func() { aud.Close() })
	g := &gateway{aud: aud, anchorer: fa, anchorEvery: time.Hour, instance: "inst-1", metrics: metrics.New()} // long interval → no tick fires

	g.aud.Append(audit.Record{SchemaVersion: 1, Event: "e", ID: "1", TS: "t"})
	// wait for the record to drain to the head
	for i := 0; i < 200; i++ {
		if _, c := g.aud.HeadHash(); c == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go g.anchorWorker(ctx, done)
	cancel() // immediate → only the final anchor should fire
	<-done
	if len(fa.calls()) != 1 {
		t.Fatalf("final-on-cancel anchor = %d, want exactly 1", len(fa.calls()))
	}
}
