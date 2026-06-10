package openaicompat

import (
	"context"
	"encoding/json"
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
