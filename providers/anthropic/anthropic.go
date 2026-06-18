// Package anthropic proxies to the Anthropic Messages API (api.anthropic.com).
// It forwards the request body verbatim (cache invariant §4.4), injects the
// gateway's own credential (§5.2), and parses responses into canonical types
// for observation. Registered as provider type "anthropic".
package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net/http"

	"github.com/inferplane/inferplane/pkg/schema"
	"github.com/inferplane/inferplane/providers"
)

func init() { providers.Register("anthropic", factory) }

type provider struct {
	baseURL string
	apiKey  string
	client  *http.Client
}

func factory(cfg providers.Config) (providers.Provider, error) {
	base := cfg.BaseURL
	if base == "" {
		base = "https://api.anthropic.com"
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{}
	}
	return &provider{baseURL: base, apiKey: cfg.APIKey, client: client}, nil
}

func (p *provider) Name() string { return "anthropic" }

func (p *provider) Models() []schema.ModelInfo { return nil } // M2: models come from config

func (p *provider) buildUpstream(ctx context.Context, path string, req *providers.ProxyRequest) (*http.Request, error) {
	u, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+path, bytes.NewReader(req.RawBody))
	if err != nil {
		return nil, err
	}
	for _, h := range []string{"Anthropic-Version", "Anthropic-Beta", "Content-Type"} {
		if v := req.Headers.Get(h); v != "" {
			u.Header.Set(h, v)
		}
	}
	if u.Header.Get("Content-Type") == "" {
		u.Header.Set("Content-Type", "application/json")
	}
	u.Header.Set("x-api-key", p.apiKey) // gateway's credential, never the client's
	return u, nil
}

func (p *provider) Complete(ctx context.Context, req *providers.ProxyRequest) (*providers.ProxyResponse, error) {
	u, err := p.buildUpstream(ctx, "/v1/messages", req)
	if err != nil {
		return nil, err
	}
	resp, err := p.client.Do(u)
	if err != nil {
		return nil, fmt.Errorf("anthropic: upstream call: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("anthropic: read upstream: %w", err)
	}
	out := &providers.ProxyResponse{StatusCode: resp.StatusCode, Headers: resp.Header, RawBody: body}
	if resp.StatusCode/100 == 2 {
		var parsed schema.ChatResponse
		if json.Unmarshal(body, &parsed) == nil {
			out.Parsed = &parsed
		}
	}
	return out, nil
}

func (p *provider) Stream(ctx context.Context, req *providers.ProxyRequest) (iter.Seq2[*providers.StreamEvent, error], error) {
	u, err := p.buildUpstream(ctx, "/v1/messages", req)
	if err != nil {
		return nil, err
	}
	u.Header.Set("Accept", "text/event-stream")
	resp, err := p.client.Do(u)
	if err != nil {
		return nil, fmt.Errorf("anthropic: upstream stream: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, &providers.UpstreamError{StatusCode: resp.StatusCode, Body: body, Header: resp.Header}
	}
	inner := readSSE(resp.Body)
	return func(yield func(*providers.StreamEvent, error) bool) {
		defer resp.Body.Close()
		for ev, err := range inner {
			if !yield(ev, err) {
				return
			}
		}
	}, nil
}

func (p *provider) CountTokens(ctx context.Context, req *providers.ProxyRequest) (int64, error) {
	u, err := p.buildUpstream(ctx, "/v1/messages/count_tokens", req)
	if err != nil {
		return 0, err
	}
	resp, err := p.client.Do(u)
	if err != nil {
		return 0, fmt.Errorf("anthropic: count_tokens call: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}
	if resp.StatusCode/100 != 2 {
		return 0, fmt.Errorf("anthropic: count_tokens status %d: %s", resp.StatusCode, body)
	}
	var out struct {
		InputTokens int64 `json:"input_tokens"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return 0, fmt.Errorf("anthropic: count_tokens parse: %w", err)
	}
	return out.InputTokens, nil
}

var (
	_ providers.Provider     = (*provider)(nil)
	_ providers.TokenCounter = (*provider)(nil)
)
