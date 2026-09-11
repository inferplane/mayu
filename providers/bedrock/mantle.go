// Bedrock Mantle egress — the real implementation of the `"mantle"` api kind
// ADR-022 deferred. Mantle (`https://bedrock-mantle.<region>.api.aws`, SigV4
// service "bedrock") serves Bedrock models through provider-native API shapes
// instead of InvokeModel/Converse, with strictly vendor-partitioned routes
// (each 400s on the others' models — probed live 2026-08-28):
//
//	anthropic.*        → POST /anthropic/v1/messages        (Anthropic Messages API)
//	openai.* / xai.*   → POST /openai/v1/chat/completions   (OpenAI Chat Completions)
//	every other vendor → POST /v1/chat/completions          (OpenAI Chat Completions)
//
// Some models (openai.gpt-5.4/-5.5) exist ONLY here, so the invoke_model
// fallback sent them to an endpoint that has never heard of them. Bedrock
// Guardrails do not apply on this path — Mantle has no guardrail parameter — so
// a request carrying an effective guardrail is refused before it gets here
// (bedrock.go's mantleGuardrailCheck), never served unguarded.
package bedrock

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	anthropicsse "github.com/inferplane/inferplane/internal/anthropic"
	"github.com/inferplane/inferplane/internal/openai"
	"github.com/inferplane/inferplane/pkg/schema"

	"github.com/inferplane/inferplane/providers"
)

// mantler is the seam Complete/Stream dispatch through, so tests can fake the
// mantle path the same way invoker/converser are faked.
type mantler interface {
	Complete(ctx context.Context, req *providers.ProxyRequest) (*providers.ProxyResponse, error)
	Stream(ctx context.Context, req *providers.ProxyRequest) (iter.Seq2[*providers.StreamEvent, error], error)
}

type mantleClient struct {
	baseURL string
	region  string
	creds   aws.CredentialsProvider
	signer  *v4.Signer
	client  *http.Client
}

func newMantleClient(baseURL, region string, creds aws.CredentialsProvider, client *http.Client) *mantleClient {
	if client == nil {
		// A zero-value client has no timeout at all, so a hung Mantle connection
		// would pin the request goroutine indefinitely. The bound goes on the
		// transport, not Client.Timeout: the latter also caps body reading, which
		// would truncate any SSE stream that outlives it.
		tr, ok := http.DefaultTransport.(*http.Transport)
		if ok {
			tr = tr.Clone()
		} else {
			tr = &http.Transport{}
		}
		tr.ResponseHeaderTimeout = 120 * time.Second
		client = &http.Client{Transport: tr}
	}
	return &mantleClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		region:  region,
		creds:   creds,
		signer:  v4.NewSigner(),
		client:  client,
	}
}

// mantlePathFor picks the vendor route for an upstream model id. The
// partitioning is exact, not a preference — a model sent to another vendor's
// route gets "isn't supported on this route" (400). The vendor is matched as
// a whole dot-separated segment (not a substring), so a geo prefix still
// routes ("us.anthropic.…") while an id that merely CONTAINS a vendor name
// ("notanthropic.…") cannot be captured by another vendor's route.
func mantlePathFor(upstream string) string {
	// Route splits exist WITHIN a vendor too: google.gemma-3-* answers on the
	// bare route while google.gemma-4-* only answers on /openai/v1 (probed
	// live 2026-09-02 — the bare route rejects it with "isn't supported on
	// this route", matching its model card's documented /openai/v1 endpoint).
	if strings.Contains(upstream, "gemma-4") {
		return "/openai/v1/chat/completions"
	}
	for _, seg := range strings.Split(upstream, ".") {
		switch seg {
		case "anthropic":
			return "/anthropic/v1/messages"
		case "openai", "xai":
			return "/openai/v1/chat/completions"
		}
	}
	return "/v1/chat/completions"
}

// toMantleAnthropicBody rewrites a Bedrock-ingress Anthropic body for Mantle's
// native Anthropic route: drop "anthropic_version" (an InvokeModel-only body
// field — the real Anthropic contract takes the version as a header), set the
// model (Bedrock ingress carries it in the URL, Mantle wants it in the body)
// and the stream flag (Bedrock selects streaming by OPERATION, Mantle by body
// field). Top-level-only rewrite, same as toInvokeBody: system/messages/tools
// values stay byte-identical so the prompt-cache prefix is preserved (§4.4).
func toMantleAnthropicBody(raw []byte, upstream string, stream bool) ([]byte, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, err
	}
	delete(top, "anthropic_version")
	model, err := json.Marshal(upstream)
	if err != nil {
		return nil, err
	}
	top["model"] = model
	if stream {
		top["stream"] = json.RawMessage("true")
	} else {
		delete(top, "stream")
	}
	return json.Marshal(top)
}

