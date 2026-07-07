package analyticsapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/inferplane/inferplane/internal/analytics"
)

type fakeQ struct {
	gotSince, gotUntil string
}

func (f *fakeQ) Summary(q analytics.SummaryQuery) (analytics.Summary, error) {
	f.gotSince, f.gotUntil = q.SinceDay, q.UntilDay
	return analytics.Summary{Totals: analytics.Totals{Requests: 3, CostMicros: 1234}}, nil
}
func (f *fakeQ) TimeSeries(analytics.TimeSeriesQuery) ([]analytics.DayPoint, error) {
	return []analytics.DayPoint{{Day: "2026-06-29", CostMicros: 1234}}, nil
}
func (f *fakeQ) Health() (analytics.Health, error) {
	return analytics.Health{Mode: "A", IsLeader: true}, nil
}

// fakeRebuildableQ additionally implements analytics.Rebuilder (Mode B shape) —
// Mode A's *analytics.Index does not, and fakeQ above deliberately doesn't
// either, so RebuildHandler's 404-vs-204 branch has a fixture for both sides.
type fakeRebuildableQ struct {
	fakeQ
	rebuilt    bool
	rebuildErr error
}

func (f *fakeRebuildableQ) Rebuild(context.Context) error {
	f.rebuilt = true
	return f.rebuildErr
}

func TestSummaryHandler_GET_defaultsWindow(t *testing.T) {
	f := &fakeQ{}
	rec := httptest.NewRecorder()
	SummaryHandler(f).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/analytics/summary", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"cost_micros"`) {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
	// Empty since must be defaulted to a bounded ~30d window, not unbounded.
	if f.gotSince == "" {
		t.Error("SummaryHandler did not default an empty since → unbounded scan (spec §13)")
	}
}

func TestSummaryHandler_rejectsBadDateAndHugeWindow(t *testing.T) {
	rec := httptest.NewRecorder()
	SummaryHandler(&fakeQ{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/analytics/summary?since=June", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("malformed since: got %d, want 400", rec.Code)
	}
	rec = httptest.NewRecorder()
	SummaryHandler(&fakeQ{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/analytics/summary?since=2000-01-01&until=2026-01-01", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("oversized window: got %d, want 400", rec.Code)
	}
}

func TestSummaryHandler_rejectsPOST(t *testing.T) {
	rec := httptest.NewRecorder()
	SummaryHandler(&fakeQ{}).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/analytics/summary", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status %d, want 405", rec.Code)
	}
}

func TestTimeSeriesHandler_GET(t *testing.T) {
	rec := httptest.NewRecorder()
	TimeSeriesHandler(&fakeQ{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/analytics/timeseries?days=7", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"day"`) {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
}

func TestSummaryHandler_rejectsReversedRange(t *testing.T) {
	rec := httptest.NewRecorder()
	SummaryHandler(&fakeQ{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/admin/analytics/summary?since=2026-06-29&until=2026-06-01", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("reversed range: got %d, want 400", rec.Code)
	}
}

// --- Phase 1b Task 4: /admin/analytics/health + /admin/analytics/rebuild ---

func TestHealthHandler_GET(t *testing.T) {
	rec := httptest.NewRecorder()
	HealthHandler(&fakeQ{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/analytics/health", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"mode":"A"`) {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
}

func TestHealthHandler_rejectsPOST(t *testing.T) {
	rec := httptest.NewRecorder()
	HealthHandler(&fakeQ{}).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/analytics/health", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status %d, want 405", rec.Code)
	}
}

func TestRebuildHandler_callsRebuildAndReturns204(t *testing.T) {
	q := &fakeRebuildableQ{}
	rec := httptest.NewRecorder()
	RebuildHandler(q).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/analytics/rebuild", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status %d, want 204", rec.Code)
	}
	if !q.rebuilt {
		t.Fatal("Rebuild was not called")
	}
}

func TestRebuildHandler_405WhenQuerierIsNotARebuilder(t *testing.T) {
	// fakeQ (Mode A shape) does not implement analytics.Rebuilder.
	rec := httptest.NewRecorder()
	RebuildHandler(&fakeQ{}).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/analytics/rebuild", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status %d, want 405 (Mode A has no rebuild)", rec.Code)
	}
}

func TestRebuildHandler_rejectsGET(t *testing.T) {
	rec := httptest.NewRecorder()
	RebuildHandler(&fakeRebuildableQ{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/analytics/rebuild", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status %d, want 405", rec.Code)
	}
}

func TestRebuildHandler_500OnRebuildError(t *testing.T) {
	q := &fakeRebuildableQ{rebuildErr: context.DeadlineExceeded}
	rec := httptest.NewRecorder()
	RebuildHandler(q).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/analytics/rebuild", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500", rec.Code)
	}
}
