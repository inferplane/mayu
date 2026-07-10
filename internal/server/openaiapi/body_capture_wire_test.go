package openaiapi

import (
	"bytes"
	"context"
	"iter"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inferplane/inferplane/internal/audit"
	"github.com/inferplane/inferplane/internal/bodystore"
	"github.com/inferplane/inferplane/internal/config"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/principal"
	"github.com/inferplane/inferplane/internal/router"
	"github.com/inferplane/inferplane/pkg/schema"
	"github.com/inferplane/inferplane/providers"
)

// recProvider records the last ProxyRequest it received, so a test can assert
// the exact bytes forwarded upstream (mirrors anthropicapi's mask_wire_test.go).
type recProvider struct {
	last *providers.ProxyRequest
}

func (p *recProvider) Name() string               { return "openai_compatible" }
func (p *recProvider) Models() []schema.ModelInfo { return nil }
func (p *recProvider) Complete(_ context.Context, req *providers.ProxyRequest) (*providers.ProxyResponse, error) {
	p.last = req
	return &providers.ProxyResponse{StatusCode: 200, RawBody: []byte(`{"id":"x","object":"chat.completion","choices":[],"usage":{}}`)}, nil
}
func (p *recProvider) Stream(context.Context, *providers.ProxyRequest) (iter.Seq2[*providers.StreamEvent, error], error) {
	return nil, nil
}

func recRouter(p providers.Provider) *router.Router {
	provs := map[string]providers.Provider{"p": p}
	models := map[string]config.ModelConfig{"m": {Targets: []config.Target{{Provider: "p", Model: "up"}}}}
	return router.New(holderFor(provs, models))
}

// anthropicWireProvider fakes an anthropic-wire upstream (e.g. Bedrock/Anthropic
// native): RawBody is anthropic-shaped JSON, distinct from what the OpenAI
// ingress writes to the client (which is openai.ResponseFromCanonical(Parsed)).
type anthropicWireProvider struct{}

func (anthropicWireProvider) Name() string               { return "anthropic" }
func (anthropicWireProvider) Models() []schema.ModelInfo { return nil }
func (anthropicWireProvider) Complete(context.Context, *providers.ProxyRequest) (*providers.ProxyResponse, error) {
	return &providers.ProxyResponse{
		StatusCode: 200,
		RawBody:    []byte(`{"id":"msg_1","type":"message","role":"assistant","model":"up","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn"}`),
		Parsed: &schema.ChatResponse{
			ID: "msg_1", Type: "message", Role: "assistant", Model: "up",
			Content: []schema.ContentBlock{{Type: "text", Text: strPtr("hi")}},
		},
	}, nil
}
func (anthropicWireProvider) Stream(context.Context, *providers.ProxyRequest) (iter.Seq2[*providers.StreamEvent, error], error) {
	return nil, nil
}

func strPtr(s string) *string { return &s }

func testKeyBody(seed byte) [32]byte {
	var k [32]byte
	for i := range k {
		k[i] = seed
	}
	return k
}

func testRecorder(t *testing.T) *bodystore.Recorder {
	t.Helper()
	store, err := bodystore.OpenSQLite(filepath.Join(t.TempDir(), "bodies.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	rec := bodystore.NewRecorder(store, testKeyBody(1), 0, 1<<20)
	t.Cleanup(rec.Close)
	return rec
}

func extractBodyRef(t *testing.T, raw []byte) string {
	t.Helper()
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if !bytes.Contains(line, []byte(`"event":"request_completed"`)) {
			continue
		}
		i := bytes.Index(line, []byte(`"body_ref":"`))
		if i < 0 {
			t.Fatalf("request_completed record missing body_ref: %s", line)
		}
		rest := line[i+len(`"body_ref":"`):]
		return string(rest[:bytes.IndexByte(rest, '"')])
	}
	t.Fatalf("no request_completed record found: %s", raw)
	return ""
}

// TestChatBodyCaptureRoundTrip proves the happy path end to end.
func TestChatBodyCaptureRoundTrip(t *testing.T) {
	rec := &recProvider{}
	bodies := testRecorder(t)

	var buf bytes.Buffer
	w, err := audit.NewWriter("i", filepath.Join(t.TempDir(), "a.wal"), []audit.Sink{audit.NewWriterSink("b", &buf, true)})
	if err != nil {
		t.Fatal(err)
	}
	h := NewChatHandlerFull(recRouter(rec), w, nil)
	h.SetBodyRecorder(bodies)

	reqBody := `{"model":"m","messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(reqBody))
	ctx := principal.With(req.Context(), keystore.Principal{KeyID: "ik", Team: "acme", AllowedModels: []string{"*"}})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req.WithContext(ctx))
	w.Close()
	bodies.Close()

	if rr.Code != 200 {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
	ref := extractBodyRef(t, buf.Bytes())

	got, err := bodies.Fetch(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if got.Team != "acme" {
		t.Fatalf("captured team = %q, want acme", got.Team)
	}
	if string(got.Request) != reqBody {
		t.Fatalf("captured request = %q, want %q", got.Request, reqBody)
	}
}

// TestChatBodyCapture_NilRecorderOmitsBodyRef proves the zero-overhead
// default: with no recorder set, a request_completed record never carries
// body_ref.
func TestChatBodyCapture_NilRecorderOmitsBodyRef(t *testing.T) {
	rec := &recProvider{}
	var buf bytes.Buffer
	w, err := audit.NewWriter("i", filepath.Join(t.TempDir(), "a.wal"), []audit.Sink{audit.NewWriterSink("b", &buf, true)})
	if err != nil {
		t.Fatal(err)
	}
	h := NewChatHandlerFull(recRouter(rec), w, nil)
	// No SetBodyRecorder call.

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[]}`))
	ctx := principal.With(req.Context(), keystore.Principal{KeyID: "ik", Team: "acme", AllowedModels: []string{"*"}})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req.WithContext(ctx))
	w.Close()

	if strings.Contains(buf.String(), `"body_ref"`) {
		t.Fatalf("body_ref must be absent with no recorder configured: %s", buf.String())
	}
}

