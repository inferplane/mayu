package anthropicapi

import (
	"net/http/httptest"
	"testing"

	"github.com/inferplane/inferplane/api/v1alpha1"
	"github.com/inferplane/inferplane/internal/policy"
	"github.com/inferplane/inferplane/internal/server/routingtest"
)

// Missing the decision seam leaks tool/system content to the public primary.
func TestMessagesPolicyRouting(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, surface := range []string{"system", "tool", "numeric"} {
			t.Run(surface+map[bool]string{false: "/complete", true: "/stream"}[stream], func(t *testing.T) {
				f := routingtest.New(t, nil)
				f.Policies = []*policy.Policy{routingtest.Privacy(v1alpha1.InternalOnly)}
				f.Private.Fail = true
				raw := routingtest.Body("anthropic", surface, stream)
				rec := httptest.NewRecorder()
				NewMessagesHandler(f.Router).ServeHTTP(rec, routingtest.Request("/v1/messages", raw))
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

func TestAnthropicCountPolicyRouting(t *testing.T) {
	for _, action := range []v1alpha1.SensitiveDataAction{v1alpha1.InternalOnly, v1alpha1.Block} {
		t.Run(string(action), func(t *testing.T) {
			f := routingtest.New(t, nil)
			f.Policies = []*policy.Policy{routingtest.Privacy(action)}
			raw := routingtest.Body("anthropic", "numeric", false)
			rec := httptest.NewRecorder()
			NewCountTokensHandler(f.Router).ServeHTTP(rec, routingtest.Request("/v1/messages/count_tokens", raw))
			if rec.Code != 200 {
				t.Fatal("count_tokens must stay 200")
			}
			if len(f.Public.Calls) != 0 {
				t.Fatal("count leaked to external provider")
			}
			if action == v1alpha1.InternalOnly && (len(f.Private.Calls) != 1 || f.Private.Calls[0].Body != raw) {
				t.Fatal("count did not use approved route with original bytes")
			}
			if action == v1alpha1.Block && len(f.Private.Calls) != 0 {
				t.Fatal("blocked count called provider")
			}
		})
	}
}
