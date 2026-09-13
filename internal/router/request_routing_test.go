package router

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/inferplane/inferplane/api/v1alpha1"
	"github.com/inferplane/inferplane/internal/config"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/live"
	"github.com/inferplane/inferplane/internal/policy"
	"github.com/inferplane/inferplane/internal/sensitivity"
	"github.com/inferplane/inferplane/providers"
	"github.com/inferplane/inferplane/providers/testing/mockprovider"
)

type routingProvider struct {
	providers.Provider
	name string
}

func (p routingProvider) Name() string { return p.name }
func init() {
	for _, name := range []string{"anthropic", "openai_compatible", "bedrock"} {
		providers.Register("request-routing-"+name, func(providers.Config) (providers.Provider, error) {
			return routingProvider{mockprovider.New("upstream"), name}, nil
		})
	}
}

func routingConfig() *config.Config {
	return &config.Config{
		Providers: map[string]config.ProviderConfig{
			"public":   {Type: "request-routing-anthropic", DataBoundary: "external", Region: "us", BaseURL: "https://public.invalid"},
			"private":  {Type: "request-routing-anthropic", DataBoundary: "internal", Region: "eu", BaseURL: "https://private.invalid"},
			"private2": {Type: "request-routing-anthropic", DataBoundary: "internal", Region: "eu", BaseURL: "https://private2.invalid"},
		},
		Models: map[string]config.ModelConfig{
			"premium": {Targets: []config.Target{{Provider: "public", Model: "premium-upstream"}}, ContextWindow: 100000},
			"private": {Aliases: []string{"private-alias"}, Targets: []config.Target{{Provider: "private", Model: "private-upstream"}, {Provider: "public", Model: "private-public"}, {Provider: "private2", Model: "private-retry"}}, ContextWindow: 100000},
			"economy": {Targets: []config.Target{{Provider: "private", Model: "economy-upstream"}}, ContextWindow: 100000},
		},
		ModelFallbacks: map[string]string{"private": "premium"},
		Pricing: config.PricingConfig{Overrides: map[string]map[string]config.RateConfig{
			"public":   {"premium-upstream": {Free: true}, "private-public": {Free: true}},
			"private":  {"private-upstream": {Free: true}, "economy-upstream": {Free: true}},
			"private2": {"private-retry": {Free: true}},
		}},
	}
}
func routingSetup(t *testing.T, cfg *config.Config) (*Router, RequestRoutingInput) {
	t.Helper()
	st, _, err := live.BuildState(cfg)
	if err != nil {
		t.Fatal(err)
	}
	holder := &live.Holder{}
	holder.Swap(st)
	r := New(holder)
	chain, _, err := r.ResolveChain("premium")
	if err != nil {
		t.Fatal(err)
	}
	return r, RequestRoutingInput{Principal: keystore.Principal{Team: "team", KeyOptions: keystore.KeyOptions{Owner: "user"}, AllowedModels: []string{"*"}}, RequestedModel: "premium", Model: "premium", Protocol: "anthropic", RawBody: []byte(`{"messages":[{"role":"user","content":"hello"}],"max_tokens":32}`), Chain: chain, State: st}
}
func privatePolicy(name string, models ...string) *policy.Policy {
	return &policy.Policy{Name: name, Generation: 7, Subject: policy.Subject{Team: "team"}, Rules: []policy.Rule{{Name: "privacy", SensitiveData: &policy.SensitiveData{OnDetected: v1alpha1.InternalOnly, OnUninspectable: v1alpha1.InternalOnly, InternalModels: models}}}}
}
func contextPolicy(name string, mode v1alpha1.ContextMode, target string) *policy.Policy {
	return &policy.Policy{Name: name, Generation: 4, Rules: []policy.Rule{{Name: "context", Routing: &policy.Routing{Context: &policy.Context{Mode: mode, FromModels: []string{"premium"}, SimpleModel: target, ComplexModel: "premium", MaxSimpleInputTokens: 1000, ComplexKeywords: []string{"security"}}}}}}
}
func installRoutingPolicies(r *Router, docs ...*policy.Policy) {
	r.SetRoutingPolicyLookup(func(string, string) ([]*policy.Policy, error) { return docs, nil })
}
func protectedInput(in *RequestRoutingInput) {
	in.RawBody = []byte(`{"messages":[{"role":"user","content":"contact person@example.test"}],"max_tokens":32}`)
}
func requireDenied(t *testing.T, got RequestRoutingResult, err error, reason string) {
	t.Helper()
	var routingErr *RequestRoutingError
	if !errors.As(err, &routingErr) || routingErr.StatusCode != 403 || routingErr.Reason != reason || len(got.Chain) != 0 || got.Decision.Reason != reason {
		t.Fatalf("want denial %s with no attempts, got result=%+v error=%v", reason, got, err)
	}
}

