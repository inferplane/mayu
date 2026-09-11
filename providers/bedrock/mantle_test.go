package bedrock

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/inferplane/inferplane/pkg/schema"
	"github.com/inferplane/inferplane/providers"
)

// Mantle's routes are strictly vendor-partitioned (probed live 2026-08-28):
// anthropic.* only on /anthropic/v1/messages, openai.*/xai.* only on
// /openai/v1/chat/completions, every other vendor only on
// /v1/chat/completions — each rejects the others' models with a 400.
func TestMantlePathFor(t *testing.T) {
	cases := map[string]string{
		"anthropic.claude-opus-5":    "/anthropic/v1/messages",
		"anthropic.claude-haiku-4-5": "/anthropic/v1/messages",
		"openai.gpt-5.4":             "/openai/v1/chat/completions",
		"openai.gpt-5.6-sol":         "/openai/v1/chat/completions",
		"xai.grok-4.3":               "/openai/v1/chat/completions",
		"deepseek.v3.2":              "/v1/chat/completions",
		// Route splits WITHIN a vendor: gemma-3 lives on the bare route,
		// gemma-4 only answers on /openai/v1 (probed live 2026-09-02 —
		// the bare route 400s "isn't supported on this route", and the
		// model card documents the /openai/v1 endpoint).
		"google.gemma-3-27b-it": "/v1/chat/completions",
		"google.gemma-4-31b":    "/openai/v1/chat/completions",
		"zai.glm-5":             "/v1/chat/completions",
		"moonshotai.kimi-k2.5":  "/v1/chat/completions",
		// A geo prefix is a leading segment, still vendor-routed.
		"us.anthropic.claude-opus-5": "/anthropic/v1/messages",
		// Vendor match is per-SEGMENT: an id that merely contains a vendor
		// name inside a segment must not be captured by that vendor's route.
		"notanthropic.some-model":   "/v1/chat/completions",
		"myvendor.openai-compat.v1": "/v1/chat/completions",
	}
	for upstream, want := range cases {
		if got := mantlePathFor(upstream); got != want {
			t.Errorf("mantlePathFor(%q) = %q, want %q", upstream, got, want)
		}
	}
}