// mantleChatStripParams lists OpenAI-wire params a Mantle chat-completions
// model rejects with a 400 — the same evidence-based style as
// converseUnsupportedInference, in this wire's field names. gpt-5.6 rejects
// temperature (any value but its default)/top_p/stop; gpt-5.4/-5.5,
// xai.grok-4.3, and the bare-route vendors (deepseek/zai/moonshotai) accepted
// all of them when probed (2026-08-28).
//
// Deliberate asymmetry with converseUnsupportedInference's "xai." entry:
// grok-4.3 was probed ON THIS ROUTE and accepted every sampling param, while
// grok-4.6 — which rejects them all on Converse — has not been probed on
// Mantle. The allow-list posture means an unprobed model keeps its params
// (and 400s loudly if it turns out to reject them) rather than being
// silently mutated on a guess; if grok-4.6 or a later xai reasoning model
// lands on this route and rejects sampling params, probe it and add the
// entry then.
var mantleChatStripParams = []struct {
	match  string
	params []string
}{
	{"openai.gpt-5.6", []string{"temperature", "top_p", "stop"}},
}

// toMantleAnthropicBodyFromCanonical renders the canonical request onto the
// Anthropic Messages wire for a NON-Anthropic-wire ingress (today: openai).
// There RawBody is the ingress's native OpenAI JSON — OpenAI roles, function
// tool shapes — which Mantle's /anthropic route would reject or misparse, so
// the verbatim path above is wrong for it. The canonical schema IS the
// Anthropic shape (pkg/schema), so this is a marshal plus the same
// model/stream fixups toMantleAnthropicBody applies; the cache prefix is
// already lost to cross-protocol conversion, no byte-stability to preserve.
// max_tokens stays absent when the OpenAI client omitted it — the route's own
// "max_tokens: field required" beats fabricating a limit the client never set.
func toMantleAnthropicBodyFromCanonical(req *providers.ProxyRequest, stream bool) ([]byte, error) {
	if req.Parsed == nil {
		return nil, fmt.Errorf("bedrock mantle: request for %s has no parsed body", req.Upstream)
	}
	cr := *req.Parsed
	cr.Model = req.Upstream
	if stream {
		t := true
		cr.Stream = &t
	} else {
		cr.Stream = nil
	}
	return json.Marshal(&cr)
}

// toMantleChatBody renders the canonical request onto Mantle's OpenAI
// chat-completions wire: internal/openai's conversion plus three Mantle
// specifics — the model is the upstream id, max_tokens is renamed
// max_completion_tokens (the gpt-5.6 family rejects max_tokens outright and
// every probed Mantle model accepts the newer name), and streaming asks for
// include_usage so the final chunk carries billable token counts.
func toMantleChatBody(req *providers.ProxyRequest, upstream string, stream bool) ([]byte, error) {
	if req.Parsed == nil {
		return nil, fmt.Errorf("bedrock mantle: request for %s has no parsed body", upstream)
	}
	cr := *req.Parsed
	cr.Model = upstream
	cr.Stream = nil // re-added below from the operation, not the body
	var top map[string]json.RawMessage
	if err := json.Unmarshal(openai.CanonicalToRequest(&cr), &top); err != nil {
		return nil, err
	}
	if mt, has := top["max_tokens"]; has {
		top["max_completion_tokens"] = mt
		delete(top, "max_tokens")
	}
	if stream {
		top["stream"] = json.RawMessage("true")
		openai.EnsureIncludeUsage(top)
	} else {
		delete(top, "stream")
	}
	for _, e := range mantleChatStripParams {
		if !strings.Contains(upstream, e.match) {
			continue
		}
		for _, p := range e.params {
			if _, has := top[p]; has {
				delete(top, p)
				logStrippedParam("mantle", upstream, p)
			}
		}
	}
	return json.Marshal(top)
}

// do builds, signs (SigV4, service "bedrock"), and sends one Mantle request.
func (m *mantleClient) do(ctx context.Context, path string, body []byte, sse bool) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if strings.HasSuffix(path, "/messages") {
		req.Header.Set("anthropic-version", "2023-06-01")
	}
	if sse {
		req.Header.Set("Accept", "text/event-stream")
	}
	creds, err := m.creds.Retrieve(ctx)
	if err != nil {
		return nil, fmt.Errorf("bedrock mantle: credentials: %w", err)
	}
	sum := sha256.Sum256(body)
	if err := m.signer.SignHTTP(ctx, creds, req, hex.EncodeToString(sum[:]), "bedrock", m.region, time.Now()); err != nil {
		return nil, fmt.Errorf("bedrock mantle: sign: %w", err)
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("bedrock mantle: upstream call: %w", err)
	}
	return resp, nil
}