// Removing the privacy filter must expose both the public primary and public retries.
func TestRequestRoutingPrivacyReplacesAndFiltersEveryRetry(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(map[bool]string{false: "public-chain", true: "empty-chain"}[empty], func(t *testing.T) {
			r, in := routingSetup(t, routingConfig())
			protectedInput(&in)
			in.AllowedRegions = []string{"eu"}
			if empty {
				in.Chain = nil
			}
			installRoutingPolicies(r, privatePolicy("privacy", "private-alias"))
			before := slices.Clone(in.RawBody)
			chainBefore := slices.Clone(in.Chain)
			got, err := r.RouteRequest(context.Background(), in)
			if err != nil || got.Model != "private" || len(got.Chain) != 2 {
				t.Fatalf("unsafe selection: %+v, %v", got, err)
			}
			for _, ct := range got.Chain {
				if ct.DataBoundary != "internal" || ct.Model != "private" || ct.Region != "eu" {
					t.Fatalf("unsafe retry candidate: %+v", ct)
				}
			}
			if got.State != in.State || !reflect.DeepEqual(in.RawBody, before) || !reflect.DeepEqual(in.Chain, chainBefore) {
				t.Fatal("decision changed input or topology generation")
			}
			got.Chain[0].Model = "mutated"
			if len(in.Chain) > 0 && in.Chain[0].Model != "premium" {
				t.Fatal("result aliases caller chain")
			}
		})
	}
}

func TestRequestRoutingPrivacyConstraints(t *testing.T) {
	tests := []struct {
		name   string
		edit   func(*config.Config, *RequestRoutingInput, *Router)
		reason string
	}{
		{"original RBAC", func(_ *config.Config, in *RequestRoutingInput, _ *Router) {
			in.Principal.AllowedModels = []string{"private"}
		}, "model_forbidden"},
		{"target RBAC", func(_ *config.Config, in *RequestRoutingInput, _ *Router) {
			in.Principal.AllowedModels = []string{"premium"}
		}, "no_safe_route"},
		{"policy RBAC", func(_ *config.Config, _ *RequestRoutingInput, r *Router) {
			r.SetPolicyGate(func(_ keystore.Principal, m string, _ func(string) string) bool { return m == "premium" })
		}, "no_safe_route"},
		{"region", func(_ *config.Config, in *RequestRoutingInput, _ *Router) { in.AllowedRegions = []string{"ap"} }, "no_safe_route"},
		{"nil snapshot", func(_ *config.Config, in *RequestRoutingInput, _ *Router) { in.State = nil }, "no_safe_route"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := routingConfig()
			r, in := routingSetup(t, cfg)
			protectedInput(&in)
			installRoutingPolicies(r, privatePolicy("p", "private"))
			tt.edit(cfg, &in, r)
			got, err := r.RouteRequest(context.Background(), in)
			requireDenied(t, got, err, tt.reason)
		})
	}
	for _, boundary := range []string{"", "unknown", "external"} {
		t.Run("boundary-"+boundary, func(t *testing.T) {
			cfg := routingConfig()
			for _, name := range []string{"private", "private2"} {
				pc := cfg.Providers[name]
				pc.DataBoundary = boundary
				cfg.Providers[name] = pc
			}
			r, in := routingSetup(t, cfg)
			protectedInput(&in)
			installRoutingPolicies(r, privatePolicy("p", "private"))
			got, err := r.RouteRequest(context.Background(), in)
			requireDenied(t, got, err, "no_safe_route")
		})
	}
}

func TestRequestRoutingPrivacyIntersectionAndBlockWins(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		for _, block := range []bool{false, true} {
			r, in := routingSetup(t, routingConfig())
			protectedInput(&in)
			a, b := privatePolicy("z", "private"), privatePolicy("a", "economy")
			if block {
				b.Rules[0].SensitiveData.OnDetected = v1alpha1.Block
			}
			docs := []*policy.Policy{a, b}
			if reverse {
				slices.Reverse(docs)
			}
			installRoutingPolicies(r, docs...)
			got, err := r.RouteRequest(context.Background(), in)
			reason := "no_safe_route"
			if block {
				reason = "sensitive_blocked"
			}
			requireDenied(t, got, err, reason)
			if len(got.Decision.Policies) != 2 || got.Decision.Policies[0].Name != "a" {
				t.Fatalf("unsorted policy refs: %+v", got.Decision)
			}
		}
	}
	r, in := routingSetup(t, routingConfig())
	protectedInput(&in)
	installRoutingPolicies(r, privatePolicy("z", "economy", "private-alias"), privatePolicy("a", "private"))
	got, err := r.RouteRequest(context.Background(), in)
	if err != nil || got.Model != "private" {
		t.Fatalf("alias intersection: %+v %v", got, err)
	}
}

