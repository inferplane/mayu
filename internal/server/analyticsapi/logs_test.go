package analyticsapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/inferplane/inferplane/internal/analytics"
)

func TestLogsHandler_returnsEvents(t *testing.T) {
	rec := httptest.NewRecorder()
	LogsHandler(&fakeQ{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/logs", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"01FAKE"`) {
		t.Fatalf("body missing event: %s", rec.Body.String())
	}
}

func TestLogsHandler_passesLimitAndBefore(t *testing.T) {
	var gotLimit int
	var gotBefore string
	q := &recordingLogsQ{fakeQ: &fakeQ{}, onRecent: func(limit int, before string) {
		gotLimit, gotBefore = limit, before
	}}
	rec := httptest.NewRecorder()
	LogsHandler(q).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/logs?limit=10&before=01ABC", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if gotLimit != 10 || gotBefore != "01ABC" {
		t.Fatalf("Recent called with limit=%d before=%q, want 10, 01ABC", gotLimit, gotBefore)
	}
}

func TestLogsHandler_rejectsNonGET(t *testing.T) {
	rec := httptest.NewRecorder()
	LogsHandler(&fakeQ{}).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/logs", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status %d, want 405", rec.Code)
	}
}

func TestLogsHandler_500OnQueryError(t *testing.T) {
	q := &recordingLogsQ{fakeQ: &fakeQ{}, err: errQueryFailed}
	rec := httptest.NewRecorder()
	LogsHandler(q).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/logs", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500", rec.Code)
	}
}

var errQueryFailed = &queryError{}

type queryError struct{}

func (*queryError) Error() string { return "query failed" }

// recordingLogsQ wraps fakeQ to observe/override Recent's arguments and error.
type recordingLogsQ struct {
	*fakeQ
	onRecent func(limit int, before string)
	err      error
}

func (q *recordingLogsQ) Recent(limit int, before string) ([]analytics.Event, error) {
	if q.onRecent != nil {
		q.onRecent(limit, before)
	}
	if q.err != nil {
		return nil, q.err
	}
	return q.fakeQ.Recent(limit, before)
}