func (m *mantleClient) buildBody(req *providers.ProxyRequest, path string, stream bool) ([]byte, error) {
	if path == "/anthropic/v1/messages" {
		// The verbatim top-level rewrite assumes RawBody is Anthropic-wire —
		// true for the anthropic and bedrock ingresses, not for openai.
		if req.IngressProtocol == "openai" {
			return toMantleAnthropicBodyFromCanonical(req, stream)
		}
		return toMantleAnthropicBody(req.RawBody, req.Upstream, stream)
	}
	return toMantleChatBody(req, req.Upstream, stream)
}

func (m *mantleClient) Complete(ctx context.Context, req *providers.ProxyRequest) (*providers.ProxyResponse, error) {
	path := mantlePathFor(req.Upstream)
	body, err := m.buildBody(req, path, false)
	if err != nil {
		return nil, err
	}
	resp, err := m.do(ctx, path, body, false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("bedrock mantle: read upstream: %w", err)
	}
	out := &providers.ProxyResponse{StatusCode: resp.StatusCode, Headers: resp.Header, RawBody: raw}
	if resp.StatusCode/100 != 2 {
		// The Anthropic ingress tees non-2xx RawBody verbatim, and the chat
		// routes speak OpenAI — re-render their {"error":{...}} envelope in
		// Anthropic shape, same invariant as the success path. The anthropic
		// route's errors are already Anthropic-shaped and pass through.
		if path != "/anthropic/v1/messages" {
			out.RawBody = anthropicErrorBody(resp.StatusCode, raw)
		}
		return out, nil
	}
	// A 2xx body we cannot parse must NOT be forwarded: out.Parsed stays nil,
	// and the ingress skips settle/metering entirely when it is — the request
	// would bill nothing and audit identically to a genuinely free model
	// (ADR-030's zero-cost bug class). Fail the call instead.
	var parsed *schema.ChatResponse
	if path == "/anthropic/v1/messages" {
		var pv schema.ChatResponse
		if err := json.Unmarshal(raw, &pv); err != nil {
			return nil, synthError(502, "bedrock mantle: unparseable upstream response body")
		}
		parsed = &pv
	} else if pv, perr := openai.ResponseToCanonical(raw); perr != nil {
		return nil, synthError(502, "bedrock mantle: unparseable upstream response body")
	} else {
		parsed = pv
	}
	// A parseable 2xx that omits usage would settle as a free request on
	// every ingress (Settle no-ops when usage is nil) — refuse instead of
	// serving an unsettleable response, same posture as the unparseable
	// case just above (C2).
	if parsed.Usage == nil {
		return nil, synthError(502, "bedrock mantle: upstream 2xx carried no usage — refusing to serve an unsettleable response")
	}
	// Mantle answers under the UPSTREAM model id, but the client asked for the
	// public name (same rewrite completeConverse does). Re-rendering is lossless:
	// ChatResponse round-trips unknown fields through Extra. The Bedrock ingress
	// tees RawBody to the client, so the chat routes' OpenAI wire has to be
	// re-rendered in Anthropic shape — raw OpenAI JSON must never reach an
	// Anthropic-speaking client.
	parsed.Model = req.Model
	out.Parsed = parsed
	rendered, rerr := json.Marshal(parsed)
	if rerr != nil {
		return nil, synthError(502, "bedrock mantle: cannot render upstream response")
	}
	out.RawBody = rendered
	return out, nil
}