func TestRequestRoutingCandidateMetadata(t *testing.T) {
	for _, tt := range []struct {
		name          string
		window        int64
		input, output int64
		flags         sensitivity.Result
		caps          []string
		ok            bool
	}{
		{name: "fits exactly", window: 110, input: 100, output: 10, ok: true},
		{name: "unknown context", window: 0, input: 100},
		{name: "input too large", window: 99, input: 100},
		{name: "output too large", window: 110, input: 100, output: 11},
		{name: "addition overflow", window: math.MaxInt64, input: math.MaxInt64 - 5, output: 10},
		{name: "negative estimate", window: 100, input: -1},
		{name: "tools missing", window: 1000, input: 100, flags: sensitivity.Result{HasTools: true}},
		{name: "vision missing", window: 1000, input: 100, flags: sensitivity.Result{HasVision: true}},
		{name: "reasoning missing", window: 1000, input: 100, flags: sensitivity.Result{HasReasoning: true}},
		{name: "structured output missing", window: 1000, input: 100, flags: sensitivity.Result{HasStructuredOutput: true}},
		{name: "all capabilities", window: 1000, input: 100, flags: sensitivity.Result{HasTools: true, HasVision: true, HasReasoning: true, HasStructuredOutput: true}, caps: []string{"tools", "vision", "reasoning", "structured_output"}, ok: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := routingConfig()
			mc := cfg.Models["private"]
			mc.ContextWindow = tt.window
			mc.Capabilities = tt.caps
			cfg.Models["private"] = mc
			r, in := routingSetup(t, cfg)
			installRoutingPolicies(r, privatePolicy("p", "private"))
			result := tt.flags
			result.Categories = []string{"email"}
			result.Complete = true
			result.InputTokens = tt.input
			result.OutputTokens = tt.output
			r.SetRequestInspector(inspectFunc(func(context.Context, string, []byte) (sensitivity.Result, error) { return result, nil }))
			got, err := r.RouteRequest(context.Background(), in)
			if tt.ok {
				if err != nil || got.Model != "private" {
					t.Fatalf("valid candidate refused: %+v %v", got, err)
				}
			} else {
				requireDenied(t, got, err, "no_safe_route")
			}
		})
	}
}

type inspectFunc func(context.Context, string, []byte) (sensitivity.Result, error)

func (f inspectFunc) Inspect(ctx context.Context, p string, b []byte) (sensitivity.Result, error) {
	return f(ctx, p, b)
}

func TestRequestRoutingLookupAndInspectionFailures(t *testing.T) {
	for _, private := range []bool{false, true} {
		t.Run(map[bool]string{false: "context", true: "privacy"}[private], func(t *testing.T) {
			r, in := routingSetup(t, routingConfig())
			doc := contextPolicy("c", v1alpha1.Enforce, "economy")
			if private {
				doc = privatePolicy("p", "private")
			}
			installRoutingPolicies(r, doc)
			in.RawBody = []byte(`{"invalid`)
			got, err := r.RouteRequest(context.Background(), in)
			if private {
				requireDenied(t, got, err, "inspection_failed")
			} else if err != nil || got.Model != "premium" || !reflect.DeepEqual(got.Chain, in.Chain) || got.Decision.Reason != "context_inspection_failed" {
				t.Fatalf("context failure changed safe route: %+v %v", got, err)
			}
		})
	}
	for _, lookupErr := range []error{policy.ErrSensitivePolicyRejected, errors.New("private prompt in dependency error")} {
		r, in := routingSetup(t, routingConfig())
		in.CountOnly = true
		r.SetRoutingPolicyLookup(func(string, string) ([]*policy.Policy, error) { return nil, lookupErr })
		r.SetRequestInspector(inspectFunc(func(context.Context, string, []byte) (sensitivity.Result, error) {
			t.Fatal("inspection after rejected lookup")
			return sensitivity.Result{}, nil
		}))
		got, err := r.RouteRequest(context.Background(), in)
		requireDenied(t, got, err, "policy_lookup_failed")
		if strings.Contains(err.Error(), "private prompt") {
			t.Fatal("lookup leaked dependency error")
		}
		if lookupErr == policy.ErrSensitivePolicyRejected && !errors.Is(err, policy.ErrSensitivePolicyRejected) {
			t.Fatal("lost rejection sentinel")
		}
	}
}

