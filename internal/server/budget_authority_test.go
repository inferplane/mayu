package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/inferplane/inferplane/internal/budget"
	"github.com/inferplane/inferplane/internal/governance"
	"github.com/inferplane/inferplane/internal/limiter"
	"github.com/inferplane/inferplane/internal/server/routingtest"
	"github.com/inferplane/inferplane/pkg/schema"
)

type authorityProbe struct {
	reserves, finishes int
	failure            error
	complete           bool
	actual             *int64
	invalid            bool
}

func (a *authorityProbe) Reserve(_ context.Context, s governance.Subject, bound int64) (*governance.BudgetPermit, error) {
	a.reserves++
	if s.Team != "team" || s.User != "user" || s.KeyID == "" {
		return nil, errors.New("lost identity")
	}
	if a.failure != nil {
		return nil, a.failure
	}
	return &governance.BudgetPermit{ID: "local-permit", BoundMicroUSD: bound}, nil
}
func (a *authorityProbe) Finish(_ context.Context, permit *governance.BudgetPermit, actual *int64, complete bool) error {
	a.finishes++
	a.invalid = permit.InvalidUsage
	a.complete = complete
	if actual != nil {
		v := *actual
		a.actual = &v
	}
	return nil
}

func TestAuthorityRefusesMultipleCompletionsBeforeDispatch(t *testing.T) {
	for _, i := range authorityIngresses() {
		t.Run(i.name(), func(t *testing.T) {
			f := routingtest.New(t, nil)
			a := &authorityProbe{}
			g := governance.NewGovernor(nil, limiter.NewMemory(), budget.NewMemory(), nil)
			g.SetBudgetAuthority(a)
			raw := strings.TrimSuffix(authorityBody(i), "}") + `,"n":2}`
			rec := policyDo(policyMux(f, nil, g, nil, nil), i.path, raw, true)
			if rec.Code != 400 || a.reserves != 0 || len(f.Public.Calls) != 0 {
				t.Fatalf("multiplicity dispatched: status=%d reserve=%d calls=%d", rec.Code, a.reserves, len(f.Public.Calls))
			}
		})
	}
}

func TestAuthorityNeverRefundsUnknownCacheTierOrInvalidUsage(t *testing.T) {
	for _, i := range authorityIngresses() {
		for _, kind := range []string{"unknown-cache-tier", "partial-cache-tier", "overflow-output", "uncertain-overflow"} {
			t.Run(i.name()+"/"+kind, func(t *testing.T) {
				f := routingtest.New(t, nil)
				in, out, cache, part := int64(2), int64(1), int64(100), int64(50)
				u := &schema.Usage{InputTokens: &in, OutputTokens: &out}
				switch kind {
				case "unknown-cache-tier":
					u.CacheCreationInputTokens = &cache
				case "partial-cache-tier":
					u.CacheCreationInputTokens = &cache
					u.CacheCreation = &schema.CacheCreation{Ephemeral5mInputTokens: &part}
				case "overflow-output":
					out = 1 << 62
				case "uncertain-overflow":
					out = 1 << 62
					u.AccountingUncertain = true
				}
				f.Public.Usage = u
				a := &authorityProbe{}
				g := governance.NewGovernor(nil, limiter.NewMemory(), budget.NewMemory(), nil)
				g.SetBudgetAuthority(a)
				_ = policyDo(policyMux(f, nil, g, nil, nil), i.path, authorityBody(i), true)
				if a.finishes != 1 || a.complete || a.actual != nil {
					t.Fatalf("unproven monetary refund: %+v", a)
				}
				if (kind == "overflow-output" || kind == "uncertain-overflow") && !a.invalid {
					t.Fatal("out-of-bound usage did not poison admission")
				}
			})
		}
	}
}

func authorityIngresses() []policyIngress {
	return append(append([]policyIngress(nil), policyIngresses...),
		policyIngress{"responses", "/v1/responses", false}, policyIngress{"responses", "/v1/responses", true})
}
func authorityBody(i policyIngress) string {
	raw := `{"model":"premium","messages":[{"role":"user","content":"hello"}],"max_tokens":32}`
	if i.protocol == "responses" {
		raw = `{"model":"premium","input":"hello","max_output_tokens":32}`
	}
	if i.stream {
		raw = strings.TrimSuffix(raw, "}") + `,"stream":true}`
	}
	return raw
}
func TestEveryIngressReservesDurablyBeforeProvider(t *testing.T) {
	for _, i := range authorityIngresses() {
		t.Run(i.name(), func(t *testing.T) {
			f := routingtest.New(t, nil)
			a := &authorityProbe{failure: governance.ErrAuthorityExhausted}
			g := governance.NewGovernor(nil, limiter.NewMemory(), budget.NewMemory(), nil)
			g.SetBudgetAuthority(a)
			rec := policyDo(policyMux(f, nil, g, nil, nil), i.path, authorityBody(i), true)
			if rec.Code != http.StatusPaymentRequired || a.reserves != 1 || len(f.Public.Calls) != 0 {
				t.Fatalf("authority bypass: status=%d reserve=%d calls=%d", rec.Code, a.reserves, len(f.Public.Calls))
			}
		})
	}
}
func TestEveryIngressSettlesPermitOnceOrRetainsUncertainty(t *testing.T) {
	for _, i := range authorityIngresses() {
		for _, partial := range []bool{false, true} {
			if partial && !i.stream {
				continue
			}
			t.Run(i.name()+map[bool]string{false: "/complete", true: "/partial"}[partial], func(t *testing.T) {
				f := routingtest.New(t, nil)
				in, out := int64(2), int64(1)
				f.Public.Usage = &schema.Usage{InputTokens: &in, OutputTokens: &out}
				f.Public.Partial = partial
				a := &authorityProbe{}
				g := governance.NewGovernor(nil, limiter.NewMemory(), budget.NewMemory(), nil)
				g.SetBudgetAuthority(a)
				rec := policyDo(policyMux(f, nil, g, nil, nil), i.path, authorityBody(i), true)
				if rec.Code != 200 || a.reserves != 1 || a.finishes != 1 || a.complete == partial || a.actual == nil {
					t.Fatalf("permit lifecycle: status=%d %+v", rec.Code, a)
				}
			})
		}
	}
}
func TestCountEndpointsNeverReserveAuthority(t *testing.T) {
	f := routingtest.New(t, nil)
	a := &authorityProbe{failure: governance.ErrAuthorityExhausted}
	g := governance.NewGovernor(nil, limiter.NewMemory(), budget.NewMemory(), nil)
	g.SetBudgetAuthority(a)
	for _, protocol := range []string{"anthropic", "bedrock"} {
		path := "/v1/messages/count_tokens"
		if protocol == "bedrock" {
			path = "/model/premium/count-tokens"
		}
		if got := policyDo(policyMux(f, nil, g, nil, nil), path, countBody(protocol, `{"model":"premium","messages":[{"role":"user","content":"hi"}]}`), true); got.Code != 200 {
			t.Fatal(got.Code)
		}
	}
	if a.reserves != 0 {
		t.Fatal("nonbillable count reserved authority")
	}
}
