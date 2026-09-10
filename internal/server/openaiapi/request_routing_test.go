package openaiapi

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/inferplane/inferplane/api/v1alpha1"
	"github.com/inferplane/inferplane/internal/config"
	"github.com/inferplane/inferplane/internal/policy"
	"github.com/inferplane/inferplane/internal/router"
	"github.com/inferplane/inferplane/internal/server/routingtest"
	"github.com/inferplane/inferplane/providers"
)

type legacyAnthropicSpy struct{ routingtest.Spy }

func (*legacyAnthropicSpy) Name() string { return "anthropic" }

func TestChatNoNewPolicyPassthrough(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "stream"}[stream], func(t *testing.T) {
			spy := &legacyAnthropicSpy{}
			r := recRouter(spy)
			raw := routingtest.Body("openai", "system", stream)
			// recRouter exposes m; this test's provider intentionally retains the
			// original anthropic Name and existing canonical conversion behavior.
			raw = strings.Replace(raw, `"premium"`, `"m"`, 1)
			rec := httptest.NewRecorder()
			NewChatHandler(r).ServeHTTP(rec, routingtest.Request("/v1/chat/completions", raw))
			if rec.Code != 200 || len(spy.Calls) != 1 || spy.Calls[0].Body != raw {
				t.Fatalf("no-policy traffic regressed: %d %s calls=%d", rec.Code, rec.Body, len(spy.Calls))
			}
			if rec.Header().Get("x-inferplane-routing-reason") != "" {
				t.Fatal("legacy traffic acquired new policy evidence")
			}
		})
	}
}

func TestChatContextOnlyOriginalTransportAndRetries(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, fail := range []bool{false, true} {
			for _, mode := range []v1alpha1.ContextMode{v1alpha1.Shadow, v1alpha1.Enforce} {
				t.Run(string(mode)+map[bool]string{false: "/complete", true: "/stream"}[stream]+map[bool]string{false: "/original", true: "/retry"}[fail], func(t *testing.T) {
					original := &legacyAnthropicSpy{}
					original.Fail = fail
					unsafe := &legacyAnthropicSpy{}
					safe := &routingtest.Spy{}
					holder := holderFor(map[string]providers.Provider{"original": original, "unsafe": unsafe, "safe": safe},
						map[string]config.ModelConfig{"m": {Targets: []config.Target{{Provider: "original", Model: "up"}, {Provider: "unsafe", Model: "up"}, {Provider: "safe", Model: "up"}}}})
					r := router.New(holder)
					p := routingtest.Context(mode)
					p.Rules[0].Routing.Context.FromModels = []string{"m"} // economy is unavailable
					r.SetRoutingPolicyLookup(func(string, string) ([]*policy.Policy, error) { return []*policy.Policy{p}, nil })
					raw := strings.Replace(routingtest.Body("openai", "clean", stream), `"premium"`, `"m"`, 1)
					rec := httptest.NewRecorder()
					NewChatHandler(r).ServeHTTP(rec, routingtest.Request("/v1/chat/completions", raw))
					retries := 0
					if fail {
						retries = 1
					}
					if rec.Code != 200 || len(original.Calls) != 1 || len(unsafe.Calls) != 0 || len(safe.Calls) != retries || original.Calls[0].Body != raw {
						t.Fatalf("optional context broke original or allowed unsafe retry: status=%d calls=%d/%d/%d", rec.Code, len(original.Calls), len(unsafe.Calls), len(safe.Calls))
					}
				})
			}
		}
	}
}

func TestChatPolicyRouting(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, surface := range []string{"system", "tool", "numeric"} {
			t.Run(surface+map[bool]string{false: "/complete", true: "/stream"}[stream], func(t *testing.T) {
				f := routingtest.New(t, nil)
				f.Policies = []*policy.Policy{routingtest.Privacy(v1alpha1.InternalOnly)}
				f.Private.Fail = true
				raw := routingtest.Body("openai", surface, stream)
				rec := httptest.NewRecorder()
				NewChatHandler(f.Router).ServeHTTP(rec, routingtest.Request("/v1/chat/completions", raw))
				if len(f.Public.Calls) != 0 {
					t.Fatal("protected request escaped to external provider")
				}
				if rec.Code != 200 || len(f.Private.Calls) != 1 || len(f.Retry.Calls) != 1 {
					t.Fatalf("internal retry failed: %d %s calls=%d/%d", rec.Code, rec.Body, len(f.Private.Calls), len(f.Retry.Calls))
				}
				if f.Retry.Calls[0].Body != raw || f.Lookups != 1 {
					t.Fatal("bytes changed or policy snapshot reloaded")
				}
			})
		}
	}
}