func TestRequestRoutingNoPoliciesSkipsInspectionAndOwnsChain(t *testing.T) {
	r, in := routingSetup(t, routingConfig())
	lookups := 0
	r.SetRoutingPolicyLookup(func(team, user string) ([]*policy.Policy, error) {
		lookups++
		if team != "team" || user != "user" {
			t.Fatal("wrong subject")
		}
		return nil, nil
	})
	r.SetRequestInspector(inspectFunc(func(context.Context, string, []byte) (sensitivity.Result, error) {
		t.Fatal("inspected without rules")
		return sensitivity.Result{}, nil
	}))
	got, err := r.RouteRequest(context.Background(), in)
	if err != nil || lookups != 1 || got.Model != "premium" || !reflect.DeepEqual(got.Chain, in.Chain) || got.Decision.Inspection != "not_inspected" {
		t.Fatalf("legacy route changed: %+v %v", got, err)
	}
	got.Chain[0].ProviderName = "mutated"
	if in.Chain[0].ProviderName != "public" {
		t.Fatal("chain aliases input")
	}
}

func TestRequestRoutingContextDecisions(t *testing.T) {
	for _, tt := range []struct {
		name                   string
		mode                   v1alpha1.ContextMode
		body                   string
		want, proposed, reason string
	}{
		{"shadow", v1alpha1.Shadow, `{"messages":[{"role":"user","content":"hi"}]}`, "premium", "economy", "context_shadow"},
		{"enforce", v1alpha1.Enforce, `{"messages":[{"role":"user","content":"hi"}]}`, "economy", "economy", "context_selected"},
		{"decoded keyword", v1alpha1.Enforce, `{"messages":[{"role":"user","content":"SECU\u0052ITY"}]}`, "premium", "premium", "context_unchanged"},
		{"history", v1alpha1.Enforce, `{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"hi"},{"role":"user","content":"hi"}]}`, "premium", "economy", "context_ineligible"},
		{"tools", v1alpha1.Enforce, `{"messages":[{"role":"user","content":"hi"}],"tools":[{"name":"lookup","input_schema":{"type":"object"}}]}`, "premium", "economy", "context_ineligible"},
		{"opaque", v1alpha1.Enforce, `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","data":"AA=="}}]}]}`, "premium", "economy", "context_ineligible"},
		{"reasoning", v1alpha1.Enforce, `{"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":10}}`, "premium", "economy", "context_ineligible"},
		{"structured", v1alpha1.Enforce, `{"messages":[{"role":"user","content":"hi"}],"output_config":{"format":{"type":"json_schema","schema":{"type":"object"}}}}`, "premium", "economy", "context_ineligible"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, in := routingSetup(t, routingConfig())
			in.RawBody = []byte(tt.body)
			installRoutingPolicies(r, contextPolicy("c", tt.mode, "economy"))
			got, err := r.RouteRequest(context.Background(), in)
			if err != nil || got.Model != tt.want || got.Decision.ProposedModel != tt.proposed || got.Decision.Reason != tt.reason {
				t.Fatalf("context result: %+v %v", got, err)
			}
			if tt.want == "premium" && !reflect.DeepEqual(got.Chain, in.Chain) {
				t.Fatal("context changed original attempts")
			}
		})
	}
}

func TestRequestRoutingContextThresholdAndAgreement(t *testing.T) {
	for _, tokens := range []int64{1000, 1001} {
		r, in := routingSetup(t, routingConfig())
		installRoutingPolicies(r, contextPolicy("c", v1alpha1.Enforce, "economy"))
		r.SetRequestInspector(inspectFunc(func(context.Context, string, []byte) (sensitivity.Result, error) {
			return sensitivity.Result{Complete: true, InputTokens: tokens, UserTurns: 1}, nil
		}))
		got, err := r.RouteRequest(context.Background(), in)
		want := "economy"
		if tokens > 1000 {
			want = "premium"
		}
		if err != nil || got.Model != want {
			t.Fatalf("threshold %d: %+v %v", tokens, got, err)
		}
	}
	for _, reverse := range []bool{false, true} {
		for _, conflict := range []bool{false, true} {
			r, in := routingSetup(t, routingConfig())
			a, b := contextPolicy("z", v1alpha1.Enforce, "economy"), contextPolicy("a", v1alpha1.Shadow, "economy")
			if conflict {
				b.Rules[0].Routing.Context.SimpleModel = "private"
			}
			docs := []*policy.Policy{a, b}
			if reverse {
				slices.Reverse(docs)
			}
			installRoutingPolicies(r, docs...)
			got, err := r.RouteRequest(context.Background(), in)
			want := "context_shadow"
			if conflict {
				want = "context_conflict"
			}
			if err != nil || got.Model != "premium" || got.Decision.Reason != want || got.Decision.Mode != "Shadow" || got.Decision.Recommendations[0].Policy.Name != "a" {
				t.Fatalf("non-deterministic overlap: %+v %v", got, err)
			}
		}
	}
}

