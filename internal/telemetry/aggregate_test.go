package telemetry

import (
	"context"
	"testing"
	"time"
)

// aggregatorSuite runs the same contract against every Aggregator
// implementation — Task 8 reruns it verbatim against Postgres.
func aggregatorSuite(t *testing.T, newAgg func(t *testing.T) Aggregator) {
	ctx := context.Background()
	w0 := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)

	batch := func(dp string, start time.Time, entries ...UsageEntry) *UsageBatch {
		return &UsageBatch{Dataplane: dp, WindowStart: start, WindowEnd: start.Add(time.Minute), Entries: entries}
	}
	entry := func(team, user, model string, spent int64) UsageEntry {
		return UsageEntry{Team: team, User: user, Model: model, SpentMicroUSD: spent,
			InputTokens: spent * 2, OutputTokens: spent, CacheReadTokens: 7, CacheWrite5mTokens: 5, CacheWrite1hTokens: 3}
	}

	t.Run("upsert twice counts once", func(t *testing.T) {
		a := newAgg(t)
		b := batch("dp-1", w0, entry("demo", "u1", "m1", 100))
		if err := a.Upsert(ctx, b); err != nil {
			t.Fatal(err)
		}
		if err := a.Upsert(ctx, b); err != nil {
			t.Fatal(err)
		}
		res, err := a.Query(ctx, QueryFilter{Since: w0, Until: w0.Add(time.Hour), GroupBy: "team"})
		if err != nil {
			t.Fatal(err)
		}
		if res.TotalMicroUSD != 100 {
			t.Fatalf("retried batch double-counted: total=%d", res.TotalMicroUSD)
		}
	})

	t.Run("batch replace removes stale rows", func(t *testing.T) {
		a := newAgg(t)
		// First delivery: two entries. Corrected retry: one entry.
		_ = a.Upsert(ctx, batch("dp-1", w0, entry("demo", "u1", "m1", 100), entry("demo", "u2", "m1", 50)))
		if err := a.Upsert(ctx, batch("dp-1", w0, entry("demo", "u1", "m1", 100))); err != nil {
			t.Fatal(err)
		}
		res, _ := a.Query(ctx, QueryFilter{Since: w0, Until: w0.Add(time.Hour), GroupBy: "user"})
		if res.TotalMicroUSD != 100 || len(res.Rows) != 1 {
			t.Fatalf("stale row survived a corrected retry: %+v", res)
		}
	})

	t.Run("group by sums across windows and dataplanes", func(t *testing.T) {
		a := newAgg(t)
		_ = a.Upsert(ctx, batch("dp-1", w0, entry("demo", "u1", "m1", 100), entry("ops", "u2", "m2", 30)))
		_ = a.Upsert(ctx, batch("dp-2", w0, entry("demo", "u3", "m2", 40)))
		_ = a.Upsert(ctx, batch("dp-1", w0.Add(time.Minute), entry("demo", "u1", "m1", 5)))

		res, err := a.Query(ctx, QueryFilter{Since: w0, Until: w0.Add(time.Hour), GroupBy: "team"})
		if err != nil {
			t.Fatal(err)
		}
		if res.TotalMicroUSD != 175 {
			t.Fatalf("total = %d, want 175", res.TotalMicroUSD)
		}
		got := map[string]int64{}
		for _, r := range res.Rows {
			got[r.Key] = r.SpentMicroUSD
		}
		if got["demo"] != 145 || got["ops"] != 30 {
			t.Fatalf("team sums wrong: %v", got)
		}

		byModel, _ := a.Query(ctx, QueryFilter{Since: w0, Until: w0.Add(time.Hour), GroupBy: "model"})
		gotM := map[string]int64{}
		for _, r := range byModel.Rows {
			gotM[r.Key] = r.SpentMicroUSD
		}
		if gotM["m1"] != 105 || gotM["m2"] != 70 {
			t.Fatalf("model sums wrong: %v", gotM)
		}
	})

	t.Run("filters compose and range is inclusive-start exclusive-end", func(t *testing.T) {
		a := newAgg(t)
		_ = a.Upsert(ctx, batch("dp-1", w0, entry("demo", "u1", "m1", 100)))
		_ = a.Upsert(ctx, batch("dp-1", w0.Add(time.Minute), entry("demo", "u1", "m2", 40)))

		// Until == second window's start → excluded.
		res, _ := a.Query(ctx, QueryFilter{Team: "demo", Since: w0, Until: w0.Add(time.Minute), GroupBy: "model"})
		if res.TotalMicroUSD != 100 {
			t.Fatalf("exclusive-end violated: %d", res.TotalMicroUSD)
		}
		// Since == first window's start → included.
		res2, _ := a.Query(ctx, QueryFilter{Team: "demo", Model: "m1", Since: w0, Until: w0.Add(time.Hour), GroupBy: "team"})
		if res2.TotalMicroUSD != 100 {
			t.Fatalf("inclusive-start or model filter violated: %d", res2.TotalMicroUSD)
		}
		// Non-matching team filter.
		res3, _ := a.Query(ctx, QueryFilter{Team: "nope", Since: w0, Until: w0.Add(time.Hour), GroupBy: "team"})
		if res3.TotalMicroUSD != 0 || len(res3.Rows) != 0 {
			t.Fatalf("team filter leaked: %+v", res3)
		}
	})

	t.Run("cache tiers survive to query rows", func(t *testing.T) {
		a := newAgg(t)
		_ = a.Upsert(ctx, batch("dp-1", w0, entry("demo", "u1", "m1", 100)))
		res, _ := a.Query(ctx, QueryFilter{Since: w0, Until: w0.Add(time.Hour), GroupBy: "team"})
		if len(res.Rows) != 1 {
			t.Fatalf("want 1 row, got %d: %+v", len(res.Rows), res)
		}
		r := res.Rows[0]
		if r.CacheReadTokens != 7 || r.CacheWrite5mTokens != 5 || r.CacheWrite1hTokens != 3 {
			t.Fatalf("cache tiers lost or collapsed: %+v", r)
		}
	})

	t.Run("rows streams raw granularity via callback", func(t *testing.T) {
		a := newAgg(t)
		_ = a.Upsert(ctx, batch("dp-1", w0, entry("demo", "u1", "m1", 100), entry("demo", "u2", "m1", 50)))
		var rows []StoredRow
		err := a.Rows(ctx, w0, w0.Add(time.Hour), func(r StoredRow) error {
			rows = append(rows, r)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 2 {
			t.Fatalf("want 2 raw rows, got %d", len(rows))
		}
		// A SQL driver may decode the same UTC instant with a different
		// Location pointer. Compare the timestamp, not time.Time internals.
		if rows[0].Dataplane != "dp-1" || !rows[0].WindowStart.Equal(w0) {
			t.Fatalf("row lost window identity: %+v", rows[0])
		}
	})

	t.Run("invalid group_by rejected", func(t *testing.T) {
		a := newAgg(t)
		if _, err := a.Query(ctx, QueryFilter{Since: w0, Until: w0.Add(time.Hour), GroupBy: "keyid"}); err == nil {
			t.Fatal("invalid group_by accepted")
		}
	})
}

func TestMemoryAggregator(t *testing.T) {
	aggregatorSuite(t, func(t *testing.T) Aggregator {
		a := NewMemoryAggregator(24 * time.Hour)
		// Pin the wall clock to aggregatorSuite's w0 so its fixtures (all
		// within a few minutes of w0) never fall outside the 24h retention
		// horizon — without this, Upsert evicts every fixture the instant
		// today's date moves more than 24h past the hardcoded w0.
		a.now = func() time.Time { return time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC) }
		return a
	})
}

