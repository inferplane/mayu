package openairesponses

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/inferplane/inferplane/providers"
)

func TestNativePassThroughAndKeyIsolation(t *testing.T) {
	raw := ` { "input" : [{"type":"reasoning","encrypted_content":"opaque"}], "model" : "public", "store":false, "unknown": [1,2] } `
	want := strings.Replace(raw, `"public"`, `"upstream"`, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/v1/responses" || r.Method != "POST" || string(body) != want {
			t.Errorf("request: %s %s %q", r.Method, r.URL.Path, body)
		}
		if r.Header.Get("Authorization") != "Bearer upstream-test-key" || r.Header.Get("X-Client-Secret") != "" {
			t.Error("client credential forwarded")
		}
		io.WriteString(w, `{"id":"r","model":"upstream","status":"completed","output":[],"usage":{"input_tokens":10,"input_tokens_details":{"cached_tokens":8},"output_tokens":2}}`)
	}))
	defer ts.Close()
	p, err := providers.New(providers.Config{Type: "openai_responses", BaseURL: ts.URL + "/v1/", APIKey: "upstream-test-key"})
	if err != nil {
		t.Fatal(err)
	}
	support := p.(interface{ SupportsIngress(string) bool })
	if !support.SupportsIngress("responses") || support.SupportsIngress("anthropic") {
		t.Fatal("incorrect supported ingress")
	}
	resp, err := p.Complete(context.Background(), &providers.ProxyRequest{IngressProtocol: "responses", Model: "public", Upstream: "upstream", RawBody: []byte(raw), Headers: http.Header{"Authorization": {"Bearer client-test-key"}, "X-Client-Secret": {"secret"}}})
	if err != nil || resp.Parsed == nil || *resp.Parsed.Usage.InputTokens != 2 || *resp.Parsed.Usage.CacheReadInputTokens != 8 {
		t.Fatalf("response: %+v %v", resp, err)
	}
}

func TestProviderRejectsMissingUsageAndForeignIngress(t *testing.T) {
	calls := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		io.WriteString(w, `{"status":"completed","output":[]}`)
	}))
	defer ts.Close()
	p, err := providers.New(providers.Config{Type: "openai_responses", BaseURL: ts.URL})
	if err != nil {
		t.Fatal(err)
	}
	req := &providers.ProxyRequest{IngressProtocol: "responses", Upstream: "u", RawBody: []byte(`{"model":"m","input":"hi"}`)}
	if resp, err := p.Complete(context.Background(), req); err == nil || resp != nil {
		t.Fatal("unbillable 200 accepted")
	}
	req.IngressProtocol = "anthropic"
	if _, err := p.Complete(context.Background(), req); err == nil || calls != 1 {
		t.Fatal("foreign ingress reached native-only provider")
	}
}

func TestProviderStreamingAndCancellation(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":2}}}\n\n")
	}))
	defer ts.Close()
	p, err := providers.New(providers.Config{Type: "openai_responses", BaseURL: ts.URL})
	if err != nil {
		t.Fatal(err)
	}
	req := &providers.ProxyRequest{IngressProtocol: "responses", Upstream: "u", Model: "m", Stream: true, RawBody: []byte(`{"model":"m","stream":true}`)}
	stream, err := p.Stream(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	seen := false
	for ev, err := range stream {
		if err != nil {
			t.Fatal(err)
		}
		seen = seen || ev.Chunk != nil && ev.Chunk.Usage != nil
	}
	if !seen {
		t.Fatal("no usage observed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Stream(ctx, req); err == nil {
		t.Fatal("cancelled request succeeded")
	}
}

func TestProviderHTTPFailureIsSafeAndKeepsStatus(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Authorization", "Bearer upstream-secret")
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":{"message":"upstream-secret"}}`)
	}))
	defer ts.Close()
	p, err := providers.New(providers.Config{Type: "openai_responses", BaseURL: ts.URL})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := p.Complete(context.Background(), &providers.ProxyRequest{IngressProtocol: "responses", Model: "m", Upstream: "u", RawBody: []byte(`{"model":"m"}`)})
	if err != nil || resp == nil || resp.StatusCode != 429 || resp.Headers.Get("Retry-After") != "3" || resp.Headers.Get("Authorization") != "" || strings.Contains(string(resp.RawBody), "upstream-secret") {
		t.Fatalf("unsafe/lost upstream failure: %+v %v", resp, err)
	}
}

func TestProviderRedirectCannotForwardCredentials(t *testing.T) {
	reached := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true }))
	defer target.Close()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer ts.Close()
	p, err := providers.New(providers.Config{Type: "openai_responses", BaseURL: ts.URL, APIKey: "test-key"})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = p.Complete(context.Background(), &providers.ProxyRequest{IngressProtocol: "responses", Model: "m", Upstream: "u", RawBody: []byte(`{"model":"m"}`)})
	if reached {
		t.Fatal("redirect bypassed selected destination")
	}
}

func TestNativeHistoryAndResponsePreserveExplicitPhase(t *testing.T) {
	raw := `{"model":"public","input":[{"type":"message","role":"assistant","phase":"commentary","content":[{"type":"output_text","text":"Checking"}]},{"type":"function_call","call_id":"call_a","name":"read","arguments":"{}"},{"type":"function_call_output","call_id":"call_a","output":"done"}]}`
	response := `{"id":"r","model":"upstream","status":"completed","output":[{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"Finished"}]}],"usage":{"input_tokens":1,"output_tokens":2}}`
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		if string(body) != strings.Replace(raw, `"public"`, `"upstream"`, 1) {
			t.Errorf("native history rewritten: %s", body)
		}
		io.WriteString(w, response)
	}))
	defer ts.Close()
	p, err := providers.New(providers.Config{Type: "openai_responses", BaseURL: ts.URL})
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.Complete(context.Background(), &providers.ProxyRequest{
		IngressProtocol: "responses", Model: "public", Upstream: "upstream", RawBody: []byte(raw),
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(got.RawBody) != response || got.Parsed == nil || len(got.Parsed.Content) != 1 || string(got.Parsed.Content[0].Extra["phase"]) != `"final_answer"` {
		t.Fatalf("native response phase/body changed: %+v", got)
	}
}