func TestMantleAnthropicBody(t *testing.T) {
	raw := []byte(`{"anthropic_version":"bedrock-2023-05-31","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	body, err := toMantleAnthropicBody(raw, "anthropic.claude-opus-5", true)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if _, has := out["anthropic_version"]; has {
		t.Error("anthropic_version must be dropped (a body field only InvokeModel takes)")
	}
	if out["model"] != "anthropic.claude-opus-5" {
		t.Errorf("model = %v", out["model"])
	}
	if out["stream"] != true {
		t.Errorf("stream = %v, want true", out["stream"])
	}
	if out["max_tokens"] != float64(64) {
		t.Errorf("max_tokens = %v", out["max_tokens"])
	}
}

func TestMantleChatBody(t *testing.T) {
	pr := parsedReq(t, `{"model":"x","max_tokens":64,"temperature":0.7,"top_p":0.9,"stop_sequences":["END"],"messages":[{"role":"user","content":"hi"}]}`)

	// gpt-5.4 keeps sampling params; max_tokens becomes max_completion_tokens.
	body, err := toMantleChatBody(pr, "openai.gpt-5.4", false)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if _, has := out["max_tokens"]; has {
		t.Error("max_tokens must be renamed to max_completion_tokens on mantle")
	}
	if out["max_completion_tokens"] != float64(64) {
		t.Errorf("max_completion_tokens = %v", out["max_completion_tokens"])
	}
	if out["model"] != "openai.gpt-5.4" {
		t.Errorf("model = %v", out["model"])
	}
	if out["temperature"] != 0.7 || out["top_p"] != 0.9 {
		t.Errorf("gpt-5.4 must keep sampling params: %v %v", out["temperature"], out["top_p"])
	}
	if _, has := out["stream"]; has {
		t.Errorf("non-streaming body must not carry stream: %v", out["stream"])
	}

	// gpt-5.6 rejects temperature/top_p/stop — they must be stripped.
	body, err = toMantleChatBody(pr, "openai.gpt-5.6-sol", true)
	if err != nil {
		t.Fatal(err)
	}
	out = map[string]any{}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"temperature", "top_p", "stop"} {
		if _, has := out[k]; has {
			t.Errorf("gpt-5.6: %q must be stripped", k)
		}
	}
	if out["stream"] != true {
		t.Errorf("stream = %v, want true", out["stream"])
	}
	if so, ok := out["stream_options"].(map[string]any); !ok || so["include_usage"] != true {
		t.Errorf("streaming body must request include_usage: %v", out["stream_options"])
	}
}

func parsedReq(t *testing.T, raw string) *providers.ProxyRequest {
	t.Helper()
	req := &providers.ProxyRequest{RawBody: []byte(raw)}
	req.Parsed = parseChat(t, raw)
	return req
}

func parseChat(t *testing.T, raw string) *schema.ChatRequest {
	t.Helper()
	var cr schema.ChatRequest
	if err := json.Unmarshal([]byte(raw), &cr); err != nil {
		t.Fatal(err)
	}
	return &cr
}

func blocksText(blocks []schema.ContentBlock) string {
	var b strings.Builder
	for _, blk := range blocks {
		if blk.Type == "text" && blk.Text != nil {
			b.WriteString(*blk.Text)
		}
	}
	return b.String()
}

func staticMantle(t *testing.T, srv *httptest.Server) *mantleClient {
	t.Helper()
	return newMantleClient(srv.URL, "us-east-1",
		aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "AKID", SecretAccessKey: "SECRET"}, nil
		}), srv.Client())
}

func TestMantleCompleteAnthropicPath(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"message","role":"assistant","model":"anthropic.claude-opus-5","content":[{"type":"text","text":"hey"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`))
	}))
	defer srv.Close()
	mc := staticMantle(t, srv)
	raw := `{"anthropic_version":"bedrock-2023-05-31","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	resp, err := mc.Complete(context.Background(), &providers.ProxyRequest{
		Model: "mantle.opus", Upstream: "anthropic.claude-opus-5",
		RawBody: []byte(raw), Parsed: parseChat(t, raw),
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/anthropic/v1/messages" {
		t.Errorf("path = %q", gotPath)
	}
	if !strings.Contains(gotAuth, "AWS4-HMAC-SHA256") {
		t.Errorf("request not SigV4 signed: %q", gotAuth)
	}
	if gotBody["model"] != "anthropic.claude-opus-5" {
		t.Errorf("upstream body model = %v", gotBody["model"])
	}
	if resp.StatusCode != 200 || resp.Parsed == nil || *resp.Parsed.Usage.OutputTokens != 2 {
		t.Fatalf("resp: %+v", resp)
	}
	// The client asked for the PUBLIC name; Mantle answered with the upstream
	// id. Both the parsed response and the teed RawBody must carry the public
	// one, or the client sees a model it never requested.
	if resp.Parsed.Model != "mantle.opus" {
		t.Errorf("parsed model = %q, want the public name mantle.opus", resp.Parsed.Model)
	}
	var teed map[string]any
	if err := json.Unmarshal(resp.RawBody, &teed); err != nil {
		t.Fatal(err)
	}
	if teed["model"] != "mantle.opus" {
		t.Errorf("teed body model = %v, want mantle.opus", teed["model"])
	}
	if teed["stop_reason"] != "end_turn" {
		t.Errorf("re-render lost stop_reason: %v", teed["stop_reason"])
	}
}

func TestMantleCompleteChatPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/openai/v1/chat/completions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","model":"openai.gpt-5.4","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hey"}}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`))
	}))
	defer srv.Close()
	mc := staticMantle(t, srv)
	raw := `{"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	resp, err := mc.Complete(context.Background(), &providers.ProxyRequest{
		Model: "mantle.gpt-5.4", Upstream: "openai.gpt-5.4",
		RawBody: []byte(raw), Parsed: parseChat(t, raw),
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || resp.Parsed == nil {
		t.Fatalf("resp: %+v", resp)
	}
	if got := blocksText(resp.Parsed.Content); got != "hey" {
		t.Errorf("text = %q", got)
	}
	if resp.Parsed.Usage == nil || *resp.Parsed.Usage.OutputTokens != 2 {
		t.Errorf("usage: %+v", resp.Parsed.Usage)
	}
	// The Bedrock ingress tees RawBody to the client, so a chat-route
	// response must be re-rendered in Anthropic shape under the PUBLIC model
	// name — raw OpenAI wire must never reach an Anthropic-speaking client.
	var body map[string]any
	if err := json.Unmarshal(resp.RawBody, &body); err != nil {
		t.Fatal(err)
	}
	if body["type"] != "message" || body["model"] != "mantle.gpt-5.4" {
		t.Errorf("RawBody not anthropic-shaped/public-model: type=%v model=%v", body["type"], body["model"])
	}
}

func TestMantleStreamChatPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["stream"] != true {
			t.Errorf("stream flag missing in upstream body: %v", body["stream"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"he\"}}]}\n\n" +
			"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
			"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\n" +
			"data: [DONE]\n\n"))
	}))
	defer srv.Close()
	mc := staticMantle(t, srv)
	raw := `{"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	evs, err := mc.Stream(context.Background(), &providers.ProxyRequest{
		Model: "mantle.gpt-5.4", Upstream: "openai.gpt-5.4",
		RawBody: []byte(raw), Parsed: parseChat(t, raw),
	})
	if err != nil {
		t.Fatal(err)
	}
	var sawText, sawUsage bool
	for ev, serr := range evs {
		if serr != nil {
			t.Fatal(serr)
		}
		if ev.Chunk == nil {
			continue
		}
		if strings.Contains(string(ev.Chunk.Delta), "he") {
			sawText = true
		}
		if ev.Chunk.Usage != nil && ev.Chunk.Usage.OutputTokens != nil {
			sawUsage = true
		}
	}
	if !sawText || !sawUsage {
		t.Fatalf("stream missing text(%v) or usage(%v)", sawText, sawUsage)
	}
}

