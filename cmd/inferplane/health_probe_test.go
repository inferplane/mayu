package main

import (
	"context"
	"iter"
	"sync"
	"testing"
	"time"

	"github.com/inferplane/inferplane/internal/live"
	"github.com/inferplane/inferplane/internal/server/configapi"
	"github.com/inferplane/inferplane/pkg/schema"
	"github.com/inferplane/inferplane/providers"
)

// fakeHealthProvider implements providers.HealthChecker with a scripted
// result and a call counter, mirroring anchor_test.go's fakeAnchorer shape.
type fakeHealthProvider struct {
	mu     sync.Mutex
	result providers.HealthResult
	calls  int
}

func (f *fakeHealthProvider) Name() string               { return "fake" }
func (f *fakeHealthProvider) Models() []schema.ModelInfo { return nil }
func (f *fakeHealthProvider) Complete(context.Context, *providers.ProxyRequest) (*providers.ProxyResponse, error) {
	return nil, nil
}
func (f *fakeHealthProvider) Stream(context.Context, *providers.ProxyRequest) (iter.Seq2[*providers.StreamEvent, error], error) {
	return nil, nil
}
func (f *fakeHealthProvider) HealthCheck(context.Context) providers.HealthResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.result
}
func (f *fakeHealthProvider) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// noHealthCheckProvider does NOT implement providers.HealthChecker.
type noHealthCheckProvider struct{}

func (noHealthCheckProvider) Name() string               { return "no-health" }
func (noHealthCheckProvider) Models() []schema.ModelInfo { return nil }
func (noHealthCheckProvider) Complete(context.Context, *providers.ProxyRequest) (*providers.ProxyResponse, error) {
	return nil, nil
}
func (noHealthCheckProvider) Stream(context.Context, *providers.ProxyRequest) (iter.Seq2[*providers.StreamEvent, error], error) {
	return nil, nil
}

func TestHealthProbeWorker_ProbesOnTick(t *testing.T) {
	fp := &fakeHealthProvider{result: providers.HealthResult{OK: true, LatencyMS: 3, Detail: "ok"}}
	holder := &live.Holder{}
	holder.Swap(live.NewState(map[string]providers.Provider{"acme": fp}, nil, nil, nil))

	g := &gateway{holder: holder, healthStore: configapi.NewHealthStore(), healthProbeEvery: 10 * time.Millisecond}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go g.healthProbeWorker(ctx, done)

	deadline := time.After(3 * time.Second)
	for {
		if snap := g.healthStore.Snapshot(); len(snap) > 0 {
			break
		}
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatal("no probe result within 3s")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	<-done

	snap := g.healthStore.Snapshot()
	rec, ok := snap["acme"]
	if !ok || !rec.OK || rec.LatencyMS != 3 || rec.Detail != "ok" {
		t.Fatalf("recorded health status wrong: %+v", snap)
	}
	if fp.callCount() < 1 {
		t.Fatal("HealthCheck was never called")
	}
}

func TestHealthProbeWorker_SkipsNonHealthCheckProvider(t *testing.T) {
	holder := &live.Holder{}
	holder.Swap(live.NewState(map[string]providers.Provider{"nohc": noHealthCheckProvider{}}, nil, nil, nil))

	g := &gateway{holder: holder, healthStore: configapi.NewHealthStore(), healthProbeEvery: 10 * time.Millisecond}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go g.healthProbeWorker(ctx, done)
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	if snap := g.healthStore.Snapshot(); len(snap) != 0 {
		t.Fatalf("a provider without HealthChecker must never appear in the snapshot, got %+v", snap)
	}
}
