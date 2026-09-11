// Package openaicompat proxies to any OpenAI-compatible Chat Completions server
// (vLLM, Ollama, llm-d). Its native wire is the OpenAI protocol, so it applies
// the protocol-match forwarding rule (§3.3): when the ingress is also "openai"
// it forwards the client's RawBody VERBATIM (lossless, cache-safe) — only the
// top-level "model" field is rewritten to the upstream's model id; when the
// ingress is "anthropic" (or anything else) it converts the canonical request
// (req.Parsed) to OpenAI via internal/openai. Registered as "openai_compatible".
package openaicompat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net/http"

	"github.com/inferplane/inferplane/internal/openai"
	"github.com/inferplane/inferplane/pkg/schema"
	"github.com/inferplane/inferplane/providers"
)

func init() { providers.Register("openai_compatible", factory) }

type provider struct {
	baseURL string
	apiKey  string
	client  *http.Client
}

func factory(cfg providers.Config) (providers.Provider, error) {
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{}
	}
	return &provider{baseURL: cfg.BaseURL, apiKey: cfg.APIKey, client: client}, nil
}

func (p *provider) Name() string { return "openai_compatible" }

func (p *provider) Models() []schema.ModelInfo { return nil } // models come from config

// buildBody produces the upstream OpenAI Chat Completions body. For an "openai"
// ingress the client's RawBody is forwarded byte-for-byte except the top-level
// "model" field, which is rewritten to req.Upstream via a top-level-map rewrite
// (like bedrock's toInvokeBody) so every other byte — and thus the prompt-cache
// prefix — is preserved. For any other ingress the canonical request is
// converted to OpenAI wire (best-effort) and the model set to req.Upstream.
func (p *provider) buildBody(req *providers.ProxyRequest, stream bool) ([]byte, error) {
	if req.IngressProtocol == "openai" {
		// Verbatim path: the client speaks OpenAI, so the body forwards
		// byte-preserving — only the model is rewritten, plus ONE governed
		// exception: a streaming request whose client did not ask for
		// include_usage gets it injected (order-preserving splice, like the
		// model rewrite), or the upstream emits no usage frame, lastUsage
		// stays nil, and the request settles at zero tokens (ADR-030's
		// zero-cost class — the cross-protocol ingresses already inject it).
		// Stream() strips the resulting usage-only frame from the client tee
		// (injectedUsageOnly), so a client that never asked still never sees
		// one.
		body, err := rewriteModel(req.RawBody, req.Upstream)
		if err != nil || !stream || openai.StreamWantsUsage(req.RawBody) {
			return body, err
		}
		return ensureIncludeUsageRaw(body)
	}
	if req.Parsed == nil {
		// Same guard as the mantle sibling: CanonicalToRequest dereferences
		// the request, and the Bedrock ingress is a newer caller surface.
		return nil, fmt.Errorf("openaicompat: request for %s has no parsed body", req.Upstream)
	}
	converted := openai.CanonicalToRequest(req.Parsed)
	if stream {
		// A cross-protocol ingress selects streaming by OPERATION, not body
		// (Bedrock: /invoke-with-response-stream), so the flag must be
		// injected here — and include_usage with it, or vLLM emits no usage
		// frame and the stream settles with zero billable tokens.
		var top map[string]json.RawMessage
		if err := json.Unmarshal(converted, &top); err != nil {
			return nil, err
		}
		top["stream"] = json.RawMessage("true")
		openai.EnsureIncludeUsage(top)
		var err error
		if converted, err = json.Marshal(top); err != nil {
			return nil, err
		}
	}
	return rewriteModel(converted, req.Upstream)
}

// ensureIncludeUsageRaw sets stream_options.include_usage=true on a raw
// OpenAI request body while preserving every other byte and the top-level key
// ORDER (same discipline as rewriteModel — the §4.4 verbatim posture). An
// existing stream_options object has include_usage merged into its value
// span; a body without one gets the key appended before the closing brace.
func ensureIncludeUsageRaw(raw []byte) ([]byte, error) {
	start, end, ok, err := topLevelKeySpan(raw, "stream_options")
	if err != nil {
		return nil, err
	}
	if ok {
		so := map[string]json.RawMessage{}
		// A non-object value was invalid on this wire to begin with —
		// replaced wholesale, same posture as openai.EnsureIncludeUsage.
		_ = json.Unmarshal(raw[start:end], &so)
		so["include_usage"] = json.RawMessage("true")
		repl, merr := json.Marshal(so)
		if merr != nil {
			return nil, merr
		}
		out := make([]byte, 0, len(raw)-(end-start)+len(repl))
		out = append(out, raw[:start]...)
		out = append(out, repl...)
		out = append(out, raw[end:]...)
		return out, nil
	}
	// Append the key before the object's closing brace.
	i := len(raw) - 1
	for i >= 0 && isJSONSpace(raw[i]) {
		i--
	}
	if i < 0 || raw[i] != '}' {
		return nil, fmt.Errorf("openaicompat: request body is not a JSON object")
	}
	ins := []byte(`,"stream_options":{"include_usage":true}`)
	out := make([]byte, 0, len(raw)+len(ins))
	out = append(out, raw[:i]...)
	out = append(out, ins...)
	out = append(out, raw[i:]...)
	return out, nil
}