// api "mantle" must actually route to the mantle client — the invoke_model
// fallback silently sent mantle-only models to the wrong endpoint.
func TestAPIForMantleNoLongerFallsBack(t *testing.T) {
	p := &provider{modelAPI: map[string]string{"openai.gpt-5.4": "mantle"}}
	if got := p.apiFor("openai.gpt-5.4"); got != "mantle" {
		t.Fatalf("apiFor = %q, want mantle", got)
	}
}

// A 2xx whose body we cannot parse must fail, not tee through: out.Parsed stays
// nil, and the ingress then skips settle entirely — the request would bill
// nothing and audit like a genuinely free model (ADR-030's zero-cost class).
func TestMantleCompleteUnparseableBodyFails(t *testing.T) {
	for _, tc := range []struct{ name, upstream, body string }{
		{"anthropic route", "anthropic.claude-opus-5", `not json at all`},
		{"chat route", "openai.gpt-5.4", `<html>gateway blurb</html>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			mc := staticMantle(t, srv)
			raw := `{"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
			resp, err := mc.Complete(context.Background(), &providers.ProxyRequest{
				Model: "mantle.m", Upstream: tc.upstream,
				RawBody: []byte(raw), Parsed: parseChat(t, raw),
			})
			if err == nil {
				t.Fatalf("unparseable 2xx returned success: %+v", resp)
			}
			var ue *providers.UpstreamError
			if !errors.As(err, &ue) || ue.StatusCode != 502 {
				t.Fatalf("want a 502 UpstreamError, got %v", err)
			}
		})
	}
}

