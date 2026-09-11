package responsesapi

import (
	"net/http"
	"time"

	"github.com/inferplane/inferplane/internal/audit"
	"github.com/inferplane/inferplane/internal/governance"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/pricing"
	"github.com/inferplane/inferplane/internal/tracing"
	"github.com/inferplane/inferplane/pkg/schema"
	"github.com/inferplane/inferplane/pkg/ulid"
	"go.opentelemetry.io/otel/trace"
)

func subject(p keystore.Principal) governance.Subject {
	return governance.Subject{Team: p.Team, KeyID: p.KeyID, User: p.Owner}
}
func keyPolicy(p keystore.Principal) governance.KeyPolicy {
	return governance.KeyPolicy{
		RatePerMin: p.RPM, TokensPerMinute: p.TPM,
		BudgetMicrosPerMonth: p.BudgetUSDMicros, BudgetMicrosPerDay: p.BudgetUSDMicrosPerDay,
	}
}
func number(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func (h *Handler) denied(req *http.Request, p keystore.Principal, model string, status int, reason string, started time.Time) {
	h.metrics.ObserveRequest("responses", "_rejected", "", p.Team, status, time.Since(started).Seconds(), 0)
	tracing.SetStatus(trace.SpanFromContext(req.Context()), false, reason)
	if h.aud != nil {
		h.aud.Append(audit.Record{
			SchemaVersion: 1, Event: "request_started", ID: ulid.New(), TS: time.Now().UTC().Format(time.RFC3339Nano),
			Principal: audit.PrincipalRef{KeyID: p.KeyID, Team: p.Team},
			Request:   audit.RequestRef{Ingress: "responses", ModelRequested: model, Routing: audit.RoutingFrom(req.Context())},
			Outcome:   &audit.OutcomeRef{Status: status, Error: &reason},
		})
	}
}

func (h *Handler) started(a attempt) {
	if h.aud == nil {
		return
	}
	h.aud.Append(audit.Record{
		SchemaVersion: 1, Event: "request_started", ID: ulid.New(), TS: time.Now().UTC().Format(time.RFC3339Nano),
		Principal: audit.PrincipalRef{KeyID: a.principal.KeyID, Team: a.principal.Team},
		Request:   a.requestRef(),
	})
}

func (a attempt) requestRef() audit.RequestRef {
	ref := audit.RoutingFrom(a.req.Context())
	return audit.RequestRef{
		Ingress: "responses", ModelRequested: a.target.Model, ModelResolved: a.target.Upstream,
		Provider: a.target.ProviderName, Stream: a.proxy.Stream,
		ModelSubstitutedFrom: audit.SubstitutedFrom(a.req.Context()), Routing: ref,
		PIIMasked: ref != nil && ref.Masked,
	}
}

func (a attempt) finish(status int, u *schema.Usage, body []byte, partial bool, ttft float64) {
	var usage *audit.UsageRef
	var cost *audit.CostRef
	span := trace.SpanFromContext(a.req.Context())
	if u != nil {
		write5m, write1h := u.CacheWriteTiers()
		pu := pricing.Usage{
			Input: number(u.InputTokens), Output: number(u.OutputTokens), CacheRead: number(u.CacheReadInputTokens),
			CacheWrite5m: write5m, CacheWrite1h: write1h,
		}
		usage = &audit.UsageRef{
			InputTokens: pu.Input, OutputTokens: pu.Output, CacheReadInputTokens: pu.CacheRead,
			CacheCreationInputTokens: write5m + write1h, CacheCreation5mInputTokens: write5m, CacheCreation1hInputTokens: write1h,
		}
		if a.h.gov != nil {
			amount, missing := a.h.gov.Settle(subject(a.principal), keyPolicy(a.principal), a.target.ProviderName, a.target.Upstream, pu, a.table, a.estimate)
			cost = &audit.CostRef{AmountUSDMicros: amount, PricingMissing: missing, PricingVersion: governance.PricingVersionOf(a.table)}
			if a.h.usage != nil {
				a.h.usage.Record(a.principal.Team, a.principal.Owner, a.target.Upstream, pu, amount)
			}
			tracing.SetCost(span, amount, missing)
		}
		for kind, n := range map[string]int64{"input": pu.Input, "output": pu.Output, "cache_read": pu.CacheRead, "cache_write_5m": write5m, "cache_write_1h": write1h} {
			a.h.metrics.ObserveTokenUsage(kind, a.target.Model, a.target.ProviderName, a.principal.Team, n)
		}
		tracing.SetGenAIResponse(span, a.target.Provider.Name(), a.target.Upstream, pu.Input, pu.Output)
		tracing.SetUsageDetail(span, pu.CacheRead, write5m, write1h)
	}
	if partial {
		tracing.SetPartial(span)
	}
	tracing.SetStatus(span, status/100 == 2 && !partial, "")
	a.h.metrics.ObserveRequest("responses", a.target.Model, a.target.ProviderName, a.principal.Team, status, time.Since(a.started).Seconds(), ttft)
	id := ulid.New()
	var bodyRef string
	if a.h.bodies != nil && status/100 == 2 {
		requestBody := a.inputBody
		if requestBody == nil {
			requestBody = a.proxy.RawBody
		}
		bodyRef = a.h.bodies.Capture(id, a.principal.Team, requestBody, body)
	}
	if a.h.aud == nil {
		return
	}
	rec := audit.Record{
		SchemaVersion: 1, Event: "request_completed", ID: id, TS: time.Now().UTC().Format(time.RFC3339Nano),
		Principal: audit.PrincipalRef{KeyID: a.principal.KeyID, Team: a.principal.Team},
		Request:   a.requestRef(), Outcome: &audit.OutcomeRef{Status: status, Partial: partial},
		Usage: usage, Cost: cost,
		Latency: &audit.LatencyRef{TotalMs: time.Since(a.started).Milliseconds(), TTFTMs: int64(ttft * 1000)},
	}
	if bodyRef != "" {
		rec.BodyRef = &bodyRef
	}
	if id := tracing.TraceID(a.req.Context()); id != "" {
		rec.TraceID = &id
	}
	a.h.aud.Append(rec)
}
