package openaicompat

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/inferplane/inferplane/pkg/schema"
	"github.com/inferplane/inferplane/providers"
)

func TestCompleteForwardsOpenAIVerbatimWhenIngressOpenAI(t *testing.T) {
	var gotBody []byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"x","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`)
	}))
	defer up.Close()
	p, _ := factory(providers.Config{Type: "openai_compatible", BaseURL: up.URL, APIKey: "k"})
	// ingress openai, but Upstream differs from the client's model → model rewritten.
	raw := []byte(`{"model":"Qwen/Qwen2.5","messages":[{"role":"user","content":"hi"}]}`)
	clientRaw := []byte(`{"model":"qwen","messages":[{"role":"user","content":"hi"}]}`)
	resp, err := p.Complete(context.Background(), &providers.ProxyRequest{Model: "qwen", Upstream: "Qwen/Qwen2.5", RawBody: clientRaw, IngressProtocol: "openai"})
	if err != nil {
		t.Fatal(err)
	}
	// forwarded verbatim except the top-level model rewritten to Upstream.
	if string(gotBody) != string(raw) {
		t.Fatalf("openai ingress → openai provider must forward verbatim w/ model rewritten:\n got: %s\nwant: %s", gotBody, raw)
	}
	if resp.Parsed == nil || resp.Parsed.Usage == nil || *resp.Parsed.Usage.InputTokens != 5 {
		t.Fatalf("usage observation: %+v", resp.Parsed)
	}
}

func TestCompleteConvertsWhenIngressAnthropic(t *testing.T) {
	var gotBody []byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"x","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer up.Close()
	p, _ := factory(providers.Config{Type: "openai_compatible", BaseURL: up.URL})
	// anthropic ingress: Parsed is canonical; RawBody is Anthropic JSON (not OpenAI)
	cr := &schema.ChatRequest{Model: "claude-x", Messages: []schema.Message{{Role: "user", Content: []schema.ContentBlock{{Type: "text", Text: ptrS("hi")}}}}}
	_, err := p.Complete(context.Background(), &providers.ProxyRequest{Model: "claude-x", Upstream: "Qwen/Qwen2.5", Parsed: cr, RawBody: []byte(`{"model":"claude-x"}`), IngressProtocol: "anthropic"})
	if err != nil {
		t.Fatal(err)
	}
	// upstream must receive an OpenAI-shaped body (converted), with model=Upstream
	var m map[string]any
	if json.Unmarshal(gotBody, &m) != nil {
		t.Fatalf("upstream body not JSON: %s", gotBody)
	}
	if m["model"] != "Qwen/Qwen2.5" {
		t.Fatalf("model not rewritten to upstream: %v", m["model"])
	}
	if _, hasMessages := m["messages"]; !hasMessages {
		t.Fatalf("converted body missing messages: %s", gotBody)
	}
}

func ptrS(s string) *string { return &s }