// TestChatBodyCapture_CapturesClientFacingBytesNotUpstreamWire proves the
// captured response body is the OpenAI-shaped JSON actually written to the
// client when routing through a non-openai-wire (anthropic/Bedrock) provider
// — not the upstream's native wire bytes, which the client never saw.
func TestChatBodyCapture_CapturesClientFacingBytesNotUpstreamWire(t *testing.T) {
	prov := anthropicWireProvider{}
	bodies := testRecorder(t)

	var buf bytes.Buffer
	w, err := audit.NewWriter("i", filepath.Join(t.TempDir(), "a.wal"), []audit.Sink{audit.NewWriterSink("b", &buf, true)})
	if err != nil {
		t.Fatal(err)
	}
	h := NewChatHandlerFull(recRouter(prov), w, nil)
	h.SetBodyRecorder(bodies)

	reqBody := `{"model":"m","messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(reqBody))
	ctx := principal.With(req.Context(), keystore.Principal{KeyID: "ik", Team: "acme", AllowedModels: []string{"*"}})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req.WithContext(ctx))
	w.Close()
	bodies.Close()

	if rr.Code != 200 {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
	clientBytes := rr.Body.Bytes()
	ref := extractBodyRef(t, buf.Bytes())
	got, err := bodies.Fetch(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}

	// Positive: the captured response is the OpenAI-shaped bytes the client
	// actually received.
	if !bytes.Equal(bytes.TrimSpace(got.Response), bytes.TrimSpace(clientBytes)) {
		t.Fatalf("captured response = %s\nwant (client-written) %s", got.Response, clientBytes)
	}
	// Negative: it must NOT be the upstream's anthropic-wire bytes — a test
	// that only checked "captured == written" would pass trivially if both
	// sides were still wrongly using RawBody.
	anthropicWireBody := `{"id":"msg_1","type":"message","role":"assistant","model":"up","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn"}`
	if bytes.Equal(bytes.TrimSpace(got.Response), []byte(anthropicWireBody)) {
		t.Fatalf("captured response is the upstream anthropic-wire body, not what the OpenAI client received: %s", got.Response)
	}
	if !bytes.Contains(clientBytes, []byte(`"object":"chat.completion"`)) {
		t.Fatalf("sanity: client response is not OpenAI-shaped: %s", clientBytes)
	}
}

// TestChatBodyCapture_RawBodyPassthroughByteForByte is the required §13
// regression test: body capture must never mutate the upstream-forwarded
// request bytes or the client-received response bytes (cache invariant §4.4).
func TestChatBodyCapture_RawBodyPassthroughByteForByte(t *testing.T) {
	rec := &recProvider{}
	h := NewChatHandler(recRouter(rec))
	h.SetBodyRecorder(testRecorder(t))

	reqBody := `{"model":"m","messages":[{"role":"user","content":"byte for byte"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(reqBody))
	ctx := principal.With(req.Context(), keystore.Principal{KeyID: "ik", Team: "acme", AllowedModels: []string{"*"}})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req.WithContext(ctx))
	if rr.Code != 200 {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
	if rec.last == nil {
		t.Fatal("provider not called")
	}
	if string(rec.last.RawBody) != reqBody {
		t.Fatalf("upstream-forwarded RawBody mutated by body capture:\n got %q\nwant %q", rec.last.RawBody, reqBody)
	}
}
