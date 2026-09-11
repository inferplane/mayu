package openairesponses

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/inferplane/inferplane/providers"
)

func TestCompletePreservesUsageOnUnsuccessfulNativeResponse(t *testing.T) {
	for _, status := range []string{"failed", "incomplete", "in_progress", "cancelled"} {
		t.Run(status, func(t *testing.T) {
			raw := `{"id":"remote","model":"upstream","status":"` + status + `","output":[],
				"error":{"message":"private-upstream-detail"},
				"usage":{"input_tokens":100,"output_tokens":7,"input_tokens_details":{"cached_tokens":60,"cache_write_tokens":10}}}`
			if status == "incomplete" {
				raw = `{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},
					"output":[{"type":"function_call","call_id":"c","name":"read","arguments":"{\"path\":"}],
					"usage":{"input_tokens":100,"output_tokens":7,"input_tokens_details":{"cached_tokens":60,"cache_write_tokens":10}}}`
			}
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Authorization", "Bearer private-upstream-detail")
				_, _ = io.WriteString(w, raw)
			}))
			defer up.Close()
			p, err := factory(providers.Config{BaseURL: up.URL})
			if err != nil {
				t.Fatal(err)
			}
			resp, err := p.Complete(context.Background(), &providers.ProxyRequest{
				IngressProtocol: "responses", Model: "public", Upstream: "upstream",
				RawBody: []byte(`{"model":"public","input":"hello"}`),
			})
			if err != nil || resp == nil || resp.StatusCode != 502 || resp.Parsed == nil || resp.Parsed.Usage == nil {
				t.Fatalf("failed native response must remain settleable: resp=%+v err=%v", resp, err)
			}
			u := resp.Parsed.Usage
			if u.InputTokens == nil || *u.InputTokens != 30 || u.OutputTokens == nil || *u.OutputTokens != 7 ||
				u.CacheReadInputTokens == nil || *u.CacheReadInputTokens != 60 ||
				u.CacheCreationInputTokens == nil || *u.CacheCreationInputTokens != 10 {
				t.Fatalf("cache-aware usage was lost or double counted: %+v", u)
			}
			parsed, err := json.Marshal(resp.Parsed)
			if err != nil {
				t.Fatal(err)
			}
			if resp.Parsed.Model != "public" || len(resp.Parsed.Content) != 0 ||
				strings.Contains(string(parsed)+string(resp.RawBody), "private-upstream-detail") ||
				resp.Headers.Get("Authorization") != "" {
				t.Fatalf("error observation exposed upstream state: parsed=%s body=%s", parsed, resp.RawBody)
			}
		})
	}
}

func TestCompleteNeverInventsUsageForMalformedNativeResponse(t *testing.T) {
	for _, raw := range []string{
		`{"status":"failed","usage":{"input_tokens":1,"output_tokens":2}`,
		`{"status":"failed","usage":{"input_tokens":1,"output_tokens":2}} trailing`,
		`{"status":"failed","usage":{"input_tokens":1,"output_tokens":2},"usage":{"input_tokens":0,"output_tokens":0}}`,
		`{"status":"failed","usage":{"input_tokens":1,"input_tokens":0,"output_tokens":2}}`,
		`{"status":"failed","usage":{"Input_Tokens":1,"output_tokens":2}}`,
		`{"status":"failed","usage":{"input_tokens":1}}`,
		`{"status":"failed","usage":{"input_tokens":-1,"output_tokens":2}}`,
		`{"status":"failed","usage":{"input_tokens":1,"output_tokens":2,"input_tokens_details":{"cached_tokens":3}}}`,
		`{"status":"failed","usage":{"input_tokens":1,"output_tokens":2,"input_tokens_details":{"Cached_Tokens":1}}}`,
		`{"status":"failed","usage":null}`,
	} {
		t.Run(raw, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, raw)
			}))
			defer up.Close()
			p, err := factory(providers.Config{BaseURL: up.URL})
			if err != nil {
				t.Fatal(err)
			}
			resp, err := p.Complete(context.Background(), &providers.ProxyRequest{
				IngressProtocol: "responses", Model: "public", Upstream: "upstream", RawBody: []byte(`{"model":"public"}`),
			})
			if err == nil && (resp == nil || resp.StatusCode/100 == 2) {
				t.Fatalf("malformed response was successful: %+v", resp)
			}
			if resp != nil && resp.Parsed != nil && resp.Parsed.Usage != nil {
				t.Fatalf("malformed response supplied fabricated usage: %+v", resp.Parsed.Usage)
			}
		})
	}
}

func TestCompleteKeepsUsageWhenCompletedOutputCannotBeDecoded(t *testing.T) {
	raw := `{"status":"completed","output":[{"type":"function_call","name":"read","call_id":"c","arguments":"not-json"}],
		"usage":{"input_tokens":0,"output_tokens":0}}`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, raw)
	}))
	defer up.Close()
	p, err := factory(providers.Config{BaseURL: up.URL})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := p.Complete(context.Background(), &providers.ProxyRequest{
		IngressProtocol: "responses", Model: "public", Upstream: "upstream", RawBody: []byte(`{"model":"public"}`),
	})
	if err != nil || resp == nil || resp.StatusCode != 502 || resp.Parsed == nil || resp.Parsed.Usage == nil {
		t.Fatalf("invalid output discarded independently valid zero usage: %+v %v", resp, err)
	}
	u := resp.Parsed.Usage
	if u.InputTokens == nil || *u.InputTokens != 0 || u.OutputTokens == nil || *u.OutputTokens != 0 {
		t.Fatalf("zero usage lost: %+v", u)
	}
}
