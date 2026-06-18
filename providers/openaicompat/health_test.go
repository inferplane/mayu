package openaicompat

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestProvider(baseURL, key string) *provider {
	return &provider{baseURL: baseURL, apiKey: key, client: &http.Client{}}
}

func TestHealthCheck_OK_WithBearer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer key-secret-xyz" {
			t.Errorf("want bearer auth, got %q", got)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	res := newTestProvider(srv.URL, "key-secret-xyz").HealthCheck(context.Background())
	if !res.OK {
		t.Fatalf("want OK, got %+v", res)
	}
}

func TestHealthCheck_KeylessSendsNoAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("keyless provider must not send Authorization")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	res := newTestProvider(srv.URL, "").HealthCheck(context.Background())
	if !res.OK {
		t.Fatalf("want OK, got %+v", res)
	}
}

func TestHealthCheck_5xx_Sanitized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	res := newTestProvider(srv.URL, "key-secret-xyz").HealthCheck(context.Background())
	if res.OK {
		t.Fatal("502 must not be OK")
	}
	if strings.Contains(res.Detail, "key-secret-xyz") {
		t.Fatalf("detail leaked the api key: %q", res.Detail)
	}
	if !strings.Contains(res.Detail, "502") {
		t.Errorf("detail should name the status, got %q", res.Detail)
	}
}

func TestHealthCheck_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	res := newTestProvider(srv.URL, "k").HealthCheck(ctx)
	if res.OK {
		t.Fatal("timed-out probe must not be OK")
	}
}
