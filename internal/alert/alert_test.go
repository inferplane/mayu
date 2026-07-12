package alert

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func waitForFires(t *testing.T, n *Notifier, want int) []Fire {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		if fires := n.Recent(); len(fires) >= want {
			return fires
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %d fire(s), got %d", want, len(n.Recent()))
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestObserve_FiresOnThresholdCross(t *testing.T) {
	var mu sync.Mutex
	var received []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		json.NewDecoder(r.Body).Decode(&payload)
		mu.Lock()
		received = append(received, payload)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := New(srv.URL, []float64{0.8, 1.0}, time.Second)

	// Below any threshold: no fire.
	n.Observe("teamA", 700_000, 1_000_000)
	time.Sleep(20 * time.Millisecond)
	if len(n.Recent()) != 0 {
		t.Fatalf("expected no fire below threshold, got %d", len(n.Recent()))
	}

	// Cross 0.8.
	n.Observe("teamA", 850_000, 1_000_000)
	fires := waitForFires(t, n, 1)
	if fires[0].Threshold != 0.8 || !fires[0].Delivered {
		t.Fatalf("expected delivered fire at 0.8, got %+v", fires[0])
	}

	// Same ratio again: dedupe, no second fire.
	n.Observe("teamA", 850_000, 1_000_000)
	time.Sleep(20 * time.Millisecond)
	if len(n.Recent()) != 1 {
		t.Fatalf("expected dedupe at same ratio, got %d fires", len(n.Recent()))
	}

	// Cross 1.0.
	n.Observe("teamA", 1_050_000, 1_000_000)
	fires = waitForFires(t, n, 2)
	if fires[0].Threshold != 1.0 {
		t.Fatalf("expected newest fire at 1.0, got %+v", fires[0])
	}

	mu.Lock()
	gotPayloads := len(received)
	mu.Unlock()
	if gotPayloads != 2 {
		t.Fatalf("expected 2 webhook POSTs, got %d", gotPayloads)
	}
}

func TestObserve_JumpOverAllThresholdsFiresOnlyHighest(t *testing.T) {
	var mu sync.Mutex
	var thresholds []float64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		json.NewDecoder(r.Body).Decode(&payload)
		mu.Lock()
		thresholds = append(thresholds, payload["threshold"].(float64))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := New(srv.URL, []float64{0.8, 1.0}, time.Second)
	// 0% -> 120% in one request: crosses both 0.8 and 1.0 simultaneously.
	n.Observe("teamA", 1_200_000, 1_000_000)
	fires := waitForFires(t, n, 1)

	if len(fires) != 1 || fires[0].Threshold != 1.0 {
		t.Fatalf("jump-over must fire only the highest threshold, got %+v", fires)
	}
	mu.Lock()
	got := append([]float64{}, thresholds...)
	mu.Unlock()
	if len(got) != 1 || got[0] != 1.0 {
		t.Fatalf("webhook must be POSTed exactly once, for 1.0, got %v", got)
	}
}

func TestObserve_RatioDropRearms(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := New(srv.URL, []float64{0.8}, time.Second)
	n.Observe("teamA", 900_000, 1_000_000) // crosses 0.8
	waitForFires(t, n, 1)

	// Window rolled over: ratio drops below the last-fired threshold.
	n.Observe("teamA", 100_000, 1_000_000)
	time.Sleep(20 * time.Millisecond)
	if len(n.Recent()) != 1 {
		t.Fatalf("expected no fire on ratio drop, got %d", len(n.Recent()))
	}

	// Crossing 0.8 again after re-arm fires again.
	n.Observe("teamA", 900_000, 1_000_000)
	waitForFires(t, n, 2)
}

func TestObserve_FiredMapEvictsUnarmedTeams(t *testing.T) {
	n := New("http://example.invalid", []float64{0.8}, time.Second)
	// A team observed below every threshold must not grow the fired map —
	// bounds memory for high team churn (nothing to remember for it).
	n.Observe("quiet-team", 100, 1000) // ratio 0.1, never crosses 0.8
	n.mu.Lock()
	_, tracked := n.fired["quiet-team"]
	n.mu.Unlock()
	if tracked {
		t.Fatal("a team that never crossed a threshold must not be tracked in fired")
	}
}

func TestObserve_NoLimitOrThresholds(t *testing.T) {
	n := New("http://example.invalid", []float64{0.8}, time.Second)
	n.Observe("teamA", 500, 0) // limit<=0
	if len(n.Recent()) != 0 {
		t.Fatalf("expected no-op with limit<=0")
	}

	n2 := New("http://example.invalid", nil, time.Second)
	n2.Observe("teamA", 500, 1000)
	if len(n2.Recent()) != 0 {
		t.Fatalf("expected no-op with no thresholds")
	}
}

func TestRecent_RingCapAndOrder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := New(srv.URL, []float64{0.1}, time.Second)
	for i := 0; i < recentCap+5; i++ {
		// Force a fire every time by dropping ratio then re-crossing.
		n.Observe("team", 0, 1000)
		n.Observe("team", 200, 1000)
	}
	fires := waitForFires(t, n, recentCap)
	if len(fires) != recentCap {
		t.Fatalf("expected ring capped at %d, got %d", recentCap, len(fires))
	}
}

func TestClose_WaitsForInFlightDeliveries(t *testing.T) {
	release := make(chan struct{})
	var served int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // block until the test lets delivery complete
		atomic.AddInt32(&served, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := New(srv.URL, []float64{0.5}, 5*time.Second)
	n.Observe("team", 600, 1000) // spawns a delivery goroutine, now blocked in the handler

	done := make(chan struct{})
	go func() { n.Close(); close(done) }()

	// Close must still be blocking while the delivery is in flight.
	select {
	case <-done:
		t.Fatal("Close returned before the in-flight delivery finished")
	case <-time.After(50 * time.Millisecond):
	}

	close(release) // let the delivery complete
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return after the delivery finished")
	}
	if atomic.LoadInt32(&served) != 1 {
		t.Fatalf("expected the in-flight delivery to complete, served=%d", served)
	}
}

func TestClose_NilSafe(t *testing.T) {
	var n *Notifier
	n.Close() // must not panic
}

// --- per-key budget alerts (ADR-017 deferred item) ---

func TestObserveKey_CrossesThresholdFiresWithKeyID(t *testing.T) {
	var mu sync.Mutex
	var received []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		json.NewDecoder(r.Body).Decode(&payload)
		mu.Lock()
		received = append(received, payload)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := New(srv.URL, []float64{0.8}, time.Second)
	n.ObserveKey("acme", "ik_over", 850_000, 1_000_000)
	fires := waitForFires(t, n, 1)
	if fires[0].Team != "acme" || fires[0].KeyID != "ik_over" || !fires[0].Delivered {
		t.Fatalf("expected delivered key-scoped fire, got %+v", fires[0])
	}

	mu.Lock()
	payload := received[0]
	mu.Unlock()
	if payload["key_id"] != "ik_over" {
		t.Fatalf("webhook payload missing key_id, got %+v", payload)
	}
}

func TestObserveKey_DedupeAndRearm(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := New(srv.URL, []float64{0.8}, time.Second)
	n.ObserveKey("acme", "ik_over", 900_000, 1_000_000) // crosses 0.8
	waitForFires(t, n, 1)

	// Same ratio again: dedupe, no second fire.
	n.ObserveKey("acme", "ik_over", 900_000, 1_000_000)
	time.Sleep(20 * time.Millisecond)
	if len(n.Recent()) != 1 {
		t.Fatalf("expected dedupe at same ratio, got %d fires", len(n.Recent()))
	}

	// Ratio drop re-arms.
	n.ObserveKey("acme", "ik_over", 100_000, 1_000_000)
	time.Sleep(20 * time.Millisecond)
	if len(n.Recent()) != 1 {
		t.Fatalf("expected no fire on ratio drop, got %d", len(n.Recent()))
	}
	n.ObserveKey("acme", "ik_over", 900_000, 1_000_000)
	waitForFires(t, n, 2)
}

// TestObserveKey_TeamNamedLikeKeyIDDoesNotCollide pins the plan-gate round-1
// fix: fired (team) and firedKey (key) are separate maps, so a team and a key
// can share the exact same string identity without corrupting each other's
// dedupe state.
func TestObserveKey_TeamNamedLikeKeyIDDoesNotCollide(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := New(srv.URL, []float64{0.8}, time.Second)
	n.Observe("ik_abc", 900_000, 1_000_000)                  // a TEAM literally named "ik_abc"
	n.ObserveKey("other-team", "ik_abc", 900_000, 1_000_000) // a KEY with the same string identity
	fires := waitForFires(t, n, 2)

	var sawTeamFire, sawKeyFire bool
	for _, f := range fires {
		if f.Team == "ik_abc" && f.KeyID == "" {
			sawTeamFire = true
		}
		if f.Team == "other-team" && f.KeyID == "ik_abc" {
			sawKeyFire = true
		}
	}
	if !sawTeamFire || !sawKeyFire {
		t.Fatalf("expected both an independent team fire and key fire, got %+v", fires)
	}

	n.mu.Lock()
	_, inFired := n.fired["ik_abc"]
	_, inFiredKey := n.firedKey["ik_abc"]
	n.mu.Unlock()
	if !inFired || !inFiredKey {
		t.Fatalf("both fired[%q] and firedKey[%q] must be tracked independently: fired=%v firedKey=%v", "ik_abc", "ik_abc", inFired, inFiredKey)
	}
}

func TestDeliver_KeyIDOmittedForTeamFire(t *testing.T) {
	var mu sync.Mutex
	var received []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		json.NewDecoder(r.Body).Decode(&payload)
		mu.Lock()
		received = append(received, payload)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := New(srv.URL, []float64{0.8}, time.Second)
	n.Observe("acme", 850_000, 1_000_000)
	waitForFires(t, n, 1)

	mu.Lock()
	_, hasKeyID := received[0]["key_id"]
	mu.Unlock()
	if hasKeyID {
		t.Fatalf("a team-level fire's webhook payload must not carry a key_id key at all, got %+v", received[0])
	}
}

func TestDeliver_ErrorNeverLeaksURL(t *testing.T) {
	// A URL with an embedded token pointing at a closed port -> connection error.
	secretURL := "http://127.0.0.1:1/webhook?token=super-secret-token"
	n := New(secretURL, []float64{0.5}, 200*time.Millisecond)
	n.Observe("team", 600, 1000)
	fires := waitForFires(t, n, 1)
	if fires[0].Delivered {
		t.Fatalf("expected delivery failure for unreachable URL")
	}
	if strings.Contains(fires[0].Error, "super-secret-token") || strings.Contains(fires[0].Error, secretURL) {
		t.Fatalf("error must not leak the webhook URL, got %q", fires[0].Error)
	}
}