func (m *mantleClient) Stream(ctx context.Context, req *providers.ProxyRequest) (iter.Seq2[*providers.StreamEvent, error], error) {
	path := mantlePathFor(req.Upstream)
	body, err := m.buildBody(req, path, true)
	if err != nil {
		return nil, err
	}
	resp, err := m.do(ctx, path, body, true)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, &providers.UpstreamError{StatusCode: resp.StatusCode, Body: raw, Header: resp.Header}
	}
	var inner iter.Seq2[*providers.StreamEvent, error]
	chatRoute := path != "/anthropic/v1/messages"
	if chatRoute {
		inner = openai.ReadChatSSE(resp.Body, req.Model)
	} else {
		inner = anthropicsse.ReadSSE(resp.Body)
	}
	return func(yield func(*providers.StreamEvent, error) bool) {
		defer resp.Body.Close()
		// Fail-closed parity with Complete and openaicompat.Stream: the
		// Bedrock ingress re-renders from ev.Chunk, so a 200 stream whose
		// every frame fails to parse would otherwise end cleanly with zero
		// canonical frames — and settle zero billable tokens for a served
		// request (ADR-030's zero-cost class).
		isAnthropicChatIngress := chatRoute && req.IngressProtocol == "anthropic"
		var sawChunk, sawErr bool
		for ev, serr := range inner {
			if serr != nil {
				sawErr = true
			}
			if ev != nil && ev.Chunk != nil {
				sawChunk = true
				// Chat routes always inject include_usage (toMantleChatBody)
				// — strip the usage-only frame's Raw from the client tee,
				// same as openaicompat. The anthropic ingress instead gets a
				// full re-render below (its usage frame becomes a real
				// message_delta), so the strip applies to the others only.
				// Chunk stays for settlement either way.
				if chatRoute && !isAnthropicChatIngress && openai.IsUsageOnlyFrame(ev.Chunk) {
					ev.Raw = nil
				}
				// Echo the PUBLIC model name, matching Complete: streamed
				// message_start frames otherwise leak the internal upstream
				// id ("anthropic.claude-opus-5") — and the Raw bytes the
				// Anthropic ingress tees verbatim must be regenerated to
				// match, or only the re-rendering ingresses get the rewrite.
				if ev.Chunk.Message != nil && ev.Chunk.Message.Model != req.Model {
					ev.Chunk.Message.Model = req.Model
					if !isAnthropicChatIngress && len(ev.Raw) > 0 {
						var buf bytes.Buffer
						if schema.WriteAnthropicSSE(&buf, ev.Chunk) == nil {
							ev.Raw = buf.Bytes()
						} else {
							// Unreachable in practice (a bytes.Buffer write
							// never errors), but a stale Raw here still names
							// the upstream id and the Anthropic ingress tees
							// Raw verbatim — drop it rather than leak it.
							ev.Raw = nil
						}
					}
				}
				if isAnthropicChatIngress {
					// Anthropic-wire client on a Mantle CHAT route: the chat
					// wire is OpenAI SSE, but the anthropic ingress tees Raw
					// verbatim — re-render EVERY chunk as real Anthropic SSE,
					// including the usage-only message_delta and the
					// synthesized frames ReadChatSSE emits with Raw==nil. The
					// model rewrite above already landed before this render
					// reads the chunk (C1). The /anthropic/v1/messages route
					// (chatRoute false) never takes this branch: its wire is
					// already Anthropic-shaped.
					var buf bytes.Buffer
					if schema.WriteAnthropicSSE(&buf, ev.Chunk) == nil {
						ev.Raw = buf.Bytes()
					} else {
						ev.Raw = nil
					}
				}
			} else if ev != nil && isAnthropicChatIngress {
				// [DONE] terminator / unparseable-frame passthrough on a chat
				// route — an Anthropic client must never see an OpenAI-wire
				// line; message_stop (re-rendered above) is the terminator (C1).
				ev.Raw = nil
			}
			if !yield(ev, serr) {
				return
			}
		}
		// !sawErr: an upstream that died before any parseable frame already
		// yielded its own error — a consumer that keeps ranging past it must
		// not receive this synthetic one on top.
		if !sawChunk && !sawErr {
			yield(nil, fmt.Errorf("bedrock mantle: upstream stream for %s produced no parseable frames", req.Upstream))
		}
	}, nil
}

var _ mantler = (*mantleClient)(nil)

// anthropicErrorBody re-renders an OpenAI {"error":{...}} envelope in
// Anthropic shape, preserving the upstream message. Unparseable input gets a
// fixed message rather than being echoed (it may not be JSON at all).
//
// Echoing the message here does NOT violate errors.go's "never echo
// ErrorMessage()" rule, and the envelope shape is what makes that so: the
// ARN-bearing error class is the AWS front layer's (auth/throttle —
// AccessDeniedException naming the caller's assumed-role ARN), and those
// bodies are AWS-shaped ({"message": ...} + x-amzn-ErrorType), which leaves
// oai.Error.Message empty and falls back to the fixed string. Only a
// vendor-emitted OpenAI envelope (error.message nested one level down) is
// echoed — model-level errors like a rejected parameter, which the client
// needs verbatim to act on, the same reason the anthropic provider tees
// upstream error bodies unmodified.
func anthropicErrorBody(status int, raw []byte) []byte {
	msg := "bedrock mantle: upstream error"
	var oai struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &oai) == nil && oai.Error.Message != "" {
		msg = oai.Error.Message
	}
	body, _ := json.Marshal(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": anthropicErrType(status), "message": msg},
	})
	return body
}
