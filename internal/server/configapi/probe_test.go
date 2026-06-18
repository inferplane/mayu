package configapi

import (
	"context"
	"encoding/json"
	"iter"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/inferplane/inferplane/pkg/schema"
	"github.com/inferplane/inferplane/providers"
	// Register the real providers so the probe path actually builds them and
	// exercises HealthCheck + the guarded client — without these the handler
	// short-circuits at "unknown provider type" and the tests pass vacuously.
	_ "github.com/inferplane/inferplane/providers/anthropic"
	_ "github.com/inferplane/inferplane/providers/openaicompat"
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

func TestProbe_DoesNotFollowRedirect_NoSecretLeak(t *testing.T) {
	t.Setenv("PROBE_REDIR_KEY", "redir-secret-key")
	// The redirect TARGET: if the probe followed the 302, this would be hit and
	// the anthropic x-api-key (which Go does NOT strip on cross-host redirects)
	// would leak here. It must never be reached.
	leakHit := false
	leak := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leakHit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer leak.Close()
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, leak.URL+"/v1/models", http.StatusFound)
	}))
	defer redir.Close()

	body := `{"type":"anthropic","base_url":"` + redir.URL + `","api_key_ref":{"env":"PROBE_REDIR_KEY"}}`
	code, res := doProbe(t, ProbeHandler(true, nil), body)
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d", code)
	}
	if leakHit {
		t.Fatal("probe followed the redirect — the api key could have leaked to the redirect target")
	}
	if res.OK {
		t.Fatalf("a 302 is not a healthy upstream, got %+v", res)
	}
}

func TestGuardedDial_BlocksMetadataAndEnforcesAllowlist(t *testing.T) {
	base := &net.Dialer{Timeout: time.Second}

	// Metadata endpoint (IP literal → resolves to itself, no network) is rejected
	// BEFORE any dial, distinguishing a guard block from a mere unreachable host.
	if _, err := guardedDial(nil, base)(context.Background(), "tcp", "169.254.169.254:80"); err != errProbeBlocked {
		t.Fatalf("metadata endpoint must be blocked, got %v", err)
	}

	// Allowlist set + host not in it → rejected before resolution.
	if _, err := guardedDial(map[string]bool{"ok.example": true}, base)(context.Background(), "tcp", "evil.example:80"); err != errProbeNotAllowed {
		t.Fatalf("non-allowlisted host must be rejected, got %v", err)
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
