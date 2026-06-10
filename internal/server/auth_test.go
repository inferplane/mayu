package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDevKeyAuth(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	h := DevKeyAuth("secret-key", next)

	cases := []struct {
		name, header, value string
		want                int
	}{
		{"valid x-api-key", "x-api-key", "secret-key", 200},
		{"valid bearer", "Authorization", "Bearer secret-key", 200},
		{"wrong key", "x-api-key", "nope", 401},
		{"missing", "", "", 401},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/v1/messages", nil)
			if c.header != "" {
				req.Header.Set(c.header, c.value)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != c.want {
				t.Fatalf("got %d want %d", rec.Code, c.want)
			}
		})
	}
}

func TestDevKeyAuthRejectsEmptyKey(t *testing.T) {
	// Defense-in-depth: an empty devKey must never authenticate anyone,
	// even a client sending no credentials (which would otherwise match "").
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	h := DevKeyAuth("", next)
	req := httptest.NewRequest("POST", "/v1/messages", nil) // no auth header
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("empty devKey must reject all requests; got %d", rec.Code)
	}
}
