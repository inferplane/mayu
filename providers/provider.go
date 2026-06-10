// Package providers defines the Provider interface and the transport types
// the gateway uses to proxy a request to an upstream LLM API. The canonical
// schema (pkg/schema) is kept pure; ProxyRequest/ProxyResponse/StreamEvent
// are transport wrappers that also carry the ORIGINAL upstream bytes, so the
// gateway can forward them verbatim and preserve the prompt-cache prefix
// (design doc §4.4) while still observing parsed content for governance.
package providers

import (
	"context"
	"iter"
	"net/http"

	"github.com/inferplane/inferplane/pkg/schema"
)

// ProxyRequest is one inbound request resolved to a target. RawBody is what
// gets sent upstream UNMODIFIED — the gateway parses Parsed only to route and
// observe, never to re-serialize the request (cache invariant, §4.4).
type ProxyRequest struct {
	Model    string              // resolved model name (routing/observation)
	Parsed   *schema.ChatRequest // parsed for inspection; do NOT re-serialize for upstream
	RawBody  []byte              // original request bytes → forwarded verbatim
	Headers  http.Header         // anthropic-version / anthropic-beta passthrough
	Stream   bool                // req.stream
	Upstream string              // target model id at the upstream (may differ from Model)
}

// ProxyResponse is a non-streaming upstream response. RawBody is teed to the
// client verbatim; Parsed is for observation (usage → audit/quota in M3/M5).
type ProxyResponse struct {
	StatusCode int
	Headers    http.Header
	RawBody    []byte
	Parsed     *schema.ChatResponse // nil if status != 2xx or body not parseable
}

// StreamEvent is one upstream SSE event. Raw is the exact event bytes
// (incl. "event:"/"data:" lines + blank-line terminator) teed to the client;
// Chunk is the parsed observation (nil for events with no JSON data payload,
// e.g. comment-only keepalives). This wrapper is why Stream yields
// *StreamEvent rather than *schema.ChatChunk: a single iter.Seq2 must carry
// BOTH the bytes to forward and the parsed view to observe.
type StreamEvent struct {
	Raw   []byte
	Chunk *schema.ChatChunk
}

// Provider proxies canonical requests to one upstream. New providers implement
// this in their own package; adding one touches providers/<name>/ + one line
// in registry.go and nothing in the core (design doc §8).
type Provider interface {
	Name() string
	Models() []schema.ModelInfo
	Complete(ctx context.Context, req *ProxyRequest) (*ProxyResponse, error)
	Stream(ctx context.Context, req *ProxyRequest) (iter.Seq2[*StreamEvent, error], error)
}

// TokenCounter is an optional capability. Providers that can count tokens
// upstream implement it; count_tokens falls back to an estimator otherwise
// (design doc §3.1, §10 #1).
type TokenCounter interface {
	CountTokens(ctx context.Context, req *ProxyRequest) (int64, error)
}

// Config is the per-provider settings slice the registry hands to a factory.
// Kept minimal for M2; providers read what they need.
type Config struct {
	Type     string // "anthropic" | (M4) "bedrock" | (M5) "openai_compatible"
	BaseURL  string // upstream base, e.g. https://api.anthropic.com
	APIKey   string // resolved secret (never logged)
	Models   []schema.ModelInfo
	Settings map[string]string // provider-specific extras
}
