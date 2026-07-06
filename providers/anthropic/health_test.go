package anthropic

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestProvider(baseURL string) *provider {
	return &provider{baseURL: baseURL, apiKey: "sk-secret-value-should-never-leak", client: &http.Client{}}
}

func TestHealthCheck_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if r.Header.Get("x-api-key") == "" {
			t.Error("probe did not send x-api-key")
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	res := newTestProvider(srv.URL).HealthCheck(context.Background())
	if !res.OK {
		t.Fatalf("want OK, got %+v", res)
	}
}

func TestHealthCheck_BearerModeUsesAuthorizationHeader(t *testing.T) {
	// A bearer-configured provider (e.g. OpenRouter) probed with x-api-key
	// would always 401 and report unhealthy even when actually up.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "" {
			t.Error("bearer mode must not send x-api-key")
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-secret-value-should-never-leak" {
			t.Errorf("Authorization = %q, want Bearer sk-secret-value-should-never-leak", got)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := newTestProvider(srv.URL)
	p.bearer = true
	res := p.HealthCheck(context.Background())
	if !res.OK {
		t.Fatalf("want OK, got %+v", res)
	}
}

func TestHealthCheck_Unauthorized_Sanitized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	res := newTestProvider(srv.URL).HealthCheck(context.Background())
	if res.OK {
		t.Fatal("401 must not be OK")
	}
	if strings.Contains(res.Detail, "sk-secret-value-should-never-leak") {
		t.Fatalf("detail leaked the api key: %q", res.Detail)
	}
	if !strings.Contains(res.Detail, "401") {
		t.Errorf("detail should name the status, got %q", res.Detail)
	}
}

func TestHealthCheck_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	res := newTestProvider(srv.URL).HealthCheck(ctx)
	if res.OK {
		t.Fatal("timed-out probe must not be OK")
	}
}