func TestStreamForwardsOpenAISSE(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" + "data: [DONE]\n\n"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse)
	}))
	defer up.Close()
	p, _ := factory(providers.Config{Type: "openai_compatible", BaseURL: up.URL})
	seq, err := p.Stream(context.Background(), &providers.ProxyRequest{Model: "q", Upstream: "q", RawBody: []byte(`{"model":"q","stream":true}`), IngressProtocol: "openai", Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	var raw strings.Builder
	var sawContent bool
	for ev, e := range seq {
		if e != nil {
			t.Fatal(e)
		}
		raw.WriteString(string(ev.Raw))
		if ev.Chunk != nil {
			sawContent = true
		}
	}
	if !strings.Contains(raw.String(), `"content":"hi"`) || !strings.Contains(raw.String(), "[DONE]") {
		t.Fatalf("openai SSE not teed verbatim: %s", raw.String())
	}
	_ = sawContent
}

// A non-OpenAI ingress (Anthropic, Bedrock) tees RawBody to its client, so a
// 2xx completion must be re-rendered in Anthropic shape under the PUBLIC
// model name — while the OpenAI ingress keeps the verbatim OpenAI wire.
func TestCompleteRerendersRawBodyForCrossProtocolIngress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","model":"upstream-m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hey"}}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`))
	}))
	defer srv.Close()
	p := &provider{baseURL: srv.URL, client: srv.Client()}
	raw := `{"model":"public-m","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	var cr schema.ChatRequest
	if err := json.Unmarshal([]byte(raw), &cr); err != nil {
		t.Fatal(err)
	}
	req := &providers.ProxyRequest{Model: "public-m", Upstream: "upstream-m", RawBody: []byte(raw), Parsed: &cr, IngressProtocol: "bedrock"}
	resp, err := p.Complete(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(resp.RawBody, &body); err != nil {
		t.Fatal(err)
	}
	if body["type"] != "message" || body["model"] != "public-m" {
		t.Fatalf("cross-protocol RawBody not anthropic-shaped/public-model: type=%v model=%v", body["type"], body["model"])
	}

	// OpenAI ingress: verbatim OpenAI wire preserved.
	req.IngressProtocol = "openai"
	resp, err = p.Complete(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	body = map[string]any{}
	if err := json.Unmarshal(resp.RawBody, &body); err != nil {
		t.Fatal(err)
	}
	if body["object"] != "chat.completion" {
		t.Fatalf("openai ingress RawBody must stay verbatim OpenAI wire: %v", body)
	}
}

// An unparseable 2xx must FAIL on a non-OpenAI ingress instead of teeing
// through: Parsed would stay nil, the Bedrock ingress skips settle when it is
// (zero billing, no token counts in the audit record), and the client would
// get raw OpenAI JSON in answer to an Anthropic-shaped request. The OpenAI
// ingress is unaffected — it tees the same wire it asked for.
func TestCompleteUnparseableBodyFailsForCrossProtocolIngress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>502 from a load balancer</html>"))
	}))
	defer srv.Close()
	p := &provider{baseURL: srv.URL, client: srv.Client()}
	raw := `{"model":"public-m","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	var cr schema.ChatRequest
	if err := json.Unmarshal([]byte(raw), &cr); err != nil {
		t.Fatal(err)
	}
	req := &providers.ProxyRequest{Model: "public-m", Upstream: "upstream-m", RawBody: []byte(raw), Parsed: &cr, IngressProtocol: "bedrock"}
	if _, err := p.Complete(context.Background(), req); err == nil {
		t.Fatal("want an error for an unparseable 2xx on a bedrock ingress, got nil")
	}

	// OpenAI ingress: the body is the client's own wire, so it still tees.
	req.IngressProtocol = "openai"
	resp, err := p.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("openai ingress must still tee an unparsed body: %v", err)
	}
	if string(resp.RawBody) != "<html>502 from a load balancer</html>" {
		t.Fatalf("RawBody = %q", resp.RawBody)
	}
}