func TestRequestRoutingContextUnavailableKeepsSafeRoute(t *testing.T) {
	for _, why := range []string{"missing", "rbac", "unpriced", "context", "region", "fallback-only"} {
		t.Run(why, func(t *testing.T) {
			cfg := routingConfig()
			target := "economy"
			if why == "missing" {
				target = "absent"
			}
			if why == "unpriced" {
				delete(cfg.Pricing.Overrides["private"], "economy-upstream")
			}
			if why == "context" {
				mc := cfg.Models["economy"]
				mc.ContextWindow = 1
				cfg.Models["economy"] = mc
			}
			if why == "fallback-only" {
				cfg.ModelFallbacks["economy"] = "premium"
				mc := cfg.Models["economy"]
				mc.Targets = nil
				cfg.Models["economy"] = mc
				delete(cfg.Pricing.Overrides["private"], "economy-upstream")
			}
			r, in := routingSetup(t, cfg)
			if why == "rbac" {
				in.Principal.AllowedModels = []string{"premium"}
			}
			if why == "region" {
				in.AllowedRegions = []string{"us"}
			}
			installRoutingPolicies(r, contextPolicy("c", v1alpha1.Enforce, target))
			got, err := r.RouteRequest(context.Background(), in)
			if err != nil || got.Model != "premium" || !reflect.DeepEqual(got.Chain, in.Chain) || got.Decision.Reason != "context_unavailable" {
				t.Fatalf("unavailable recommendation weakened original: %+v %v", got, err)
			}
		})
	}
}

func TestRequestRoutingPrivacyPrecedesShadowAndCountOnly(t *testing.T) {
	for _, count := range []bool{false, true} {
		r, in := routingSetup(t, routingConfig())
		protectedInput(&in)
		in.CountOnly = count
		installRoutingPolicies(r, contextPolicy("c", v1alpha1.Shadow, "economy"), privatePolicy("p", "private"))
		got, err := r.RouteRequest(context.Background(), in)
		if err != nil || got.Model != "private" || len(got.Chain) != 2 {
			t.Fatalf("privacy weakened: %+v %v", got, err)
		}
		if count && (got.Decision.ProposedModel != "" || len(got.Decision.Recommendations) != 0) {
			t.Fatal("count performed context optimization")
		}
	}
}

func TestRequestRoutingUsesOnlySuppliedGeneration(t *testing.T) {
	r, in := routingSetup(t, routingConfig())
	protectedInput(&in)
	installRoutingPolicies(r, privatePolicy("p", "private-alias"))
	in.Principal.AllowedModels = []string{"premium", "private-alias"}
	inspected := 0
	r.SetRequestInspector(inspectFunc(func(ctx context.Context, p string, raw []byte) (sensitivity.Result, error) {
		inspected++
		cfg := routingConfig()
		mc := cfg.Models["private"]
		mc.Aliases = nil
		cfg.Models["private"] = mc
		for _, name := range []string{"private", "private2"} {
			pc := cfg.Providers[name]
			pc.DataBoundary = "external"
			pc.Region = "us"
			cfg.Providers[name] = pc
		}
		delete(cfg.Pricing.Overrides["private"], "private-upstream")
		next, _, err := live.BuildState(cfg)
		if err != nil {
			t.Fatal(err)
		}
		r.live.Swap(next)
		return sensitivity.NewInspector().Inspect(ctx, p, raw)
	}))
	in.AllowedRegions = []string{"eu"}
	got, err := r.RouteRequest(context.Background(), in)
	if err != nil || got.State != in.State || got.Model != "private" || len(got.Chain) != 2 || inspected != 1 {
		t.Fatalf("mixed generations: %+v %v", got, err)
	}
	for _, ct := range got.Chain {
		p, _ := in.State.Provider(ct.ProviderName)
		if ct.Provider != p || ct.DataBoundary != "internal" || !got.State.Pricing().HasRate(ct.ProviderName, ct.Upstream) {
			t.Fatal("provider/pricing drift")
		}
	}
}

func TestRequestRoutingMatchesSourceAfterBudgetTier(t *testing.T) {
	r, in := routingSetup(t, routingConfig())
	r.SetTierGate(func(keystore.Principal) map[string]string { return map[string]string{"premium": "economy"} })
	in.Model, _ = r.SubstituteTier(in.Principal, in.Model)
	in.Chain, in.State, _ = r.ResolveChain(in.Model)
	c := contextPolicy("c", v1alpha1.Enforce, "private")
	installRoutingPolicies(r, c)
	got, err := r.RouteRequest(context.Background(), in)
	if err != nil || got.Model != "economy" || got.Decision.ProposedModel != "" {
		t.Fatalf("context undid tier: %+v %v", got, err)
	}
	c.Rules[0].Routing.Context.FromModels = []string{"economy"}
	got, err = r.RouteRequest(context.Background(), in)
	if err != nil || got.Model != "private" {
		t.Fatalf("explicit post-tier source did not match: %+v %v", got, err)
	}
}

