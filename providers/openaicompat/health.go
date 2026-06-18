package openaicompat

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/inferplane/inferplane/providers"
)

// HealthCheck probes the OpenAI-compatible upstream with a cheap GET
// {base_url}/v1/models using the gateway's resolved credential (ADR-014 D2).
// A 2xx is healthy; any other status is unhealthy with a SANITIZED detail
// (HTTP status only — never the key or ref). The caller bounds the timeout via
// ctx. A keyless upstream (e.g. local Ollama) simply sends no Authorization.
func (p *provider) HealthCheck(ctx context.Context) providers.HealthResult {
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/v1/models", nil)
	if err != nil {
		return providers.HealthResult{OK: false, Detail: "could not build probe request"}
	}
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey) // gateway's credential, never echoed back
	}

	resp, err := p.client.Do(req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
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