// A cross-protocol Stream must ask the upstream for usage in the final chunk
// (stream_options.include_usage) — without it vLLM emits no usage frame and
// every streamed request settles with zero billable tokens.
func TestStreamRequestsUsageForCrossProtocolIngress(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()
	p := &provider{baseURL: srv.URL, client: srv.Client()}
	raw := `{"model":"public-m","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	var cr schema.ChatRequest
	if err := json.Unmarshal([]byte(raw), &cr); err != nil {
		t.Fatal(err)
	}
	evs, err := p.Stream(context.Background(), &providers.ProxyRequest{
		Model: "public-m", Upstream: "upstream-m", RawBody: []byte(raw), Parsed: &cr, IngressProtocol: "bedrock",
	})
	if err != nil {
		t.Fatal(err)
	}
	for range evs {
	}
	if gotBody["stream"] != true {
		t.Errorf("stream flag missing: %v", gotBody["stream"])
	}
	so, ok := gotBody["stream_options"].(map[string]any)
	if !ok || so["include_usage"] != true {
		t.Errorf("stream_options.include_usage missing: %v", gotBody["stream_options"])
	}
}

// Review follow-up (PR #65): a nil Parsed on the conversion path must be a
// typed error, not a panic — the Bedrock-ingress path is a new caller
// surface, and the mantle sibling already guards this.
func TestCrossProtocolNilParsedIsErrorNotPanic(t *testing.T) {
	p := &provider{baseURL: "http://127.0.0.1:0"}
	req := &providers.ProxyRequest{Model: "m", Upstream: "u", IngressProtocol: "bedrock"}
	if _, err := p.buildBody(req, false); err == nil {
		t.Fatal("nil Parsed must error, not panic or succeed")
	}
}

// Review follow-up (PR #65): Complete fails closed when a 2xx body cannot be
// parsed for a non-openai ingress; Stream must match — a stream whose every
// frame fails ChunkToCanonical would otherwise exit cleanly with zero
// billable tokens.
func TestStreamFailsClosedWhenNothingParsesForCrossProtocolIngress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {not json\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()
	p := &provider{baseURL: srv.URL, client: srv.Client()}
	raw := `{"model":"public-m","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	var cr schema.ChatRequest
	if err := json.Unmarshal([]byte(raw), &cr); err != nil {
		t.Fatal(err)
	}
	evs, err := p.Stream(context.Background(), &providers.ProxyRequest{
		Model: "public-m", Upstream: "upstream-m", RawBody: []byte(raw), Parsed: &cr, IngressProtocol: "bedrock",
	})
	if err != nil {
		t.Fatal(err)
	}
	var sawErr bool
	for ev, serr := range evs {
		if serr != nil {
			sawErr = true
		}
		_ = ev
	}
	if !sawErr {
		t.Fatal("a cross-protocol stream with zero parseable frames must surface an error, not end cleanly")
	}
}

// An upstream that dies before any parseable frame yields ONE error (the IO
// error itself). A consumer that keeps ranging past it must not receive the
// synthetic no-parseable-frames error stacked on top.
func TestStreamDoesNotDoubleErrorAfterUpstreamIOFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// Claim a body longer than what is sent, then drop the connection:
		// the client reader gets an unexpected-EOF style transport error.
		w.Header().Set("Content-Length", "1000")
		_, _ = w.Write([]byte("data: {not json\n\n"))
	}))
	defer srv.Close()
	p := &provider{baseURL: srv.URL, client: srv.Client()}
	raw := `{"model":"public-m","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	var cr schema.ChatRequest
	if err := json.Unmarshal([]byte(raw), &cr); err != nil {
		t.Fatal(err)
	}
	evs, err := p.Stream(context.Background(), &providers.ProxyRequest{
		Model: "public-m", Upstream: "upstream-m", RawBody: []byte(raw), Parsed: &cr, IngressProtocol: "bedrock",
	})
	if err != nil {
		t.Fatal(err)
	}
	var errs int
	for _, serr := range evs { // keep ranging past errors on purpose
		if serr != nil {
			errs++
		}
	}
	if errs != 1 {
		t.Fatalf("got %d errors, want exactly 1 (the IO error, no synthetic duplicate)", errs)
	}
}

// Native OpenAI-ingress streaming without stream_options.include_usage: the
// upstream body must gain it (order-preserving splice — zero-billing guard,
// review finding PR #65 round 4), the usage must still be OBSERVED for
// settlement, and the usage-only frame must NOT reach the client tee — the
// client opted out, and its empty choices array crashes naive indexing.
func TestStreamInjectsAndStripsUsageForNativeOpenAIIngress(t *testing.T) {
	var upstreamBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
				"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
				"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2}}\n\n" +
				"data: [DONE]\n\n"))
	}))
	defer srv.Close()
	p := &provider{baseURL: srv.URL, client: srv.Client()}
	raw := `{"model":"public-m","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	evs, err := p.Stream(context.Background(), &providers.ProxyRequest{
		Model: "public-m", Upstream: "upstream-m", RawBody: []byte(raw), IngressProtocol: "openai",
	})
	if err != nil {
		t.Fatal(err)
	}
	var teed []byte
	var sawUsage bool
	for ev, serr := range evs {
		if serr != nil {
			t.Fatal(serr)
		}
		teed = append(teed, ev.Raw...)
		if ev.Chunk != nil && ev.Chunk.Usage != nil {
			sawUsage = true
		}
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(upstreamBody, &top); err != nil {
		t.Fatal(err)
	}
	if string(top["stream_options"]) != `{"include_usage":true}` {
		t.Errorf("upstream stream_options = %s, want injected include_usage", top["stream_options"])
	}
	if !strings.HasPrefix(string(upstreamBody), `{"model":"upstream-m","stream":true`) {
		t.Errorf("injection must preserve top-level key order: %s", upstreamBody)
	}
	if !sawUsage {
		t.Error("usage frame must still be observed for settlement")
	}
	if strings.Contains(string(teed), `"usage"`) || strings.Contains(string(teed), `"choices":[]`) {
		t.Errorf("injected usage-only frame leaked to the client tee: %s", teed)
	}
	if !strings.Contains(string(teed), "data: [DONE]") {
		t.Errorf("terminal [DONE] must still tee: %s", teed)
	}

	// A client that ASKED for usage keeps the frame on its tee, byte-verbatim.
	raw = `{"model":"public-m","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`
	evs, err = p.Stream(context.Background(), &providers.ProxyRequest{
		Model: "public-m", Upstream: "upstream-m", RawBody: []byte(raw), IngressProtocol: "openai",
	})
	if err != nil {
		t.Fatal(err)
	}
	teed = nil
	for ev, serr := range evs {
		if serr != nil {
			t.Fatal(serr)
		}
		teed = append(teed, ev.Raw...)
	}
	if !strings.Contains(string(teed), `"usage"`) {
		t.Errorf("opted-in client must receive the usage frame: %s", teed)
	}
}