func TestRequestRoutingRechecksPromotedPrimaryAndAllFallbacks(t *testing.T) {
	r, in := routingSetup(t, routingConfig())
	in.Principal.AllowedModels = []string{"premium"}
	in.Chain, in.State, _ = r.ResolveChain("private")
	installRoutingPolicies(r, contextPolicy("c", v1alpha1.Shadow, "economy"))
	got, err := r.RouteRequest(context.Background(), in)
	if err != nil || len(got.Chain) != 1 || got.Chain[0].Model != "premium" {
		t.Fatalf("promoted primary bypassed RBAC: %+v %v", got, err)
	}
}

func TestRequestRoutingPhysicalCompatibility(t *testing.T) {
	for _, tt := range []struct {
		ingress, egress string
		feature         string
		ok              bool
	}{
		{"openai", "anthropic", "", false}, {"bedrock", "anthropic", "", false},
		{"anthropic", "openai_compatible", "", true}, {"openai", "openai_compatible", "vision", true},
		{"anthropic", "openai_compatible", "vision", false}, {"anthropic", "openai_compatible", "reasoning", false}, {"anthropic", "openai_compatible", "structured_output", false},
		{"openai", "bedrock", "vision", false}, {"bedrock", "openai_compatible", "reasoning", false},
		{"anthropic", "bedrock", "vision", false},
	} {
		t.Run(tt.ingress+"-"+tt.egress+"-"+tt.feature, func(t *testing.T) {
			cfg := routingConfig()
			for _, name := range []string{"private", "private2"} {
				pc := cfg.Providers[name]
				pc.Type = "request-routing-" + tt.egress
				cfg.Providers[name] = pc
			}
			pc := cfg.Providers["public"]
			if tt.ingress == "openai" {
				pc.Type = "request-routing-openai_compatible"
			} else if tt.ingress == "bedrock" {
				pc.Type = "request-routing-bedrock"
			}
			cfg.Providers["public"] = pc
			mc := cfg.Models["private"]
			mc.Capabilities = []string{"tools", "vision", "reasoning", "structured_output"}
			cfg.Models["private"] = mc
			r, in := routingSetup(t, cfg)
			in.Protocol = tt.ingress
			installRoutingPolicies(r, privatePolicy("p", "private"))
			r.SetRequestInspector(inspectFunc(func(context.Context, string, []byte) (sensitivity.Result, error) {
				return sensitivity.Result{Complete: true, Categories: []string{"email"}, HasVision: tt.feature == "vision", HasReasoning: tt.feature == "reasoning", HasStructuredOutput: tt.feature == "structured_output", InputTokens: 100}, nil
			}))
			got, err := r.RouteRequest(context.Background(), in)
			if tt.ok {
				if err != nil || got.Model != "private" {
					t.Fatalf("compatible route refused: %+v %v", got, err)
				}
			} else {
				requireDenied(t, got, err, "no_safe_route")
			}
		})
	}
}

func TestRequestRoutingDecisionDoesNotExposeTextOrIdentity(t *testing.T) {
	r, in := routingSetup(t, routingConfig())
	protectedInput(&in)
	in.Principal.KeyID = "key-private-id"
	in.Principal.Owner = "private-user"
	installRoutingPolicies(r, privatePolicy("p", "private"))
	got, err := r.RouteRequest(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(got.Decision)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"person@example.test", "key-private-id", "private-user"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("decision exposed %s", secret)
		}
	}
}

// A new provider must be able to opt into native Bedrock ingress without an
// internal/router edit. The opt-in must not bypass known built-in feature loss.
type ingressCapableProvider struct {
	routingProvider
	supported bool
}

