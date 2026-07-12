package bodystore

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// backends runs each contract test below against every backend. Postgres is
// included only when INFERPLANE_TEST_PG_DSN is set (the zero-dependency
// default path never requires Postgres, mirrors internal/analytics/pgstore).
func backends(t *testing.T) map[string]Store {
	t.Helper()
	sq, err := OpenSQLite(filepath.Join(t.TempDir(), "bodies.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sq.Close() })
	out := map[string]Store{"sqlite": sq}

	if dsn := os.Getenv("INFERPLANE_TEST_PG_DSN"); dsn != "" {
		pg, err := NewPostgres(context.Background(), dsn)
		if err != nil {
			t.Fatalf("NewPostgres: %v", err)
		}
		t.Cleanup(func() { pg.Close() })
		// Clean slate: this package's tests are the only writer of the
		// bodies table in the test DB, so a blanket delete is safe.
		pg.db.Exec(context.Background(), `DELETE FROM bodies`)
		out["postgres"] = pg
	}
	return out
}

func testRow(ref string, size int64, created, expires string) Row {
	return Row{
		Ref: ref, RecordID: "rec-" + ref, Team: "acme",
		CreatedTS: created, ExpiresTS: expires, Size: size,
		WrappedKeyNonce: []byte("wknonce"), WrappedKeyCT: []byte("wkct"),
		ReqNonce: []byte("reqnonce"), ReqCT: []byte("reqct"),
	}
}

func TestBackends_PutGetRoundTrip(t *testing.T) {
	for name, s := range backends(t) {
		t.Run(name, func(t *testing.T) {
			row := testRow("ref-1", 100, "2026-01-01T00:00:00Z", "2099-01-01T00:00:00Z")
			if err := s.Put(context.Background(), row); err != nil {
				t.Fatal(err)
			}
			got, err := s.Get(context.Background(), "ref-1")
			if err != nil {
				t.Fatal(err)
			}
			if got.Ref != row.Ref || got.RecordID != row.RecordID || got.Team != row.Team ||
				string(got.ReqCT) != string(row.ReqCT) || got.RespCT != nil {
				t.Fatalf("roundtrip mismatch: %+v", got)
			}
		})
	}
}

func TestBackends_GetAbsentReturnsErrGone(t *testing.T) {
	for name, s := range backends(t) {
		t.Run(name, func(t *testing.T) {
			if _, err := s.Get(context.Background(), "never-existed"); err != ErrGone {
				t.Fatalf("Get on absent ref = %v, want ErrGone", err)
			}
		})
	}
}

func TestBackends_DeleteIdempotent(t *testing.T) {
	for name, s := range backends(t) {
		t.Run(name, func(t *testing.T) {
			row := testRow("ref-del", 10, "2026-01-01T00:00:00Z", "2099-01-01T00:00:00Z")
			if err := s.Put(context.Background(), row); err != nil {
				t.Fatal(err)
			}
			if err := s.Delete(context.Background(), "ref-del"); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Get(context.Background(), "ref-del"); err != ErrGone {
				t.Fatalf("Get after Delete = %v, want ErrGone", err)
			}
			// second delete: idempotent, no error.
			if err := s.Delete(context.Background(), "ref-del"); err != nil {
				t.Fatalf("second Delete must be a no-op, got: %v", err)
			}
		})
	}
}

func TestBackends_PurgeTTL(t *testing.T) {
	for name, s := range backends(t) {
		t.Run(name, func(t *testing.T) {
			now := time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC)
			expired := testRow("ref-expired", 10, "2026-01-01T00:00:00Z", "2026-07-01T00:00:00Z")
			fresh := testRow("ref-fresh", 10, "2026-01-01T00:00:00Z", "2099-01-01T00:00:00Z")
			if err := s.Put(context.Background(), expired); err != nil {
				t.Fatal(err)
			}
			if err := s.Put(context.Background(), fresh); err != nil {
				t.Fatal(err)
			}
			n, err := s.Purge(context.Background(), now, 1<<62) // huge cap: TTL only
			if err != nil {
				t.Fatal(err)
			}
			if n != 1 {
				t.Fatalf("purged %d rows, want 1 (only the expired one)", n)
			}
			if _, err := s.Get(context.Background(), "ref-expired"); err != ErrGone {
				t.Fatal("expired row must be gone after Purge")
			}
			if _, err := s.Get(context.Background(), "ref-fresh"); err != nil {
				t.Fatal("fresh row must survive TTL purge")
			}
		})
	}
}

// TestBackends_ListAndUpdateWrappedKey pins the ADR-018 key-rotation deferred
// item's Store contract: ListWrappedKeys projects only the wrapped-key
// columns, and UpdateWrappedKey is a compare-and-swap keyed on the OLD bytes.
func TestBackends_ListAndUpdateWrappedKey(t *testing.T) {
	for name, s := range backends(t) {
		t.Run(name, func(t *testing.T) {
			row := testRow("ref-rewrap", 10, "2026-01-01T00:00:00Z", "2099-01-01T00:00:00Z")
			if err := s.Put(context.Background(), row); err != nil {
				t.Fatal(err)
			}

			list, err := s.ListWrappedKeys(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			var found *WrappedKeyRow
			for i := range list {
				if list[i].Ref == "ref-rewrap" {
					found = &list[i]
				}
			}
			if found == nil {
				t.Fatal("ListWrappedKeys did not return the row")
			}
			if string(found.Nonce) != string(row.WrappedKeyNonce) || string(found.CT) != string(row.WrappedKeyCT) {
				t.Fatalf("ListWrappedKeys bytes = %q/%q, want %q/%q", found.Nonce, found.CT, row.WrappedKeyNonce, row.WrappedKeyCT)
			}

			newNonce, newCT := []byte("new-nonce"), []byte("new-ct")
			matched, err := s.UpdateWrappedKey(context.Background(), "ref-rewrap", found.Nonce, found.CT, newNonce, newCT)
			if err != nil {
				t.Fatal(err)
			}
			if !matched {
				t.Fatal("UpdateWrappedKey with the correct old bytes must match")
			}

			list2, err := s.ListWrappedKeys(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			var found2 *WrappedKeyRow
			for i := range list2 {
				if list2[i].Ref == "ref-rewrap" {
					found2 = &list2[i]
				}
			}
			if found2 == nil || string(found2.Nonce) != "new-nonce" || string(found2.CT) != "new-ct" {
				t.Fatalf("update did not persist: %+v", found2)
			}

			// A second CAS with the now-STALE old bytes must not match (the
			// row already moved on) — proves the swap is keyed on the old
			// value, not just the ref.
			matched2, err := s.UpdateWrappedKey(context.Background(), "ref-rewrap", found.Nonce, found.CT, []byte("x"), []byte("y"))
			if err != nil {
				t.Fatal(err)
			}
			if matched2 {
				t.Fatal("UpdateWrappedKey with stale old bytes must not match")
			}
		})
	}
}

func TestBackends_PurgeSizeCapEvictsOldestFirst(t *testing.T) {
	for name, s := range backends(t) {
		t.Run(name, func(t *testing.T) {
			far := "2099-01-01T00:00:00Z" // no TTL pressure — only size-cap matters
			old := testRow("ref-old", 100, "2026-01-01T00:00:00Z", far)
			mid := testRow("ref-mid", 100, "2026-01-02T00:00:00Z", far)
			new := testRow("ref-new", 100, "2026-01-03T00:00:00Z", far)
			for _, r := range []Row{old, mid, new} {
				if err := s.Put(context.Background(), r); err != nil {
					t.Fatal(err)
				}
			}
			// Cap at 150 bytes: total is 300, so the two oldest must go, one survives.
			// now=time.Now() (well before the far-future expires_ts above) so this
			// purge is size-cap-only — TTL must not also fire and delete everything.
			n, err := s.Purge(context.Background(), time.Now(), 150)
			if err != nil {
				t.Fatal(err)
			}
			if n != 2 {
				t.Fatalf("purged %d rows, want 2", n)
			}
			if _, err := s.Get(context.Background(), "ref-old"); err != ErrGone {
				t.Fatal("oldest row must be evicted first")
			}
			if _, err := s.Get(context.Background(), "ref-mid"); err != ErrGone {
				t.Fatal("second-oldest row must also be evicted (still over cap)")
			}
			if _, err := s.Get(context.Background(), "ref-new"); err != nil {
				t.Fatal("newest row must survive the size cap")
			}
		})
	}
}