// Cross-protocol callers get include_usage injected unconditionally — the
// resulting usage-only frame must carry NO Raw: the Bedrock ingress ignores
// Raw, so stripping the injected usage-only frame's Raw is a harmless
// belt-and-braces there (review finding, PR #65 round 5). The anthropic
// ingress no longer takes this strip path at all — it gets a full re-render
// instead (C1, TestStreamRendersAnthropicSSEForAnthropicIngress below).
func TestStreamStripsInjectedUsageRawForBedrockIngress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
				"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
				"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2}}\n\n" +
				"data: [DONE]\n\n"))
	}))
	defer srv.Close()
	p := &provider{baseURL: srv.URL, client: srv.Client()}
	raw := `{"model":"public-m","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	var cr schema.ChatRequest
	if err := json.Unmarshal([]byte(raw), &cr); err != nil {
		t.Fatal(err)
	}
	evs, err := p.Stream(context.Background(), &providers.ProxyRequest{
		Model: "public-m", Upstream: "upstream-m", RawBody: []byte(raw), Parsed: &cr, IngressProtocol: "bedrock",
	})
	if err != nil {
		t.Fatal(err)
	}
	var sawUsage bool
	for ev, serr := range evs {
		if serr != nil {
			t.Fatal(serr)
		}
		if ev.Chunk != nil && ev.Chunk.Usage != nil {
			sawUsage = true
			if ev.Raw != nil {
				t.Errorf("injected usage-only frame kept Raw on a cross-protocol stream: %s", ev.Raw)
			}
		}
	}
	if !sawUsage {
		t.Error("usage frame must still be observed for settlement")
	}
}

// TestStreamRendersAnthropicSSEForAnthropicIngress is C1's contract: the
// anthropic ingress tees ev.Raw verbatim, so on this provider's OpenAI wire
// every frame's Raw must be REAL Anthropic SSE — the full frame vocabulary,
// no bare OpenAI JSON lines, no "data: [DONE]" terminator.
func TestStreamRendersAnthropicSSEForAnthropicIngress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
				"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
				"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2}}\n\n" +
				"data: [DONE]\n\n"))
	}))
	defer srv.Close()
	p := &provider{baseURL: srv.URL, client: srv.Client()}
	raw := `{"model":"public-m","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	var cr schema.ChatRequest
	if err := json.Unmarshal([]byte(raw), &cr); err != nil {
		t.Fatal(err)
	}
	evs, err := p.Stream(context.Background(), &providers.ProxyRequest{
		Model: "public-m", Upstream: "upstream-m", RawBody: []byte(raw), Parsed: &cr, IngressProtocol: "anthropic",
	})
	if err != nil {
		t.Fatal(err)
	}
	var tee strings.Builder
	for ev, serr := range evs {
		if serr != nil {
			t.Fatal(serr)
		}
		if ev != nil && ev.Raw != nil {
			tee.Write(ev.Raw)
		}
	}
	out := tee.String()
	if !strings.HasPrefix(out, "event: message_start") {
		t.Errorf("tee must start with event: message_start, got: %.80s", out)
	}
	for _, want := range []string{
		"event: content_block_start",
		"event: content_block_delta",
		"event: content_block_stop",
		"event: message_delta",
		"event: message_stop",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("tee missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "data: [DONE]") {
		t.Errorf("tee must not contain the OpenAI [DONE] terminator:\n%s", out)
	}
	if strings.Contains(out, `"choices"`) {
		t.Errorf("tee must not contain OpenAI-wire JSON:\n%s", out)
	}
}

// A parseable 2xx that omits usage must FAIL (C2): Settle no-ops on nil
// usage, so serving it would bill zero and audit like a free model — on
// every ingress, native openai included.
func TestCompleteMissingUsageFails(t *testing.T) {
	const withUsage = `{"id":"x","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`
	const noUsage = `{"id":"x","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`
	cases := []struct {
		name    string
		body    string
		ingress string
		wantErr bool
	}{
		{"with usage, openai ingress", withUsage, "openai", false},
		{"with usage, anthropic ingress", withUsage, "anthropic", false},
		{"no usage, openai ingress", noUsage, "openai", true},
		{"no usage, anthropic ingress", noUsage, "anthropic", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			p := &provider{baseURL: srv.URL, client: srv.Client()}
			raw := `{"model":"public-m","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
			var cr schema.ChatRequest
			if err := json.Unmarshal([]byte(raw), &cr); err != nil {
				t.Fatal(err)
			}
			resp, err := p.Complete(context.Background(), &providers.ProxyRequest{
				Model: "public-m", Upstream: "upstream-m", RawBody: []byte(raw), Parsed: &cr, IngressProtocol: tc.ingress,
			})
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if resp.Parsed == nil || resp.Parsed.Usage == nil {
					t.Fatalf("usage must be populated: %+v", resp.Parsed)
				}
				return
			}
			if err == nil {
				t.Fatal("want an error for a 2xx with no usage, got nil")
			}
			var ue *providers.UpstreamError
			if !errors.As(err, &ue) || ue.StatusCode != 502 {
				t.Fatalf("want *UpstreamError 502, got %v", err)
			}
			if resp != nil {
				t.Fatalf("no response may be returned alongside the refusal: %+v", resp)
			}
		})
	}
}
