package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/inferplane/inferplane/api/v1alpha1"
	"github.com/inferplane/inferplane/internal/filter"
	"github.com/inferplane/inferplane/internal/policy"
	"github.com/inferplane/inferplane/internal/sensitivity"
	"github.com/inferplane/inferplane/internal/server/openaiapi"
	"github.com/inferplane/inferplane/internal/server/responsesapi"
	"github.com/inferplane/inferplane/internal/server/routingtest"
	"github.com/inferplane/inferplane/pkg/schema"
	"github.com/inferplane/inferplane/plugins/piimask"
)

func TestPolicyMaskCannotExemptAnEnabledLegacyFilter(t *testing.T) {
	for _, protocol := range []string{"openai", "responses"} {
		t.Run(protocol, func(t *testing.T) {
			f := routingtest.New(t, nil)
			f.Policies = []*policy.Policy{maskRoutingPolicy()}
			f.Router.SetRequestRedactor(sensitivity.NewRedactor())
			in, out := int64(2), int64(1)
			f.Public.Usage = &schema.Usage{InputTokens: &in, OutputTokens: &out}
			mask := &filter.Masking{Global: true, Filter: piimask.New(piimask.Options{})}
			body := `{"model":"premium","messages":[{"role":"user","content":"alice@example.test 2125551212"}]}`
			var handler http.Handler
			if protocol == "openai" {
				h := openaiapi.NewChatHandler(f.Router)
				h.SetMasking(mask)
				handler = h
			} else {
				h := responsesapi.NewHandler(f.Router, nil, nil, nil)
				h.SetMasking(mask)
				handler = h
				body = `{"model":"premium","input":"alice@example.test 2125551212"}`
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, routingtest.Request("/", body))
			if rec.Code != 200 || len(f.Public.Calls) != 1 {
				t.Fatalf("combined masking failed: %d %s", rec.Code, rec.Body.String())
			}
			if strings.Contains(f.Public.Calls[0].Body, "2125551212") || strings.Contains(f.Public.Calls[0].Body, "alice@example.test") {
				t.Fatalf("one enabled obligation was skipped: %s", f.Public.Calls[0].Body)
			}
		})
	}
}

func TestResponsesLegacyFilterHandlesCleanAndProtectedRequests(t *testing.T) {
	for _, text := range []string{"hello", "2125551212"} {
		f := routingtest.New(t, nil)
		in, out := int64(2), int64(1)
		f.Public.Usage = &schema.Usage{InputTokens: &in, OutputTokens: &out}
		h := responsesapi.NewHandler(f.Router, nil, nil, nil)
		h.SetMasking(&filter.Masking{Global: true, Filter: piimask.New(piimask.Options{})})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, routingtest.Request("/", `{"model":"premium","input":"`+text+`"}`))
		if rec.Code != 200 || len(f.Public.Calls) != 1 || strings.Contains(f.Public.Calls[0].Body, "2125551212") {
			t.Fatalf("legacy filter could not serve safely: %d %s", rec.Code, rec.Body.String())
		}
	}
}
func maskRoutingPolicy() *policy.Policy {
	return &policy.Policy{Name: "mask", Generation: 1, Rules: []policy.Rule{{
		Name: "protected", SensitiveData: &policy.SensitiveData{
			OnDetected: v1alpha1.Mask, OnUninspectable: v1alpha1.Block,
		},
	}}}
}

func TestPolicyMaskingSanitizesRawAndParsedOnEveryIngress(t *testing.T) {
	for _, ingress := range policyIngresses {
		t.Run(ingress.name(), func(t *testing.T) {
			f := routingtest.New(t, nil)
			f.Policies = []*policy.Policy{maskRoutingPolicy()}
			f.Router.SetRequestRedactor(sensitivity.NewRedactor())
			body := `{"model":"premium","max_tokens":64,"messages":[{"role":"user","content":"contact alice@example.test"}]}`
			if ingress.stream {
				body = strings.TrimSuffix(body, "}") + `,"stream":true}`
			}
			rec := policyDo(policyMux(f, nil, nil, nil, nil), ingress.path, body, true)
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			if len(f.Public.Calls) != 1 || len(f.Private.Calls) != 0 {
				t.Fatalf("unexpected routes: public=%d private=%d", len(f.Public.Calls), len(f.Private.Calls))
			}
			call := f.Public.Calls[0]
			parsed, err := json.Marshal(call.Parsed)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(call.Body, "alice@example.test") || strings.Contains(string(parsed), "alice@example.test") {
				t.Fatal("protected value reached provider raw or canonical representation")
			}
			if !strings.Contains(call.Body, "[REDACTED_PII]") {
				t.Fatal("forwarded body did not use completed redaction")
			}
		})
	}
}

func TestPolicyMaskingCoversCountAndRefusesNumericTypeChanges(t *testing.T) {
	for _, protocol := range []string{"anthropic", "bedrock"} {
		for _, numeric := range []bool{false, true} {
			t.Run(protocol+map[bool]string{false: "/text", true: "/numeric"}[numeric], func(t *testing.T) {
				f := routingtest.New(t, nil)
				f.Policies = []*policy.Policy{maskRoutingPolicy()}
				f.Router.SetRequestRedactor(sensitivity.NewRedactor())
				body := `{"model":"premium","messages":[{"role":"user","content":"alice@example.test"}]}`
				if numeric {
					body = `{"model":"premium","messages":[{"role":"user","content":[{"type":"tool_use","id":"call_a","name":"f","input":{"card":4111111111111111}}]}]}`
				}
				path := "/v1/messages/count_tokens"
				if protocol == "bedrock" {
					path = "/model/premium/count-tokens"
				}
				rec := policyDo(policyMux(f, nil, nil, nil, nil), path, countBody(protocol, body), true)
				if rec.Code != http.StatusOK {
					t.Fatalf("count returned %d", rec.Code)
				}
				if numeric {
					if len(f.Public.Calls)+len(f.Private.Calls)+len(f.Retry.Calls) != 0 {
						t.Fatal("unmaskable numeric data reached upstream")
					}
				} else if len(f.Public.Calls) != 1 || strings.Contains(f.Public.Calls[0].Body, "alice@example.test") {
					t.Fatal("count did not forward the sanitized body")
				}
			})
		}
	}
}
