// Package openairesponses provides the native Responses transport. It is
// intentionally Responses-only; conversion to other protocols belongs to the
// shared ingress adapter.
package openairesponses

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"iter"
	"net/http"
	"net/url"
	"strings"

	"github.com/inferplane/inferplane/internal/responses"
	"github.com/inferplane/inferplane/pkg/schema"
	"github.com/inferplane/inferplane/providers"
)

func init() { providers.Register("openai_responses", factory) }

type provider struct {
	endpoint, apiKey string
	client           *http.Client
}

func factory(cfg providers.Config) (providers.Provider, error) {
	base := strings.TrimRight(cfg.BaseURL, "/")
	if base == "" {
		base = "https://api.openai.com"
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || u.Scheme != "https" && u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("openai_responses: invalid base URL")
	}
	if strings.HasSuffix(base, "/v1") {
		base += "/responses"
	} else {
		base += "/v1/responses"
	}
	client := http.Client{}
	if cfg.HTTPClient != nil {
		client = *cfg.HTTPClient
	}
	// A configured upstream cannot redirect a credential-bearing request to
	// another destination. The caller's transport/timeout remain intact.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &provider{endpoint: base, apiKey: cfg.APIKey, client: &client}, nil
}

func (p *provider) Name() string                        { return "openai_responses" }
func (p *provider) Models() []schema.ModelInfo          { return nil }
func (p *provider) SupportsIngress(ingress string) bool { return ingress == "responses" }

func (p *provider) request(ctx context.Context, req *providers.ProxyRequest, stream bool) (*http.Request, error) {
	if req == nil || !p.SupportsIngress(req.IngressProtocol) {
		return nil, errors.New("openai_responses: unsupported ingress")
	}
	if _, err := responses.RequestToCanonical(req.RawBody); err != nil {
		return nil, err
	}
	var opts struct {
		Stream bool `json:"stream"`
	}
	if json.Unmarshal(req.RawBody, &opts) != nil || opts.Stream != stream {
		return nil, errors.New("openai_responses: stream operation does not match request")
	}
	body, err := rewriteModel(req.RawBody, req.Upstream)
	if err != nil {
		return nil, err
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("openai_responses: invalid upstream request")
	}
	r.Header.Set("Content-Type", "application/json")
	if stream {
		r.Header.Set("Accept", "text/event-stream")
	}
	if p.apiKey != "" {
		r.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	return r, nil
}

func rewriteModel(raw []byte, model string) ([]byte, error) {
	if model == "" {
		return nil, responses.ErrInvalid
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if _, err := dec.Token(); err != nil {
		return nil, responses.ErrInvalid
	}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return nil, responses.ErrInvalid
		}
		start := int(dec.InputOffset())
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, responses.ErrInvalid
		}
		end := int(dec.InputOffset())
		if key != "model" {
			continue
		}
		var current string
		if json.Unmarshal(value, &current) != nil {
			return nil, responses.ErrInvalid
		}
		if current == model {
			return raw, nil
		}
		for start < end && (raw[start] == ':' || raw[start] == ' ' || raw[start] == '\t' || raw[start] == '\r' || raw[start] == '\n') {
			start++
		}
		replacement, _ := json.Marshal(model)
		out := make([]byte, 0, len(raw)+len(replacement))
		out = append(out, raw[:start]...)
		out = append(out, replacement...)
		out = append(out, raw[end:]...)
		return out, nil
	}
	return nil, responses.ErrInvalid
}

func safeHeaders(in http.Header) http.Header {
	out := http.Header{}
	for _, key := range []string{"Content-Type", "Retry-After", "X-Request-Id"} {
		if value := in.Get(key); value != "" {
			out.Set(key, value)
		}
	}
	return out
}

func upstreamError(status int, header http.Header) *providers.UpstreamError {
	return &providers.UpstreamError{StatusCode: status, Header: safeHeaders(header), Body: []byte(`{"error":{"type":"upstream_error","message":"Responses upstream request failed"}}`)}
}

func (p *provider) Complete(ctx context.Context, req *providers.ProxyRequest) (*providers.ProxyResponse, error) {
	r, err := p.request(ctx, req, false)
	if err != nil {
		return nil, err
	}
	resp, err := p.client.Do(r)
	if err != nil {
		return nil, transportError(ctx)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		failure := upstreamError(resp.StatusCode, resp.Header)
		return &providers.ProxyResponse{StatusCode: failure.StatusCode, Headers: failure.Header, RawBody: failure.Body}, nil
	}
	const maxResponse = 32 << 20
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil || len(raw) > maxResponse {
		return nil, upstreamError(http.StatusBadGateway, nil)
	}
	usage, usageErr := responses.ResponseUsage(raw)
	if usageErr != nil {
		return nil, upstreamError(http.StatusBadGateway, nil)
	}
	parsed, parseErr := responses.ResponseToCanonical(raw)
	var envelope struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(raw, &envelope) // ResponseUsage already validated the document.
	if parseErr != nil || envelope.Status != "completed" {
		// A failed/incomplete or unrenderable response may already have spent
		// tokens. Return a safe non-2xx observation rather than losing usage
		// in a generic error. The ingress must settle this attempt before it
		// considers fallback; none of the remote output/error is client-safe.
		failure := upstreamError(http.StatusBadGateway, resp.Header)
		return &providers.ProxyResponse{
			StatusCode: failure.StatusCode, Headers: failure.Header, RawBody: failure.Body,
			Parsed: &schema.ChatResponse{Model: req.Model, Usage: usage},
		}, nil
	}
	parsed.Model = req.Model
	return &providers.ProxyResponse{StatusCode: resp.StatusCode, Headers: safeHeaders(resp.Header), RawBody: raw, Parsed: parsed}, nil
}

func (p *provider) Stream(ctx context.Context, req *providers.ProxyRequest) (iter.Seq2[*providers.StreamEvent, error], error) {
	r, err := p.request(ctx, req, true)
	if err != nil {
		return nil, err
	}
	resp, err := p.client.Do(r)
	if err != nil {
		return nil, transportError(ctx)
	}
	if resp.StatusCode/100 != 2 {
		resp.Body.Close()
		return nil, upstreamError(resp.StatusCode, resp.Header)
	}
	return func(yield func(*providers.StreamEvent, error) bool) {
		defer resp.Body.Close()
		for event, err := range responses.ReadSSE(resp.Body, req.Model) {
			if err != nil && ctx.Err() != nil {
				err = ctx.Err()
			}
			if !yield(event, err) {
				return
			}
		}
	}, nil
}

func transportError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return errors.New("openai_responses: upstream transport failed")
}

var _ providers.Provider = (*provider)(nil)
