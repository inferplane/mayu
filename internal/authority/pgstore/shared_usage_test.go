package pgstore

import (
	"testing"

	v1alpha1 "github.com/inferplane/inferplane/api/v1alpha1"
	"github.com/inferplane/inferplane/internal/keystore"
)

func TestSharedUsageWireLabelsAndReservationSubset(t *testing.T) {
	f := newSharedFixture(t)
	f.team(t, keystore.TeamRecord{Name: "alpha", RPM: 10, TokensPerDay: 100})
	putDocument(t, f.db, sharedDoc("personal", v1alpha1.Subject{Team: "alpha", User: "person"},
		sharedQuota("quota", 100, v1alpha1.PeriodCalendarMonth)))
	_, r := f.key(t, "alpha", keystore.KeyOptions{TPM: 100, Owner: "person"})
	sharedReserve(t, f.a, r)
	combined := false
	for _, u := range sharedUsage(t, f.b, r.Subject) {
		switch u.Scope {
		case "team", "key", "user":
		case "team-user":
			combined = true
		default:
			t.Fatalf("unsupported scope %q", u.Scope)
		}
		switch u.Kind {
		case "rpm", "tpm":
			if u.Window != "minute" {
				t.Fatalf("rate window=%q, want minute", u.Window)
			}
		case "tokens", "microUSD":
			if u.Window != "CalendarDay" && u.Window != "CalendarMonth" {
				t.Fatalf("calendar window=%q", u.Window)
			}
		default:
			t.Fatalf("unsupported kind %q", u.Kind)
		}
		if u.Used < 0 || u.Reserved < 0 || u.Reserved > u.Used {
			t.Fatalf("invalid reservation subset: used=%d reserved=%d", u.Used, u.Reserved)
		}
	}
	if !combined {
		t.Fatal("missing team-user scope")
	}
}
