package anthropic

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/inferplane/inferplane/providers"
)

func TestCompleteForwardsRawBodyAndParsesUsage(t *testing.T) {
	var gotBody []byte
	var gotKey, gotVersion string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":12,"output_tokens":3}}`)
	}))
	defer upstream.Close()

	p, _ := factory(providers.Config{Type: "anthropic", BaseURL: upstream.URL, APIKey: "sk-up"})
	raw := []byte(`{"model":"claude-sonnet-4-6","max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`)
	hdr := http.Header{"Anthropic-Version": {"2023-06-01"}}
	resp, err := p.Complete(context.Background(), &providers.ProxyRequest{
		Model: "claude-sonnet-4-6", Upstream: "claude-sonnet-4-6", RawBody: raw, Headers: hdr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(gotBody) != string(raw) {
		t.Fatalf("upstream body mutated:\n got: %s\nwant: %s", gotBody, raw)
	}
	if gotKey != "sk-up" {
		t.Fatalf("upstream key = %q, want gateway key", gotKey)
	}
	if gotVersion != "2023-06-01" {
		t.Fatalf("anthropic-version not forwarded: %q", gotVersion)
	}
	if resp.Parsed == nil || resp.Parsed.Usage == nil || *resp.Parsed.Usage.InputTokens != 12 {
		t.Fatalf("usage not parsed: %+v", resp.Parsed)
	}
}

func TestStreamTeesRawAndObservesUsage(t *testing.T) {
	sse := "event: message_start\ndata: {\"type\":\"message_start\"}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"input_tokens\":4,\"output_tokens\":9}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, sse)
	}))
	defer upstream.Close()

	p, _ := factory(providers.Config{Type: "anthropic", BaseURL: upstream.URL, APIKey: "sk-up"})
	seq, err := p.Stream(context.Background(), &providers.ProxyRequest{
		Model: "claude-sonnet-4-6", RawBody: []byte(`{"stream":true}`), Headers: http.Header{}, Stream: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var raw strings.Builder
	var lastOut int64
	for ev, err := range seq {
		if err != nil {
			t.Fatal(err)
		}
		raw.Write(ev.Raw)
		if ev.Chunk != nil && ev.Chunk.Usage != nil && ev.Chunk.Usage.OutputTokens != nil {
			lastOut = *ev.Chunk.Usage.OutputTokens
		}
	}
	if raw.String() != sse {
		t.Fatalf("tee not byte-exact")
	}
	if lastOut != 9 {
		t.Fatalf("usage observation wrong: %d", lastOut)
	}
}

func TestCountTokensProxiesUpstream(t *testing.T) {
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"input_tokens":1234}`)
	}))
	defer upstream.Close()

	p, _ := factory(providers.Config{Type: "anthropic", BaseURL: upstream.URL, APIKey: "sk-up"})
	tc, ok := p.(providers.TokenCounter)
	if !ok {
		t.Fatal("anthropic provider should implement TokenCounter")
	}
	n, err := tc.CountTokens(context.Background(), &providers.ProxyRequest{
		RawBody: []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}`),
		Headers: http.Header{"Anthropic-Version": {"2023-06-01"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1234 {
		t.Fatalf("count = %d, want 1234", n)
	}
	if gotPath != "/v1/messages/count_tokens" {
		t.Fatalf("path = %q", gotPath)
	}
}
