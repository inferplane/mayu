package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAdminTokenAuth(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	h := AdminTokenAuth([]string{"tok-a", "tok-b"}, next)
	cases := []struct {
		tok  string
		want int
	}{
		{"tok-a", 200}, {"tok-b", 200}, {"wrong", 401}, {"", 401},
	}
	for _, c := range cases {
		req := httptest.NewRequest("POST", "/admin/keys", nil)
		if c.tok != "" {
			req.Header.Set("Authorization", "Bearer "+c.tok)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Fatalf("tok %q: got %d want %d", c.tok, rec.Code, c.want)
		}
	}
}

func TestAdminTokenAuthRejectsEmptyConfig(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	h := AdminTokenAuth(nil, next)
	req := httptest.NewRequest("POST", "/admin/keys", nil)
	req.Header.Set("Authorization", "Bearer anything")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("empty token config must deny all: got %d", rec.Code)
	}
}