// rewriteModel sets the top-level "model" field to model while preserving every
// other byte — including top-level key ORDER — so the request stays verbatim and
// the prompt-cache prefix is untouched (§4.4). It locates the top-level model
// VALUE span with a streaming token scan and splices the new value in place,
// rather than re-marshalling a map (which would reorder keys). If model is empty
// the body is returned unchanged; if there is no top-level "model" key the body
// is returned unchanged (the upstream uses its own default).
func rewriteModel(raw []byte, model string) ([]byte, error) {
	if model == "" {
		return raw, nil
	}
	start, end, ok, err := topLevelKeySpan(raw, "model")
	if err != nil {
		return nil, err
	}
	if !ok {
		return raw, nil
	}
	repl, err := json.Marshal(model)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(raw)-(end-start)+len(repl))
	out = append(out, raw[:start]...)
	out = append(out, repl...)
	out = append(out, raw[end:]...)
	return out, nil
}

// topLevelKeySpan returns the byte offsets [start,end) of the top-level
// keyed VALUE within raw (a JSON object), or ok=false if absent. It uses a
// json.Decoder at object depth 1 so nested "model" keys (e.g. inside a tool
// definition) are not matched. After a key it reads the full value — descending
// through any nested composites with depth tracking — so InputOffset() lands at
// the true value end. The returned start is trimmed of leading whitespace so the
// splice is byte-exact.
func topLevelKeySpan(raw []byte, wantKey string) (start, end int, ok bool, err error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return 0, 0, false, err
	}
	if d, isDelim := tok.(json.Delim); !isDelim || d != '{' {
		return 0, 0, false, nil // not a top-level object
	}
	for dec.More() {
		keyTok, kerr := dec.Token()
		if kerr != nil {
			return 0, 0, false, kerr
		}
		key, _ := keyTok.(string)
		valStart := int(dec.InputOffset())
		if verr := skipValue(dec); verr != nil {
			return 0, 0, false, verr
		}
		valEnd := int(dec.InputOffset())
		if key == wantKey {
			// InputOffset after the key lands just past the closing quote, so
			// valStart includes the ":" separator and any surrounding spaces;
			// advance past both to point at the value's first byte.
			s := valStart
			for s < valEnd && (isJSONSpace(raw[s]) || raw[s] == ':') {
				s++
			}
			return s, valEnd, true, nil
		}
	}
	return 0, 0, false, nil
}

func isJSONSpace(b byte) bool { return b == ' ' || b == '\t' || b == '\n' || b == '\r' }

