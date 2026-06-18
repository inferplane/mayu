package anthropic

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/inferplane/inferplane/providers"
)

// anthropicVersion is the API version sent on the health probe; /v1/models
// requires it. It is independent of the per-request Anthropic-Version a client
// may send (the probe carries no client request).
const anthropicVersion = "2023-06-01"

// HealthCheck probes the Anthropic upstream with a cheap GET /v1/models using
// the gateway's resolved credential (ADR-014 D2). A 2xx is healthy; any other
// status is unhealthy with a SANITIZED detail (HTTP status only — never the key
// or ref). The caller bounds the timeout via ctx.
func (p *provider) HealthCheck(ctx context.Context) providers.HealthResult {
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/v1/models", nil)
	if err != nil {
		return providers.HealthResult{OK: false, Detail: "could not build probe request"}
	}
	req.Header.Set("x-api-key", p.apiKey) // gateway's credential, never echoed back
	req.Header.Set("anthropic-version", anthropicVersion)

	resp, err := p.client.Do(req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		// ctx errors and transport errors: report the class, never the URL/key.
		if ctx.Err() != nil {
			return providers.HealthResult{OK: false, LatencyMS: latency, Detail: "probe timed out"}
		}
		return providers.HealthResult{OK: false, LatencyMS: latency, Detail: "upstream unreachable"}
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 == 2 {
		return providers.HealthResult{OK: true, LatencyMS: latency, Detail: "ok"}
	}
	return providers.HealthResult{
		OK:        false,
		LatencyMS: latency,
		Detail:    fmt.Sprintf("upstream returned HTTP %d", resp.StatusCode),
	}
}