// Retention: windows older than the retention horizon are evicted on Upsert.
func TestMemoryAggregatorRetention(t *testing.T) {
	ctx := context.Background()
	a := NewMemoryAggregator(time.Hour)
	old := time.Date(2026, 8, 4, 0, 0, 0, 0, time.UTC)
	fresh := old.Add(2 * time.Hour)
	a.now = func() time.Time { return fresh } // pin the wall clock (retention anchor)

	_ = a.Upsert(ctx, &UsageBatch{Dataplane: "dp-1", WindowStart: old, WindowEnd: old.Add(time.Minute),
		Entries: []UsageEntry{{Team: "demo", Model: "m1", SpentMicroUSD: 100}}})
	// A fresh upsert 2h later evicts the >1h-old window.
	_ = a.Upsert(ctx, &UsageBatch{Dataplane: "dp-1", WindowStart: fresh, WindowEnd: fresh.Add(time.Minute),
		Entries: []UsageEntry{{Team: "demo", Model: "m1", SpentMicroUSD: 5}}})

	res, _ := a.Query(ctx, QueryFilter{Since: old, Until: fresh.Add(time.Hour), GroupBy: "team"})
	if res.TotalMicroUSD != 5 {
		t.Fatalf("expired window not evicted: total=%d", res.TotalMicroUSD)
	}
}