// skipValue consumes exactly one JSON value from dec, fully descending through
// nested objects/arrays so the decoder is positioned right after the value.
func skipValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if _, isDelim := tok.(json.Delim); !isDelim {
		return nil // scalar: a single token is the whole value
	}
	depth := 1 // entered an object or array
	for depth > 0 {
		t, terr := dec.Token()
		if terr != nil {
			return terr
		}
		if dd, ok := t.(json.Delim); ok {
			switch dd {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
	}
	return nil
}

func (p *provider) buildUpstream(ctx context.Context, req *providers.ProxyRequest, stream bool) (*http.Request, error) {
	body, err := p.buildBody(req, stream)
	if err != nil {
		return nil, fmt.Errorf("openaicompat: build body: %w", err)
	}
	u, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	u.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		u.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	return u, nil
}

// noUsageError is the fixed 502 for a parseable 2xx that omits usage (C2) —
// the message never echoes upstream content.
func noUsageError() *providers.UpstreamError {
	body, _ := json.Marshal(map[string]any{
		"error": map[string]string{"message": "openaicompat: upstream 2xx carried no usage — refusing to serve an unsettleable response"},
	})
	return &providers.UpstreamError{StatusCode: 502, Body: body}
}

func (p *provider) Complete(ctx context.Context, req *providers.ProxyRequest) (*providers.ProxyResponse, error) {
	u, err := p.buildUpstream(ctx, req, false)
	if err != nil {
		return nil, err
	}
	resp, err := p.client.Do(u)
	if err != nil {
		return nil, fmt.Errorf("openaicompat: upstream call: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("openaicompat: read upstream: %w", err)
	}
	// RawBody is the provider's native OpenAI wire for an OpenAI ingress
	// (teed verbatim). Any other ingress (Anthropic re-renders from Parsed;
	// Bedrock tees RawBody) gets an Anthropic-shaped re-render under the
	// PUBLIC model name, so raw OpenAI JSON never reaches an
	// Anthropic-speaking client. Non-2xx still returns the upstream body
	// (teeable) with Parsed nil.
	out := &providers.ProxyResponse{StatusCode: resp.StatusCode, Headers: resp.Header, RawBody: body}
	if resp.StatusCode/100 == 2 {
		if parsed, perr := openai.ResponseToCanonical(body); perr == nil {
			if parsed.Usage == nil {
				// A parseable 2xx that omits usage would settle as a free
				// request on EVERY ingress (Settle no-ops when usage is nil)
				// — refuse rather than serve an unsettleable response. The
				// error body is fixed and never echoes upstream content (C2).
				return nil, noUsageError()
			}
			out.Parsed = parsed
			if req.IngressProtocol != "openai" {
				parsed.Model = req.Model
				rendered, rerr := json.Marshal(parsed)
				if rerr != nil {
					return nil, fmt.Errorf("openaicompat: cannot render upstream response: %w", rerr)
				}
				out.RawBody = rendered
			}
		} else if req.IngressProtocol != "openai" {
			// An unparsed 2xx leaves Parsed nil, and the Bedrock ingress skips
			// settle entirely when it is: the request would bill nothing and
			// the client would get raw OpenAI JSON it cannot read.
			return nil, fmt.Errorf("openaicompat: re-render for %s ingress: %w", req.IngressProtocol, perr)
		}
	}
	return out, nil
}

func (p *provider) Stream(ctx context.Context, req *providers.ProxyRequest) (iter.Seq2[*providers.StreamEvent, error], error) {
	u, err := p.buildUpstream(ctx, req, true)
	if err != nil {
		return nil, err
	}
	u.Header.Set("Accept", "text/event-stream")
	resp, err := p.client.Do(u)
	if err != nil {
		return nil, fmt.Errorf("openaicompat: upstream stream: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, &providers.UpstreamError{StatusCode: resp.StatusCode, Body: body, Header: resp.Header}
	}
	inner := openai.ReadChatSSE(resp.Body, req.Model)
	return func(yield func(*providers.StreamEvent, error) bool) {
		defer resp.Body.Close()
		// Fail-closed parity with Complete: a cross-protocol ingress ignores
		// Raw and re-renders from Chunk, so a stream whose every frame fails
		// ChunkToCanonical would otherwise end cleanly with zero canonical
		// frames — and settle zero billable tokens (ADR-030's zero-cost
		// class). Surface an error instead of a silent empty stream.
		crossProtocol := req.IngressProtocol != "openai"
		// buildBody injected include_usage the client never asked for (the
		// zero-billing guard) — the resulting usage-only frame is observed
		// (Chunk stays, so settle sees real tokens) but stripped from the
		// client tee (Raw cleared). Native path: a client that opted out
		// must not receive a usage frame, and one with empty choices can
		// crash naive choices[0] indexing. Cross-protocol path: injection is
		// unconditional and an Anthropic-wire client must never see an
		// OpenAI-shaped line at all — the Bedrock ingress ignores Raw, but
		// the Anthropic ingress tees it verbatim.
		injectedUsage := crossProtocol || !openai.StreamWantsUsage(req.RawBody)
		isAnthropicIngress := req.IngressProtocol == "anthropic"
		var sawChunk, sawErr bool
		for ev, err := range inner {
			if err != nil {
				sawErr = true
			}
			if ev != nil && ev.Chunk != nil {
				sawChunk = true
				if !isAnthropicIngress && injectedUsage && openai.IsUsageOnlyFrame(ev.Chunk) {
					ev.Raw = nil
				}
				if isAnthropicIngress {
					// An Anthropic-wire client must see the Anthropic SSE
					// frame vocabulary, never raw OpenAI JSON — the anthropic
					// ingress tees Raw verbatim (unlike the bedrock ingress,
					// which re-frames from Chunk), so re-render EVERY chunk
					// (including the usage-only frame, which rides
					// message_delta on this wire, matching what the Converse
					// egress already sends) rather than the strip-Raw special
					// case above, which stays exactly as-is for the other
					// ingresses (C1).
					var buf bytes.Buffer
					if schema.WriteAnthropicSSE(&buf, ev.Chunk) == nil {
						ev.Raw = buf.Bytes()
					} else {
						ev.Raw = nil
					}
				}
			} else if ev != nil && isAnthropicIngress {
				// The [DONE] terminator (Raw="data: [DONE]\n\n", Chunk=nil)
				// and any unparseable-frame passthrough — an Anthropic client
				// must never see either; message_stop (synthesized with its
				// own Chunk, re-rendered above) is the real terminator (C1).
				ev.Raw = nil
			}
			if !yield(ev, err) {
				return
			}
		}
		// !sawErr: an upstream that died before any parseable frame already
		// yielded its own error — a consumer that keeps ranging past it must
		// not receive this synthetic one on top.
		if crossProtocol && !sawChunk && !sawErr {
			yield(nil, fmt.Errorf("openaicompat: upstream stream for %s produced no parseable frames", req.Upstream))
		}
	}, nil
}

var _ providers.Provider = (*provider)(nil)