func (p ingressCapableProvider) SupportsIngress(string) bool { return p.supported }
func init() {
	for _, name := range []string{"custom", "anthropic", "openai_compatible", "bedrock"} {
		providers.Register("request-routing-capable-"+name, func(providers.Config) (providers.Provider, error) {
			return ingressCapableProvider{routingProvider{mockprovider.New("upstream"), name}, true}, nil
		})
	}
	providers.Register("request-routing-incapable", func(providers.Config) (providers.Provider, error) {
		return ingressCapableProvider{routingProvider{mockprovider.New("upstream"), "custom"}, false}, nil
	})
}
func TestRequestRoutingProviderIngressOptIn(t *testing.T) {
	for _, tt := range []struct {
		name, ingress, provider string
		feature, ok             bool
	}{
		{"custom native bedrock", "bedrock", "capable-custom", false, true},
		{"custom lossless feature path", "bedrock", "capable-custom", true, true},
		{"explicit refusal", "anthropic", "incapable", false, false},
		{"builtin direct anthropic still forbidden", "openai", "capable-anthropic", false, false},
		{"builtin conversion still loses vision", "anthropic", "capable-openai_compatible", true, false},
		{"builtin converse still loses vision", "bedrock", "capable-bedrock", true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := routingConfig()
			for _, name := range []string{"private", "private2"} {
				pc := cfg.Providers[name]
				pc.Type = "request-routing-" + tt.provider
				cfg.Providers[name] = pc
			}
			mc := cfg.Models["private"]
			mc.Capabilities = []string{"vision"}
			cfg.Models["private"] = mc
			r, in := routingSetup(t, cfg)
			in.Protocol = tt.ingress
			in.Chain = nil
			installRoutingPolicies(r, privatePolicy("p", "private"))
			r.SetRequestInspector(inspectFunc(func(context.Context, string, []byte) (sensitivity.Result, error) {
				return sensitivity.Result{Complete: !tt.feature, HasVision: tt.feature, Categories: []string{"email"}, InputTokens: 100}, nil
			}))
			got, err := r.RouteRequest(context.Background(), in)
			if tt.ok {
				if err != nil || got.Model != "private" {
					t.Fatalf("provider cannot opt in: %+v %v", got, err)
				}
			} else {
				requireDenied(t, got, err, "no_safe_route")
			}
		})
	}
}

func TestRequestRoutingPrivacyFiltersBeforeBreakerRanking(t *testing.T) {
	r, in := routingSetup(t, routingConfig())
	protectedInput(&in)
	installRoutingPolicies(r, privatePolicy("p", "private"))
	for _, name := range []string{"private", "private2"} {
		id, _ := in.State.Identity(name)
		for i := 0; i < 5; i++ {
			r.brk.RecordFailure(id)
		}
	}
	got, err := r.RouteRequest(context.Background(), in)
	// The public fallback's closed breaker must not prevent the existing
	// all-open retry rule from considering the two approved internal providers.
	if err != nil || got.Model != "private" || len(got.Chain) != 2 {
		t.Fatalf("unsafe candidate influenced breaker ranking: %+v %v", got, err)
	}
	for _, ct := range got.Chain {
		if ct.DataBoundary != "internal" {
			t.Fatal("reinserted public fallback")
		}
	}
}

func TestRequestRoutingUnknownOpaqueConversionRefused(t *testing.T) {
	cfg := routingConfig()
	for _, name := range []string{"private", "private2"} {
		pc := cfg.Providers[name]
		pc.Type = "request-routing-openai_compatible"
		cfg.Providers[name] = pc
	}
	r, in := routingSetup(t, cfg)
	installRoutingPolicies(r, privatePolicy("p", "private"))
	in.RawBody = []byte(`{"messages":[{"role":"user","content":[{"type":"future_block","payload":"opaque"}]}]}`)
	got, err := r.RouteRequest(context.Background(), in)
	requireDenied(t, got, err, "no_safe_route")
}

func TestRequestRoutingBedrockRejectsOpenAITools(t *testing.T) {
	cfg := routingConfig()
	for _, name := range []string{"private", "private2"} {
		pc := cfg.Providers[name]
		pc.Type = "request-routing-bedrock"
		cfg.Providers[name] = pc
	}
	mc := cfg.Models["private"]
	mc.Capabilities = []string{"tools"}
	cfg.Models["private"] = mc
	r, in := routingSetup(t, cfg)
	in.Protocol = "openai"
	in.Chain = nil
	in.RawBody = []byte(`{"messages":[{"role":"user","content":"person@example.test"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`)
	installRoutingPolicies(r, privatePolicy("p", "private"))
	got, err := r.RouteRequest(context.Background(), in)
	requireDenied(t, got, err, "no_safe_route")
}

func TestRequestRoutingBedrockEffectiveAPIUsesAllSnapshotRoutes(t *testing.T) {
	cfg := routingConfig()
	for _, name := range []string{"private", "private2"} {
		pc := cfg.Providers[name]
		pc.Type = "request-routing-bedrock"
		cfg.Providers[name] = pc
	}
	// BuildState configures API per provider/upstream, across ALL model routes.
	// An override elsewhere wins over the selected model's empty/default API.
	cfg.Models["other-route"] = config.ModelConfig{Targets: []config.Target{{Provider: "private", Model: "private-upstream", API: "invoke_model"}, {Provider: "private2", Model: "private-retry", API: "invoke_model"}}}
	r, in := routingSetup(t, cfg)
	in.Protocol = "openai"
	in.Chain = nil
	protectedInput(&in)
	installRoutingPolicies(r, privatePolicy("p", "private"))
	got, err := r.RouteRequest(context.Background(), in)
	requireDenied(t, got, err, "no_safe_route")
}