// The memory Rows implementation must snapshot under the lock and stream
// after releasing it: a slow callback (a stalled export client) must not
// block Upsert (ingestion).
func TestMemoryRowsDoesNotBlockUpsert(t *testing.T) {
	ctx := context.Background()
	a := NewMemoryAggregator(24 * time.Hour)
	w0 := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	// Pin the wall clock to w0: without it, retention (relative to the real
	// now) evicts w0 on the very next Upsert once "today" drifts more than
	// 24h past this hardcoded date, leaving Rows nothing to stream — the
	// callback below never runs and <-inCallback hangs forever.
	a.now = func() time.Time { return w0 }
	_ = a.Upsert(ctx, &UsageBatch{Dataplane: "dp-1", WindowStart: w0, WindowEnd: w0.Add(time.Minute),
		Entries: []UsageEntry{{Team: "demo", Model: "m1", SpentMicroUSD: 1}}})

	inCallback := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- a.Rows(ctx, w0, w0.Add(time.Hour), func(StoredRow) error {
			close(inCallback)
			<-release // simulate a stalled network client
			return nil
		})
	}()

	<-inCallback
	upserted := make(chan struct{})
	go func() {
		_ = a.Upsert(ctx, &UsageBatch{Dataplane: "dp-2", WindowStart: w0, WindowEnd: w0.Add(time.Minute),
			Entries: []UsageEntry{{Team: "demo", Model: "m1", SpentMicroUSD: 2}}})
		close(upserted)
	}()

	select {
	case <-upserted: // ingestion proceeded while the export callback was stalled
	case <-time.After(2 * time.Second):
		t.Fatal("Upsert blocked behind a stalled Rows callback — lock held across client writes")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// A clock-skewed data plane sending a far-future window must not mass-evict
// everyone else's current windows (retention anchors to the wall clock).
func TestRetentionImmuneToFutureSkew(t *testing.T) {
	ctx := context.Background()
	a := NewMemoryAggregator(24 * time.Hour)
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return now }

	_ = a.Upsert(ctx, &UsageBatch{Dataplane: "dp-1", WindowStart: now, WindowEnd: now.Add(time.Minute),
		Entries: []UsageEntry{{Team: "demo", Model: "m1", SpentMicroUSD: 100}}})
	// Skewed plane: +30 days.
	_ = a.Upsert(ctx, &UsageBatch{Dataplane: "dp-skew", WindowStart: now.Add(30 * 24 * time.Hour), WindowEnd: now.Add(30*24*time.Hour + time.Minute),
		Entries: []UsageEntry{{Team: "demo", Model: "m1", SpentMicroUSD: 1}}})

	res, _ := a.Query(ctx, QueryFilter{Since: now.Add(-time.Hour), Until: now.Add(time.Hour), GroupBy: "team"})
	if res.TotalMicroUSD != 100 {
		t.Fatalf("current window evicted by a skewed peer: total=%d", res.TotalMicroUSD)
	}
}