// Mantle has no guardrail parameter, and the Bedrock ingress writes the
// request's guardrail id into the tamper-evident audit chain regardless — so a
// guarded model routed here would be served UNGUARDED and then attested as
// guarded. ADR-019 gives guardrails no per-team opt-out; refuse instead.
func TestMantleRefusesWhenAGuardrailIsConfigured(t *testing.T) {
	req := &providers.ProxyRequest{Model: "mantle.gpt", Upstream: "openai.gpt-5.4"}
	for _, tc := range []struct {
		name string
		p    *provider
	}{
		{"provider default", &provider{
			modelAPI:         map[string]string{"openai.gpt-5.4": "mantle"},
			defaultGuardrail: Guardrail{ID: "gr-1", Version: "2"},
		}},
		{"per-team override", &provider{
			modelAPI: map[string]string{"openai.gpt-5.4": "mantle"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := *req
			if tc.name == "per-team override" {
				r.GuardrailID = "gr-team"
			}
			if _, err := tc.p.Complete(context.Background(), &r); !isGuardrailRefusal(err) {
				t.Errorf("Complete: want a 400 refusal, got %v", err)
			}
			if _, err := tc.p.Stream(context.Background(), &r); !isGuardrailRefusal(err) {
				t.Errorf("Stream: want a 400 refusal, got %v", err)
			}
		})
	}
	// No guardrail anywhere: the mantle client is reached as before (nil `man`
	// would panic if the check short-circuited the wrong way).
	clean := &provider{modelAPI: map[string]string{"openai.gpt-5.4": "mantle"}, man: &panicMantler{}}
	if _, err := clean.Complete(context.Background(), req); err == nil || err.Error() != "reached mantle" {
		t.Errorf("unguarded request did not reach the mantle client: %v", err)
	}
}

func isGuardrailRefusal(err error) bool {
	var ue *providers.UpstreamError
	return errors.As(err, &ue) && ue.StatusCode == 400 && strings.Contains(string(ue.Body), "guardrail")
}

type panicMantler struct{}

func (panicMantler) Complete(context.Context, *providers.ProxyRequest) (*providers.ProxyResponse, error) {
	return nil, errors.New("reached mantle")
}

func (panicMantler) Stream(context.Context, *providers.ProxyRequest) (iter.Seq2[*providers.StreamEvent, error], error) {
	return nil, errors.New("reached mantle")
}

// Review follow-up (PR #65, round 2): a 200 SSE stream whose every frame
// fails to parse must surface an error, not end cleanly — otherwise the
// ingress settles zero billable tokens for a served stream (ADR-030's
// zero-cost class), same fail-closed rule as openaicompat.Stream.
func TestMantleStreamFailsClosedWhenNothingParses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {not json\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()
	mc := staticMantle(t, srv)
	raw := `{"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	evs, err := mc.Stream(context.Background(), &providers.ProxyRequest{
		Model: "mantle.gpt-5.4", Upstream: "openai.gpt-5.4",
		RawBody: []byte(raw), Parsed: parseChat(t, raw),
	})
	if err != nil {
		t.Fatal(err)
	}
	var sawErr bool
	for _, serr := range evs {
		if serr != nil {
			sawErr = true
		}
	}
	if !sawErr {
		t.Fatal("a mantle stream with zero parseable frames must surface an error, not end cleanly")
	}
}

// Local review of PR #65 (CONFIRMED): a non-2xx from a mantle CHAT route is an
// OpenAI-shaped {"error":{...}} envelope, and the Anthropic ingress tees
// RawBody verbatim for non-2xx — the error must be re-rendered in Anthropic
// shape, same as the success path.
func TestMantleChatErrorBodyRerenderedAnthropicShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited","type":"rate_limit_exceeded","code":"429"}}`))
	}))
	defer srv.Close()
	mc := staticMantle(t, srv)
	raw := `{"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	resp, err := mc.Complete(context.Background(), &providers.ProxyRequest{
		Model: "mantle.gpt-5.4", Upstream: "openai.gpt-5.4",
		RawBody: []byte(raw), Parsed: parseChat(t, raw),
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 429 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	var body struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(resp.RawBody, &body); err != nil || body.Type != "error" {
		t.Fatalf("non-2xx chat body must be Anthropic-shaped: %s", resp.RawBody)
	}
	if body.Error.Message != "rate limited" {
		t.Fatalf("upstream error message lost: %s", resp.RawBody)
	}
}

// Local review of PR #65 (CONFIRMED): streamed frames leaked the internal
// upstream model id ("openai.gpt-5.4") where Complete echoes the public name;
// on the chat routes the re-rendered message_start had model "" entirely.
func TestMantleStreamEchoesPublicModelName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"openai.gpt-5.4\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"}}]}\n\n" +
			"data: [DONE]\n\n"))
	}))
	defer srv.Close()
	mc := staticMantle(t, srv)
	raw := `{"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	evs, err := mc.Stream(context.Background(), &providers.ProxyRequest{
		Model: "mantle.gpt-5.4", Upstream: "openai.gpt-5.4",
		RawBody: []byte(raw), Parsed: parseChat(t, raw),
	})
	if err != nil {
		t.Fatal(err)
	}
	for ev, serr := range evs {
		if serr != nil || ev == nil || ev.Chunk == nil {
			continue
		}
		if ev.Chunk.Message != nil && ev.Chunk.Message.Model != "mantle.gpt-5.4" {
			t.Fatalf("frame leaks upstream model id: %q", ev.Chunk.Message.Model)
		}
	}
}

