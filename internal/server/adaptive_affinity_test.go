package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/inferplane/inferplane/api/v1alpha1"
	"github.com/inferplane/inferplane/internal/config"
	"github.com/inferplane/inferplane/internal/policy"
	"github.com/inferplane/inferplane/internal/server/routingtest"
	"github.com/inferplane/inferplane/pkg/schema"
)

func TestIngressAffinityPinsSuccessfulFallbackAcrossContextChanges(t *testing.T) {
	for _, ingress := range policyIngresses {
		t.Run(ingress.name(), func(t *testing.T) {
			f := routingtest.New(t, func(cfg *config.Config) {
				cheap := cfg.Models["economy"]
				cheap.Targets = append(cheap.Targets, config.Target{Provider: "retry", Model: "up"})
				cfg.Models["economy"] = cheap
			})
			f.Private.Fail = true
			in, out := int64(2), int64(1)
			f.Retry.Usage = &schema.Usage{InputTokens: &in, OutputTokens: &out}
			f.Policies = []*policy.Policy{routingtest.Context(v1alpha1.Enforce)}
			c := f.Policies[0].Rules[0].Routing.Context
			c.ComplexKeywords = []string{"architecture"}
			c.Stability = &policy.ContextStability{MinHold: 5 * time.Minute, MinRequests: 3, SessionTTL: 30 * time.Minute}
			now := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
			f.Router.SetAffinityClock(func() time.Time { return now })
			h := policyMux(f, nil, nil, nil, nil)
			send := func(content string) *httptest.ResponseRecorder {
				body := `{"model":"premium","max_tokens":64,"messages":[{"role":"user","content":"` + content + `"}]}`
				if ingress.stream {
					body = strings.TrimSuffix(body, "}") + `,"stream":true}`
				}
				req := httptest.NewRequest(http.MethodPost, ingress.path, strings.NewReader(body))
				req.Header.Set("x-api-key", "dev-key")
				req.Header.Set("X-Inferplane-Session-ID", "opaque-session-hint")
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				return rec
			}
			first := send("hello")
			if first.Code != 200 || len(f.Retry.Calls) != 1 {
				t.Fatalf("initial fallback failed: %d %s", first.Code, first.Body.String())
			}
			now = now.Add(time.Second)
			second := send("review the architecture")
			if second.Code != 200 || len(f.Retry.Calls) != 2 || len(f.Public.Calls) != 0 || len(f.Private.Calls) != 1 {
				t.Fatalf("did not retain successful cache target: status=%d private=%d retry=%d public=%d",
					second.Code, len(f.Private.Calls), len(f.Retry.Calls), len(f.Public.Calls))
			}
		})
	}
}

func TestIngressAffinityDoesNotPinMissingUsage(t *testing.T) {
	for _, ingress := range policyIngresses {
		t.Run(ingress.name(), func(t *testing.T) {
			f := routingtest.New(t, nil)
			f.Policies = []*policy.Policy{routingtest.Context(v1alpha1.Enforce)}
			c := f.Policies[0].Rules[0].Routing.Context
			c.ComplexKeywords = []string{"architecture"}
			c.Stability = &policy.ContextStability{MinHold: time.Minute, MinRequests: 3, SessionTTL: time.Hour}
			h := policyMux(f, nil, nil, nil, nil)
			for _, text := range []string{"hello", "review architecture"} {
				body := `{"model":"premium","messages":[{"role":"user","content":"` + text + `"}]}`
				if ingress.stream {
					body = strings.TrimSuffix(body, "}") + `,"stream":true}`
				}
				req := httptest.NewRequest("POST", ingress.path, strings.NewReader(body))
				req.Header.Set("x-api-key", "dev-key")
				req.Header.Set("X-Inferplane-Session-ID", "opaque-hint")
				h.ServeHTTP(httptest.NewRecorder(), req)
			}
			if len(f.Private.Calls) != 1 || len(f.Public.Calls) != 1 {
				t.Fatalf("invalid usage response established affinity: private=%d public=%d", len(f.Private.Calls), len(f.Public.Calls))
			}
		})
	}
}
