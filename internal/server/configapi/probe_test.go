package configapi

import (
	"context"
	"encoding/json"
	"iter"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/inferplane/inferplane/pkg/schema"
	"github.com/inferplane/inferplane/providers"
	_ "github.com/inferplane/inferplane/providers/anthropic" // registers "anthropic" for TestProbe_AuthHeaderBearerUsesAuthorizationHeader
)

// noHealthProvider is a provider that does NOT implement HealthChecker, to
// exercise the "probe unsupported" path. Registered under a unique test type.
type noHealthProvider struct{}

func (noHealthProvider) Name() string               { return "no-health" }
func (noHealthProvider) Models() []schema.ModelInfo { return nil }
func (noHealthProvider) Complete(context.Context, *providers.ProxyRequest) (*providers.ProxyResponse, error) {
	return nil, nil
}
func (noHealthProvider) Stream(context.Context, *providers.ProxyRequest) (iter.Seq2[*providers.StreamEvent, error], error) {
	return nil, nil
}

func init() {
	providers.Register("probe-test-nohealth", func(providers.Config) (providers.Provider, error) {
		return noHealthProvider{}, nil
	})
}

func doProbe(t *testing.T, h http.Handler, body string) (int, ProbeResult) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/admin/providers/test", strings.NewReader(body))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	var res ProbeResult
	_ = json.Unmarshal(rr.Body.Bytes(), &res)
	return rr.Code, res
}

func TestProbe_405WhenStoreDisabled(t *testing.T) {
	code, _ := doProbe(t, ProbeHandler(false, nil), `{"type":"anthropic"}`)
	if code != http.StatusMethodNotAllowed {
		t.Fatalf("want 405, got %d", code)
	}
}

func TestProbe_InlineSecretRejected(t *testing.T) {
	code, _ := doProbe(t, ProbeHandler(true, nil), `{"type":"openai_compatible","api_key":"sk-leak"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("inline api_key must be 400, got %d", code)
	}
}

func TestProbe_UnsupportedCapability(t *testing.T) {
	code, res := doProbe(t, ProbeHandler(true, nil), `{"type":"probe-test-nohealth"}`)
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d", code)
	}
	if res.OK || !strings.Contains(res.Detail, "unsupported") {
		t.Fatalf("want unsupported result, got %+v", res)
	}
}

func TestProbe_OK_And_Sanitized(t *testing.T) {
	t.Setenv("PROBE_TEST_KEY", "super-secret-do-not-leak")
	// Upstream returns 401 so we exercise the sanitized non-OK detail path.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	// allowlist = the test server host, proving the allow path works.
	host := strings.TrimPrefix(srv.URL, "http://")
	host = host[:strings.IndexByte(host, ':')]
	body := `{"type":"openai_compatible","base_url":"` + srv.URL + `","api_key_ref":{"env":"PROBE_TEST_KEY"}}`
	code, res := doProbe(t, ProbeHandler(true, []string{host}), body)
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d", code)
	}
	if res.OK {
		t.Fatalf("401 upstream must be not-OK, got %+v", res)
	}
	if strings.Contains(res.Detail, "super-secret-do-not-leak") {
		t.Fatalf("detail leaked the resolved secret: %q", res.Detail)
	}
}

// TestProbe_AuthHeaderBearerUsesAuthorizationHeader is the end-to-end proof for
// PR #13 review Finding 2: probing an OpenRouter-style draft provider
// (type=anthropic, auth_header=bearer) must send Authorization: Bearer, not
// x-api-key — otherwise the probe always reports "unhealthy" for a live
// provider, misleading the operator.
func TestProbe_AuthHeaderBearerUsesAuthorizationHeader(t *testing.T) {
	t.Setenv("PROBE_OR_KEY", "or-secret-value")
	var gotAuth, gotAPIKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAPIKey = r.Header.Get("x-api-key")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	host = host[:strings.IndexByte(host, ':')]
	body := `{"type":"anthropic","base_url":"` + srv.URL + `","api_key_ref":{"env":"PROBE_OR_KEY"},"auth_header":"bearer"}`
	code, res := doProbe(t, ProbeHandler(true, []string{host}), body)
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d", code)
	}
	if !res.OK {
		t.Fatalf("want a healthy probe, got %+v", res)
	}
	if gotAPIKey != "" {
		t.Fatalf("bearer mode must not send x-api-key, got %q", gotAPIKey)
	}
	if gotAuth != "Bearer or-secret-value" {
		t.Fatalf("Authorization = %q, want Bearer or-secret-value", gotAuth)
	}
}

func TestProbe_MetadataEndpointBlocked(t *testing.T) {
	body := `{"type":"openai_compatible","base_url":"http://169.254.169.254","api_key_ref":{"env":"PATH"}}`
	code, res := doProbe(t, ProbeHandler(true, nil), body)
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d", code)
	}
	if res.OK {
		t.Fatalf("probe to metadata endpoint must be not-OK, got %+v", res)
	}
}

func TestProbe_AllowlistViolationBlocked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	// allowlist excludes 127.0.0.1 → dial rejected → not-OK.
	body := `{"type":"openai_compatible","base_url":"` + srv.URL + `","api_key_ref":{"env":"PATH"}}`
	code, res := doProbe(t, ProbeHandler(true, []string{"only.example.com"}), body)
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d", code)
	}
	if res.OK {
		t.Fatalf("allowlist violation must be not-OK, got %+v", res)
	}
}

func TestProbe_TimeoutHonored(t *testing.T) {
	old := probeTimeout
	probeTimeout = 20 * time.Millisecond
	defer func() { probeTimeout = old }()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	host = host[:strings.IndexByte(host, ':')]
	body := `{"type":"openai_compatible","base_url":"` + srv.URL + `","api_key_ref":{"env":"PATH"}}`
	code, res := doProbe(t, ProbeHandler(true, []string{host}), body)
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d", code)
	}
	if res.OK {
		t.Fatalf("timed-out probe must be not-OK, got %+v", res)
	}
}