func TestMantleAnthropicStreamRewritesModelInChunkAndRaw(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"anthropic.claude-opus-5\",\"content\":[],\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}}\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer srv.Close()
	mc := staticMantle(t, srv)
	raw := `{"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	evs, err := mc.Stream(context.Background(), &providers.ProxyRequest{
		Model: "mantle.opus", Upstream: "anthropic.claude-opus-5",
		RawBody: []byte(raw), Parsed: parseChat(t, raw),
	})
	if err != nil {
		t.Fatal(err)
	}
	var sawStart bool
	for ev, serr := range evs {
		if serr != nil || ev == nil || ev.Chunk == nil || ev.Chunk.Message == nil {
			continue
		}
		sawStart = true
		if ev.Chunk.Message.Model != "mantle.opus" {
			t.Fatalf("Chunk leaks upstream model id: %q", ev.Chunk.Message.Model)
		}
		// The Anthropic ingress tees Raw verbatim — it must carry the public
		// name too, or the rewrite only covers the re-rendering ingresses.
		if strings.Contains(string(ev.Raw), "anthropic.claude-opus-5") || !strings.Contains(string(ev.Raw), "mantle.opus") {
			t.Fatalf("Raw leaks upstream model id: %s", ev.Raw)
		}
	}
	if !sawStart {
		t.Fatal("no message_start frame observed")
	}
}

// An OpenAI-ingress request routed to Mantle's Anthropic route must be
// re-rendered from the canonical form: RawBody is the ingress's native
// OpenAI JSON (OpenAI roles, {"type":"function"} tool shapes), which the
// route would reject or misparse — the verbatim top-level rewrite is only
// correct for the Anthropic-wire ingresses (anthropic, bedrock).
func TestMantleAnthropicBodyFromOpenAIIngress(t *testing.T) {
	var parsed schema.ChatRequest
	if err := json.Unmarshal([]byte(`{"model":"claude-opus-5","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`), &parsed); err != nil {
		t.Fatal(err)
	}
	req := &providers.ProxyRequest{
		Upstream:        "anthropic.claude-opus-5",
		IngressProtocol: "openai",
		// What the OpenAI ingress actually carries in RawBody — forwarding
		// this verbatim is the bug this test fences.
		RawBody: []byte(`{"model":"claude-opus-5","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f","parameters":{}}}]}`),
		Parsed:  &parsed,
	}
	m := &mantleClient{}
	body, err := m.buildBody(req, "/anthropic/v1/messages", true)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if string(out["model"]) != `"anthropic.claude-opus-5"` {
		t.Errorf("model = %s, want upstream id", out["model"])
	}
	if string(out["stream"]) != "true" {
		t.Errorf("stream = %s, want true", out["stream"])
	}
	if strings.Contains(string(body), `"function"`) {
		t.Errorf("OpenAI tool shape leaked into the Anthropic-route body: %s", body)
	}

	// Same route, bedrock ingress: the verbatim path stays byte-preserving —
	// the canonical render must not replace it (cache invariant).
	req.IngressProtocol = "bedrock"
	req.RawBody = []byte(`{"anthropic_version":"bedrock-2023-05-31","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	body, err = m.buildBody(req, "/anthropic/v1/messages", true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "anthropic_version") {
		t.Errorf("anthropic_version not dropped on the verbatim path: %s", body)
	}

	// No parsed body on the openai ingress is an error, never a garbled
	// forward.
	req.IngressProtocol = "openai"
	req.Parsed = nil
	if _, err := m.buildBody(req, "/anthropic/v1/messages", true); err == nil {
		t.Error("nil Parsed on openai ingress must fail, not forward OpenAI JSON")
	}
}

// TestMantleStreamRendersAnthropicSSEForAnthropicIngress is C1's contract on
// the Mantle chat routes: the anthropic ingress tees ev.Raw verbatim, so on
// this OpenAI-wire route every frame's Raw must be REAL Anthropic SSE — the
// full frame vocabulary, no bare OpenAI JSON lines, no "data: [DONE]".
func TestMantleStreamRendersAnthropicSSEForAnthropicIngress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"he\"}}]}\n\n" +
			"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
			"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\n" +
			"data: [DONE]\n\n"))
	}))
	defer srv.Close()
	mc := staticMantle(t, srv)
	raw := `{"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	evs, err := mc.Stream(context.Background(), &providers.ProxyRequest{
		Model: "mantle.gpt-5.4", Upstream: "openai.gpt-5.4",
		RawBody: []byte(raw), Parsed: parseChat(t, raw), IngressProtocol: "anthropic",
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
	// The public-model echo must survive the re-render: message_start carries
	// the model the CLIENT asked for, never the internal upstream id.
	if !strings.Contains(out, `"model":"mantle.gpt-5.4"`) || strings.Contains(out, `openai.gpt-5.4`) {
		t.Errorf("message_start must carry the public model name, not the upstream id:\n%s", out)
	}
}

// A parseable 2xx that omits usage must fail, same posture as the unparseable
// case above (C2): Settle no-ops on nil usage, so the request would bill zero.
func TestMantleCompleteMissingUsageFails(t *testing.T) {
	const anthropicOK = `{"type":"message","role":"assistant","model":"anthropic.claude-opus-5","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":3,"output_tokens":2}}`
	const anthropicNoUsage = `{"type":"message","role":"assistant","model":"anthropic.claude-opus-5","content":[{"type":"text","text":"hi"}]}`
	const chatOK = `{"id":"c1","object":"chat.completion","model":"openai.gpt-5.4","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`
	const chatNoUsage = `{"id":"c1","object":"chat.completion","model":"openai.gpt-5.4","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}]}`
	for _, tc := range []struct {
		name, upstream, body string
		wantErr              bool
	}{
		{"anthropic route with usage", "anthropic.claude-opus-5", anthropicOK, false},
		{"anthropic route no usage", "anthropic.claude-opus-5", anthropicNoUsage, true},
		{"chat route with usage", "openai.gpt-5.4", chatOK, false},
		{"chat route no usage", "openai.gpt-5.4", chatNoUsage, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			mc := staticMantle(t, srv)
			raw := `{"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
			resp, err := mc.Complete(context.Background(), &providers.ProxyRequest{
				Model: "mantle.m", Upstream: tc.upstream,
				RawBody: []byte(raw), Parsed: parseChat(t, raw),
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
				t.Fatalf("2xx with no usage returned success: %+v", resp)
			}
			var ue *providers.UpstreamError
			if !errors.As(err, &ue) || ue.StatusCode != 502 {
				t.Fatalf("want a 502 UpstreamError, got %v", err)
			}
			if resp != nil {
				t.Fatalf("no response may be returned alongside the refusal: %+v", resp)
			}
		})
	}
}
