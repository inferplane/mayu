package configapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/inferplane/inferplane/internal/config"
	"github.com/inferplane/inferplane/providers"
)

// probeTimeout bounds one connection probe end to end. It is a var (not const)
// so tests can shorten it.
var probeTimeout = 8 * time.Second

// blockedProbeIPs are addresses no legitimate LLM upstream uses; the SSRF guard
// rejects them at dial time (on the resolved IP) so DNS rebinding cannot bypass
// it (ADR-014 D2).
var blockedProbeIPs = []net.IP{net.ParseIP("169.254.169.254"), net.ParseIP("fd00:ec2::254")}

// ProbeResult is the JSON returned by the connection-test endpoint.
type ProbeResult struct {
	OK        bool   `json:"ok"`
	LatencyMS int64  `json:"latency_ms"`
	Detail    string `json:"detail"`
}

// ProbeHandler tests a DRAFT provider's upstream connectivity (ADR-014 D2). It
// accepts a ProviderWrite body (refs only — never a secret), resolves the ref
// server-side, builds a live provider with an SSRF-guarded HTTP client, and
// invokes its HealthChecker. It is STATELESS — no server-side cache (a draft
// test keyed by name would poison a saved provider's status; the console caches
// in sessionStorage instead). When the provider store is disabled it 405s like
// the write path.
//
// allowedHosts, when non-empty, restricts probe targets to those hostnames
// (probe.allowed_hosts config); empty ⇒ any host (the metadata endpoint is
// always blocked).
func ProbeHandler(storeEnabled bool, allowedHosts []string) http.Handler {
	allow := make(map[string]bool, len(allowedHosts))
	for _, h := range allowedHosts {
		allow[h] = true
	}
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if !storeEnabled {
			writeErr(rw, http.StatusMethodNotAllowed, "provider store not enabled; registration is config-driven (set provider_store to enable UI writes/tests, ADR-005/008)")
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxWriteBody))
		if err != nil {
			writeErr(rw, http.StatusBadRequest, "cannot read body")
			return
		}
		// "probe" is a placeholder name — the test never persists. ParseProviderWrite
		// rejects an inline api_key and validates the ref shape (§7).
		row, perr := ParseProviderWrite("probe", body)
		if perr != nil {
			writeErr(rw, http.StatusBadRequest, perr.Error())
			return
		}

		// Resolve the ref SERVER-SIDE — the client never sends a secret.
		var apiKey string
		if row.APIKeyRefEnv != "" || row.APIKeyRefFile != "" {
			apiKey, err = config.ResolveSecretRef(&config.SecretRef{Env: row.APIKeyRefEnv, File: row.APIKeyRefFile})
			if err != nil {
				// Sanitized: name the ref kind, never its value.
				writeProbeJSON(rw, ProbeResult{OK: false, Detail: "api_key_ref did not resolve (set the value in your secret store)"})
				return
			}
		}

		prov, err := providers.New(providers.Config{
			Type:       row.Type,
			BaseURL:    row.BaseURL,
			APIKey:     apiKey,
			Settings:   map[string]string{"region": row.Region, "auth_mode": row.AuthMode, "profile": row.AuthProfile},
			HTTPClient: guardedClient(allow),
		})
		if err != nil {
			writeProbeJSON(rw, ProbeResult{OK: false, Detail: "unknown provider type"})
			return
		}
		hc, ok := prov.(providers.HealthChecker)
		if !ok {
			writeProbeJSON(rw, ProbeResult{OK: false, Detail: "probe unsupported for this provider type"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
		defer cancel()
		res := hc.HealthCheck(ctx)
		writeProbeJSON(rw, ProbeResult{OK: res.OK, LatencyMS: res.LatencyMS, Detail: res.Detail})
	})
}

func writeProbeJSON(rw http.ResponseWriter, v ProbeResult) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(rw).Encode(v)
}

// guardedClient builds an *http.Client whose DialContext (1) rejects any host
// outside allow when allow is non-empty, and (2) rejects the cloud metadata
// endpoint always. The IP is validated at dial time and the connection is
// pinned to the validated IP (dialing it directly, not re-resolving), so DNS
// rebinding (TOCTOU) cannot bypass the guard.
func guardedClient(allow map[string]bool) *http.Client {
	base := &net.Dialer{Timeout: probeTimeout}
	return &http.Client{
		Timeout: probeTimeout,
		// Never follow redirects: a probe has no reason to, and following one
		// would forward a custom auth header (anthropic's x-api-key — which Go
		// does NOT strip on cross-host redirects, unlike Authorization) to a
		// redirect target the operator did not register. Treat the 3xx as the
		// response instead (ADR-014 D2, secret-exfil hardening).
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport:     &http.Transport{DialContext: guardedDial(allow, base)},
	}
}

// errProbeBlocked / errProbeNotAllowed are the SSRF-guard dial rejections,
// exported as sentinels so the guard can be unit-tested precisely (distinguishing
// a guard rejection from an ordinary unreachable host, which the result layer
// cannot).
var (
	errProbeBlocked    = fmt.Errorf("probe target blocked")
	errProbeNotAllowed = fmt.Errorf("probe target host not allowed")
)

// guardedDial is the SSRF-guarded DialContext: it rejects hosts outside allow
// (when non-empty), resolves the host, rejects the cloud metadata IP, and pins
// the connection to the validated IP (no re-resolution → TOCTOU-safe).
func guardedDial(allow map[string]bool, base *net.Dialer) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		if len(allow) > 0 && !allow[host] {
			return nil, errProbeNotAllowed
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		for _, ip := range ips {
			for _, b := range blockedProbeIPs {
				if b != nil && ip.IP.Equal(b) {
					return nil, errProbeBlocked
				}
			}
		}
		// Pin to the validated IP — no re-resolution (TOCTOU-safe).
		return base.DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
	}
}