func TestRequestRoutingPrivacyCeilingAppliesToContextAndFallbackMetadata(t *testing.T) {
	for _, mode := range []v1alpha1.ContextMode{v1alpha1.Shadow, v1alpha1.Enforce} {
		r, in := routingSetup(t, routingConfig())
		protectedInput(&in)
		installRoutingPolicies(r, privatePolicy("p", "private"), contextPolicy("c", mode, "economy"))
		got, err := r.RouteRequest(context.Background(), in)
		if err != nil || got.Model != "private" || len(got.Chain) != 2 {
			t.Fatalf("context escaped privacy ceiling: %+v %v", got, err)
		}
	}
	for _, why := range []string{"unpriced", "unknown context", "denied", "region", "boundary"} {
		t.Run(why, func(t *testing.T) {
			cfg := routingConfig()
			cfg.ModelFallbacks["private"] = "economy"
			cfg.Providers["fallback"] = config.ProviderConfig{Type: "request-routing-anthropic", DataBoundary: "internal", Region: "eu", BaseURL: "https://fallback.invalid"}
			mc := cfg.Models["economy"]
			mc.Targets[0].Provider = "fallback"
			cfg.Models["economy"] = mc
			delete(cfg.Pricing.Overrides["private"], "economy-upstream")
			cfg.Pricing.Overrides["fallback"] = map[string]config.RateConfig{"economy-upstream": {Free: true}}
			switch why {
			case "unpriced":
				delete(cfg.Pricing.Overrides, "fallback")
			case "unknown context":
				mc.ContextWindow = 0
				cfg.Models["economy"] = mc
			case "region":
				pc := cfg.Providers["fallback"]
				pc.Region = "us"
				cfg.Providers["fallback"] = pc
			case "boundary":
				pc := cfg.Providers["fallback"]
				pc.DataBoundary = "unknown"
				cfg.Providers["fallback"] = pc
			}
			r, in := routingSetup(t, cfg)
			protectedInput(&in)
			in.AllowedRegions = []string{"eu"}
			if why == "denied" {
				in.Principal.AllowedModels = []string{"premium", "private"}
			}
			installRoutingPolicies(r, privatePolicy("p", "private", "economy"))
			// Preserve the existing candidate chain, including its appended fallback.
			// Make private the requested current model so privacy need not re-resolve it.
			in.RequestedModel = "private"
			in.Model = "private"
			in.Chain, in.State, _ = r.ResolveChain("private")
			got, err := r.RouteRequest(context.Background(), in)
			if err != nil || len(got.Chain) != 2 {
				t.Fatalf("safe original lost or unsafe fallback kept: %+v %v", got, err)
			}
			for _, ct := range got.Chain {
				if ct.Model != "private" || ct.DataBoundary != "internal" {
					t.Fatalf("unsafe retry: %+v", ct)
				}
			}
		})
	}
}

func TestRequestRoutingUninspectableAndDetectedBlockTie(t *testing.T) {
	for _, count := range []bool{false, true} {
		r, in := routingSetup(t, routingConfig())
		in.CountOnly = count
		in.RawBody = []byte(`{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"https://example.test/person@example.test"}}]}]}`)
		doc := privatePolicy("p", "private")
		doc.Rules[0].SensitiveData.OnUninspectable = v1alpha1.Block
		installRoutingPolicies(r, doc)
		got, err := r.RouteRequest(context.Background(), in)
		requireDenied(t, got, err, "sensitive_blocked")
	}
}

func TestRequestRoutingBedrockConflictingOverridesFailClosed(t *testing.T) {
	cfg := routingConfig()
	for _, name := range []string{"private", "private2"} {
		pc := cfg.Providers[name]
		pc.Type = "request-routing-bedrock"
		cfg.Providers[name] = pc
	}
	mc := cfg.Models["private"]
	for i := range mc.Targets {
		if mc.Targets[i].Provider != "public" {
			mc.Targets[i].API = "invoke_model"
		}
	}
	cfg.Models["private"] = mc
	cfg.Models["other-route"] = config.ModelConfig{Targets: []config.Target{{Provider: "private", Model: "private-upstream", API: "converse"}, {Provider: "private2", Model: "private-retry", API: "converse"}}}
	r, in := routingSetup(t, cfg)
	protectedInput(&in)
	installRoutingPolicies(r, privatePolicy("p", "private"))
	got, err := r.RouteRequest(context.Background(), in)
	requireDenied(t, got, err, "no_safe_route")
}
