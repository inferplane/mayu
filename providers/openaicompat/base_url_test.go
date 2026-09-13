package openaicompat

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/inferplane/inferplane/providers"
)

func TestBaseURLAcceptsRootOrVersionPrefix(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/chat/completions":
			fmt.Fprint(w, `{"id":"x","model":"up","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
		case "/v1/models":
			fmt.Fprint(w, `{"data":[]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer up.Close()
	for _, suffix := range []string{"", "/", "/v1", "/v1/"} {
		t.Run("base"+suffix, func(t *testing.T) {
			p, err := factory(providers.Config{BaseURL: up.URL + suffix})
			if err != nil {
				t.Fatal(err)
			}
			resp, err := p.Complete(context.Background(), &providers.ProxyRequest{
				Model: "public", Upstream: "up", IngressProtocol: "openai",
				RawBody: []byte(`{"model":"public","messages":[{"role":"user","content":"hi"}]}`),
			})
			if err != nil || resp.StatusCode != 200 {
				t.Fatalf("version prefix duplicated: response=%+v error=%v", resp, err)
			}
			if result := p.(providers.HealthChecker).HealthCheck(context.Background()); !result.OK {
				t.Fatalf("health check prefix mismatch: %+v", result)
			}
		})
	}
}
