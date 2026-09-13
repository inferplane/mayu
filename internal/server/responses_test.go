package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/inferplane/inferplane/internal/budget"
	"github.com/inferplane/inferplane/internal/config"
	"github.com/inferplane/inferplane/internal/governance"
	"github.com/inferplane/inferplane/internal/limiter"
	"github.com/inferplane/inferplane/internal/policy"
	"github.com/inferplane/inferplane/internal/sensitivity"
	"github.com/inferplane/inferplane/internal/server/routingtest"
	"github.com/inferplane/inferplane/pkg/schema"
)

func TestResponsesIdentityReadinessAndUserBudgetBeforeEgress(t *testing.T) {
	for _, scenario := range []string{"unauthenticated", "unready", "user-budget"} {
		t.Run(scenario, func(t *testing.T) {
			f := routingtest.New(t, nil)
			store := budget.NewMemory()
			gov := governance.NewGovernor(nil, limiter.NewMemory(), store, nil)
			gov.SetUserLookup(func(team, user string) (governance.UserPolicy, bool) {
				if team != "team" || user != "user" {
					t.Fatalf("lost governed identity: %q/%q", team, user)
				}
				return governance.UserPolicy{BudgetMicrosPerMonth: 1, BudgetExceeded: "block"}, true
			})
			window := budget.CalendarMonthIn(time.UTC)
			// The existing optimistic budget store rejects spent+estimate >
			// limit. Seed an exceeded counter to verify this ingress carries
			// the user's identity into that established admission contract.
			store.Debit(budget.Key(budget.ScopeUser, "team/user", window), 2, window)
			ready := scenario != "unready"
			h := policyMux(f, nil, gov, nil, nil, WithGovernanceGate(func() (bool, string) { return ready, "not_ready" }))
			rec := policyDo(h, "/v1/responses", `{"model":"premium","input":"hello"}`, scenario != "unauthenticated")
			want := map[string]int{"unauthenticated": 401, "unready": 503, "user-budget": 402}[scenario]
			if rec.Code != want || len(f.Public.Calls)+len(f.Private.Calls) != 0 {
				t.Fatalf("gate bypass: status=%d want=%d calls=%d", rec.Code, want, len(f.Public.Calls)+len(f.Private.Calls))
			}
		})
	}
}

func TestResponsesInterruptedStreamSettlesObservedUsageWithoutRetry(t *testing.T) {
	f := routingtest.New(t, func(cfg *config.Config) {
		cfg.Pricing.Overrides["public"]["up"] = config.RateConfig{InputPerMTok: 1, OutputPerMTok: 2}
		m := cfg.Models["premium"]
		m.Targets = append(m.Targets, config.Target{Provider: "private", Model: "up"})
		cfg.Models["premium"] = m
	})
	in := int64(10)
	f.Public.Usage = &schema.Usage{InputTokens: &in}
	f.Public.Partial = true
	store := budget.NewMemory()
	gov := governance.NewGovernor(map[string]governance.TeamPolicy{
		"team": {BudgetMicrosPerMonth: 1000, BudgetExceeded: "block"},
	}, limiter.NewMemory(), store, nil)
	rec := policyDo(policyMux(f, nil, gov, nil, nil), "/v1/responses", `{"model":"premium","input":"hello","stream":true}`, true)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "event: error") ||
		strings.Contains(rec.Body.String(), "event: response.completed") || len(f.Private.Calls) != 0 {
		t.Fatalf("interrupted response was completed or retried: status=%d body=%s private=%d", rec.Code, rec.Body.String(), len(f.Private.Calls))
	}
	window := budget.CalendarMonthIn(time.UTC)
	if spent := store.Spent(budget.Key(budget.ScopeTeam, "team", window), window); spent != 10 {
		t.Fatalf("observed partial usage not settled: got %d want 10 microUSD", spent)
	}
}

func TestResponsesIngressUsesSharedPolicyAndUsagePipeline(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "stream"}[stream], func(t *testing.T) {
			f := routingtest.New(t, nil)
			in, out := int64(8), int64(3)
			f.Public.Usage = &schema.Usage{InputTokens: &in, OutputTokens: &out}
			f.Policies = []*policy.Policy{maskRoutingPolicy()}
			f.Router.SetRequestRedactor(sensitivity.NewRedactor())
			body := `{"model":"premium","input":"contact alice@example.test","max_output_tokens":64}`
			if stream {
				body = strings.TrimSuffix(body, "}") + `,"stream":true}`
			}
			rec := policyDo(policyMux(f, nil, nil, nil, nil), "/v1/responses", body, true)
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			if len(f.Public.Calls) != 1 || strings.Contains(f.Public.Calls[0].Body, "alice@example.test") {
				t.Fatal("Responses ingress did not apply original-byte privacy policy")
			}
			if stream {
				if !strings.Contains(rec.Body.String(), "event: response.created") || !strings.Contains(rec.Body.String(), "event: response.completed") {
					t.Fatalf("invalid Responses lifecycle: %s", rec.Body.String())
				}
			} else {
				var got struct {
					Object string `json:"object"`
					Status string `json:"status"`
					Usage  struct {
						Input int64 `json:"input_tokens"`
					} `json:"usage"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.Object != "response" || got.Status != "completed" || got.Usage.Input != 8 {
					t.Fatalf("invalid response: %s, %v", rec.Body.String(), err)
				}
			}
		})
	}
}

func TestResponsesIngressRejectsUnsupportedConversionBeforeEgress(t *testing.T) {
	for _, body := range []string{
		`{"model":"premium","previous_response_id":"previous","input":"hello"}`,
		`{"model":"premium","input":"hello","tools":[{"type":"web_search"}]}`,
		`{"model":"premium","input":[{"type":"reasoning","encrypted_content":"opaque","summary":[]} ,{"role":"user","content":"hello"}]}`,
	} {
		f := routingtest.New(t, nil)
		rec := policyDo(policyMux(f, nil, nil, nil, nil), "/v1/responses", body, true)
		if rec.Code != http.StatusBadRequest || len(f.Public.Calls) != 0 {
			t.Fatalf("unsupported conversion reached provider: %d %s", rec.Code, rec.Body.String())
		}
	}
}
