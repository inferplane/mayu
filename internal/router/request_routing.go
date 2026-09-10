package router

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/inferplane/inferplane/api/v1alpha1"
	"github.com/inferplane/inferplane/internal/config"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/live"
	"github.com/inferplane/inferplane/internal/policy"
	"github.com/inferplane/inferplane/internal/sensitivity"
	"github.com/inferplane/inferplane/providers"
)

// RequestRoutingInput borrows original, unmasked ingress bytes and a route
// resolved on State. RequestedModel is the resolved model BEFORE budget-tier
// selection (an alias is allowed); Model is the current, post-tier model.
// An empty RequestedModel defaults to Model. The original model must pass RBAC.
// Chain may be empty after ingress/region filtering; privacy can still choose
// an approved alternative. AllowedRegions must be supplied again for those
// alternatives. Bedrock counts supply the already-decoded inner body.
type RequestRoutingInput struct {
	Principal      keystore.Principal
	Protocol       string
	RawBody        []byte
	RequestedModel string
	Model          string
	Chain          []ChainTarget
	State          *live.State
	AllowedRegions []string
	CountOnly      bool
}

// RequestRoutingResult owns its chain and decision slices. State is exactly the
// supplied immutable generation: use it for attempts, context checks and pricing.
// On error Model/Chain/Decision.SelectedModel are empty; Decision describes denial.
type RequestRoutingResult struct {
	Model    string
	Chain    []ChainTarget
	State    *live.State
	Decision RoutingDecision
}

// RoutingPolicyRef identifies an applicable rule without its content or subject.
type RoutingPolicyRef struct {
	Name       string `json:"name"`
	Generation int64  `json:"generation"`
	Rule       string `json:"rule"`
}

// RoutingRecommendation is one context rule's recommendation, not an approved
// attempt. Entries follow policy name, generation and rule-name order.
type RoutingRecommendation struct {
	Policy RoutingPolicyRef `json:"policy"`
	Model  string           `json:"model"`
	Reason string           `json:"reason"`
}

// RoutingDecision contains only model names, policy references and bounded
// inspection/category/reason values. It never includes request text or identity.
// A proposed model is observational: only RequestRoutingResult.Chain may be tried.
type RoutingDecision struct {
	RequestedModel  string                  `json:"requested_model"`
	SelectedModel   string                  `json:"selected_model,omitempty"`
	ProposedModel   string                  `json:"proposed_model,omitempty"`
	Mode            string                  `json:"mode,omitempty"`
	Reason          string                  `json:"reason"`
	Inspection      string                  `json:"inspection"`
	Privacy         string                  `json:"privacy,omitempty"`
	Categories      []string                `json:"categories,omitempty"`
	Policies        []RoutingPolicyRef      `json:"policies,omitempty"`
	Recommendations []RoutingRecommendation `json:"recommendations,omitempty"`
}

// RequestRoutingError is a security refusal (StatusCode 403). Ingresses translate
// it into their own error shape. Count-only endpoints instead estimate locally
// with HTTP 200 and no upstream call. Error/Unwrap never expose dependency text;
// only policy.ErrSensitivePolicyRejected is preserved as an optional cause.
type RequestRoutingError struct {
	StatusCode int
	Reason     string
	cause      error
}

func (e *RequestRoutingError) Error() string {
	return fmt.Sprintf("router: request routing denied (%s)", e.Reason)
}
func (e *RequestRoutingError) Unwrap() error { return e.cause }

// IngressSupporter is an optional structural provider contract. Implementations
// need not import router. True attests that this provider instance supports the
// named ingress ("anthropic", "openai", "bedrock") without losing submitted
// features, including opaque content, on its configured paths. False refuses
// that ingress. Model capability/context checks still apply, and true NEVER
// overrides known unsafe built-in paths (e.g. OpenAI -> direct Anthropic,
// InvokeModel's Anthropic-shaped body requirement, or lossy Converse conversion).
// Without it, existing Anthropic/OpenAI text paths retain legacy compatibility;
// unknown providers cannot claim Bedrock or feature-preserving conversion.
type IngressSupporter interface {
	SupportsIngress(protocol string) bool
}

// RouteRequest decides every permitted attempt before any upstream call. The
// supplied state is the only topology/pricing generation consulted. Input bytes
// and slices are borrowed; returned slices belong to the caller. On error Chain
// is empty and Decision remains safe to audit; token-count callers must estimate
// locally and return 200, never forward a refused body upstream.
func (r *Router) RouteRequest(ctx context.Context, in RequestRoutingInput) (RequestRoutingResult, error) {
	requested := in.RequestedModel
	if requested == "" {
		requested = in.Model
	}
	out := RequestRoutingResult{State: in.State, Decision: RoutingDecision{RequestedModel: requested, Reason: "unchanged", Inspection: "not_inspected"}}
	// Lookup errors must be checked even for count-only/otherwise ineligible
	// requests. Never expose arbitrary dependency error text in a routing error.
	var docs []*policy.Policy
	if r.routingPolicies != nil {
		var err error
		docs, err = r.routingPolicies(in.Principal.Team, in.Principal.Owner)
		if err != nil {
			var cause error
			if errors.Is(err, policy.ErrSensitivePolicyRejected) {
				cause = policy.ErrSensitivePolicyRejected
			}
			return denyRouting(out, "policy_lookup_failed", cause)
		}
	}
	if in.State == nil {
		return denyRouting(out, "no_safe_route", nil)
	}
	st := in.State
	in.Model = st.Canonical(in.Model)
	rules := requestRules(docs, st, in.Model)
	for _, rule := range rules {
		out.Decision.Policies = append(out.Decision.Policies, rule.ref)
	}
	if !r.allowsInState(in.Principal, st.Canonical(requested), st) || !r.allowsInState(in.Principal, in.Model, st) {
		return denyRouting(out, "model_forbidden", nil)
	}
	boundary := requestBoundary{router: r, in: in, apis: requestAPIs(st)}
	safe := boundary.filter(in.Chain, false)
	if len(rules) == 0 {
		return finishRouting(out, safe)
	}
	inspector := r.requestInspector
	if inspector == nil {
		inspector = sensitivity.NewInspector()
	}
	inspected, err := inspector.Inspect(ctx, in.Protocol, in.RawBody)
	hasPrivacy := false
	for _, rule := range rules {
		hasPrivacy = hasPrivacy || rule.sensitive != nil
	}
	if err != nil {
		out.Decision.Inspection = "failed"
		if hasPrivacy {
			return denyRouting(out, "inspection_failed", nil)
		}
		out.Decision.Reason = "context_inspection_failed"
		return finishRouting(out, safe)
	}
	out.Decision.Inspection = "complete"
	if !inspected.Complete {
		out.Decision.Inspection = "incomplete"
	}
	out.Decision.Categories = boundedCategories(inspected.Categories)
	boundary.inspection = &inspected
	// All triggered privacy rules apply, regardless of document delivery order.
	// A nil model set means unrestricted; a non-nil empty intersection denies.
	block := false
	for _, rule := range rules {
		if rule.sensitive == nil {
			continue
		}
		sd := rule.sensitive
		internal := false
		for _, trigger := range []struct {
			active bool
			action v1alpha1.SensitiveDataAction
		}{
			{len(inspected.Categories) > 0, sd.OnDetected}, {!inspected.Complete, sd.OnUninspectable},
		} {
			if !trigger.active {
				continue
			}
			switch trigger.action {
			case v1alpha1.InternalOnly:
				internal = true
			default:
				block = true // Block, or an invalid injected action, fails closed.
			}
		}
		if internal {
			boundary.intersect(sd.InternalModels)
		}
	}
	if block {
		out.Decision.Privacy = "block"
		return denyRouting(out, "sensitive_blocked", nil)
	}
	if boundary.models != nil {
		out.Decision.Privacy = "internal_only"
		out.Decision.Reason = "internal_only"
	}
	safe = boundary.filter(in.Chain, false)
	if len(safe) == 0 && boundary.models != nil {
		names := make([]string, 0, len(boundary.models))
		for name := range boundary.models {
			names = append(names, name)
		}
		slices.Sort(names)
		for _, name := range names {
			candidate := r.requestChain(boundary, name)
			if len(candidate) > 0 {
				safe = candidate
				break
			}
		}
	}
	out, err = finishRouting(out, safe)
	if err != nil || in.CountOnly {
		return out, err
	}
	return r.applyContext(out, boundary, rules, inspected), nil
}

func denyRouting(out RequestRoutingResult, reason string, cause error) (RequestRoutingResult, error) {
	out.Model = ""
	out.Chain = nil
	out.Decision.SelectedModel = ""
	out.Decision.Reason = reason
	return out, &RequestRoutingError{StatusCode: 403, Reason: reason, cause: cause}
}
func finishRouting(out RequestRoutingResult, chain []ChainTarget) (RequestRoutingResult, error) {
	if len(chain) == 0 {
		return denyRouting(out, "no_safe_route", nil)
	}
	out.Chain = chain
	out.Model = chain[0].Model
	out.Decision.SelectedModel = out.Model
	return out, nil
}

type requestRule struct {
	ref       RoutingPolicyRef
	sensitive *policy.SensitiveData
	context   *policy.Context
}

func requestRules(docs []*policy.Policy, st *live.State, source string) []requestRule {
	var rules []requestRule
	for _, doc := range docs {
		if doc == nil {
			continue
		}
		for _, rule := range doc.Rules {
			ref := RoutingPolicyRef{Name: doc.Name, Generation: doc.Generation, Rule: rule.Name}
			if rule.SensitiveData != nil {
				rules = append(rules, requestRule{ref: ref, sensitive: rule.SensitiveData})
			}
			if rule.Routing == nil || rule.Routing.Context == nil {
				continue
			}
			contextRule := rule.Routing.Context
			for _, from := range contextRule.FromModels {
				if st.Canonical(from) == source {
					rules = append(rules, requestRule{ref: ref, context: contextRule})
					break
				}
			}
		}
	}
	slices.SortFunc(rules, func(a, b requestRule) int {
		if c := cmp.Compare(a.ref.Name, b.ref.Name); c != 0 {
			return c
		}
		if c := cmp.Compare(a.ref.Generation, b.ref.Generation); c != 0 {
			return c
		}
		return cmp.Compare(a.ref.Rule, b.ref.Rule)
	})
	return rules
}

func (r *Router) applyContext(out RequestRoutingResult, boundary requestBoundary, rules []requestRule, inspected sensitivity.Result) RequestRoutingResult {
	mode := "Enforce"
	proposed := ""
	conflict := false
	for _, rule := range rules {
		c := rule.context
		if c == nil {
			continue
		}
		if c.Mode != v1alpha1.Enforce {
			mode = "Shadow"
		}
		model, reason := c.SimpleModel, "simple"
		if inspected.InputTokens > c.MaxSimpleInputTokens {
			model, reason = c.ComplexModel, "input_threshold"
		} else if sensitivity.MatchesKeywords(boundary.in.RawBody, c.ComplexKeywords) {
			model, reason = c.ComplexModel, "complex_keyword"
		}
		model = boundary.in.State.Canonical(model)
		out.Decision.Recommendations = append(out.Decision.Recommendations, RoutingRecommendation{Policy: rule.ref, Model: model, Reason: reason})
		if proposed != "" && model != proposed {
			conflict = true
		}
		proposed = model
	}
	if len(out.Decision.Recommendations) == 0 {
		return out
	}
	out.Decision.Mode = mode
	if conflict {
		out.Decision.Reason = "context_conflict"
		return out
	}
	out.Decision.ProposedModel = proposed
	if mode == "Shadow" {
		out.Decision.Reason = "context_shadow"
		return out
	}
	if !inspected.Complete || inspected.UserTurns != 1 || inspected.HasHistory || inspected.HasTools || inspected.HasVision || inspected.HasReasoning || inspected.HasStructuredOutput {
		out.Decision.Reason = "context_ineligible"
		return out
	}
	if proposed == out.Model {
		out.Decision.Reason = "context_unchanged"
		return out
	}
	candidate := r.requestChain(boundary, proposed)
	// An unavailable recommendation may not silently switch to its fallback.
	if len(candidate) == 0 || candidate[0].Model != proposed {
		out.Decision.Reason = "context_unavailable"
		return out
	}
	out.Model = proposed
	out.Chain = candidate
	out.Decision.SelectedModel = proposed
	out.Decision.Reason = "context_selected"
	return out
}

// allowsInState mirrors Allows without loading a new topology generation.
func (r *Router) allowsInState(p keystore.Principal, model string, st *live.State) bool {
	allowed := p.Allows(model)
	for _, name := range p.AllowedModels {
		if st.Canonical(name) == model {
			allowed = true
			break
		}
	}
	return allowed && (r.policyGate == nil || r.policyGate(p, model, st.Canonical))
}

type requestBoundary struct {
	router     *Router
	in         RequestRoutingInput
	inspection *sensitivity.Result
	models     map[string]bool
	apis       map[requestTargetKey]string
}

type requestTargetKey struct{ provider, upstream string }

// BuildState collects nonempty Bedrock API overrides by provider/upstream from
// every model route. An empty annotation on this candidate does not cancel an
// override elsewhere. Conflicting overrides have no deterministic wire contract.
func requestAPIs(st *live.State) map[requestTargetKey]string {
	apis := make(map[requestTargetKey]string)
	for _, mc := range st.Models() {
		for _, target := range mc.Targets {
			if target.API == "" {
				continue
			}
			key := requestTargetKey{target.Provider, target.Model}
			if previous, ok := apis[key]; ok && previous != target.API {
				apis[key] = "conflicting"
			} else {
				apis[key] = target.API
			}
		}
	}
	return apis
}

func (b *requestBoundary) intersect(names []string) {
	set := make(map[string]bool, len(names))
	for _, name := range names {
		set[b.in.State.Canonical(name)] = true
	}
	if b.models == nil {
		b.models = set
		return
	}
	for name := range b.models {
		if !set[name] {
			delete(b.models, name)
		}
	}
}

// filter checks EVERY entry, including the first one: ingress may have promoted
// a fallback before this call. Only the explicitly selected model retains its
// existing metadata/context behavior; all automatically selected models require
// declared context, required capabilities and pricing on this exact generation.
func (b requestBoundary) filter(chain []ChainTarget, automatic bool) []ChainTarget {
	st := b.in.State
	var out []ChainTarget
	for _, ct := range chain {
		ct.Model = st.Canonical(ct.Model)
		if !b.router.allowsInState(b.in.Principal, ct.Model, st) {
			continue
		}
		mc, ok := st.Route(ct.Model)
		if !ok {
			continue
		}
		// Validate the target against the supplied topology, not caller annotations.
		var target config.Target
		found := false
		for _, t := range mc.Targets {
			if t.Provider == ct.ProviderName && t.Model == ct.Upstream {
				target = t
				found = true
				break
			}
		}
		if !found {
			continue
		}
		p, ok := st.Provider(ct.ProviderName)
		if !ok || p == nil {
			continue
		}
		ct.Provider = p
		ct.Identity, _ = st.Identity(ct.ProviderName)
		ct.Region = st.Region(ct.ProviderName)
		ct.DataBoundary = st.DataBoundary(ct.ProviderName)
		if api, ok := b.apis[requestTargetKey{ct.ProviderName, ct.Upstream}]; ok {
			target.API = api
		}
		if len(b.in.AllowedRegions) > 0 && (ct.Region == "" || !slices.Contains(b.in.AllowedRegions, ct.Region)) {
			continue
		}
		if b.models != nil && (!b.models[ct.Model] || ct.DataBoundary != "internal") {
			continue
		}
		if !requestCompatible(b.in.Protocol, p, target, b.inspection) {
			continue
		}
		if b.inspection != nil && (automatic || ct.Model != b.in.Model) {
			if !fitsRequest(mc, *b.inspection) || st.Pricing() == nil || !st.Pricing().HasRate(ct.ProviderName, ct.Upstream) {
				continue
			}
		}
		out = append(out, ct)
	}
	return out
}
func fitsRequest(mc config.ModelConfig, s sensitivity.Result) bool {
	// Subtract after range checking: input+output can overflow int64.
	if s.InputTokens < 0 || s.OutputTokens < 0 || mc.ContextWindow <= 0 || s.InputTokens > mc.ContextWindow || s.OutputTokens > mc.ContextWindow-s.InputTokens {
		return false
	}
	for _, cap := range []struct {
		required bool
		name     string
	}{{s.HasTools, "tools"}, {s.HasVision, "vision"}, {s.HasReasoning, "reasoning"}, {s.HasStructuredOutput, "structured_output"}} {
		if cap.required && !slices.Contains(mc.Capabilities, cap.name) {
			return false
		}
	}
	return true
}

// requestCompatible reflects the physical implementations, not model capability
// declarations. Bedrock InvokeModel accepts Anthropic-shaped bytes; Converse
// drops image/reasoning blocks. Cross-wire canonical conversion cannot preserve
// vision/reasoning/structured output. Unproven paths stay conservative.
func requestCompatible(ingress string, provider providers.Provider, target config.Target, s *sensitivity.Result) bool {
	if ingress != "anthropic" && ingress != "openai" && ingress != "bedrock" {
		return false
	}
	feature := s != nil && (!s.Complete || s.HasVision || s.HasReasoning || s.HasStructuredOutput)
	declared, hasDeclaration := provider.(IngressSupporter)
	if hasDeclaration && !declared.SupportsIngress(ingress) {
		return false
	}
	switch provider.Name() {
	case "anthropic":
		return ingress == "anthropic"
	case "openai_compatible":
		return ingress == "openai" || !feature
	case "bedrock":
		api := target.API
		if api == "" {
			api = "converse"
			if strings.Contains(target.Model, "anthropic.") || strings.Contains(target.Model, "claude") {
				api = "invoke_model"
			}
		}
		switch api {
		case "invoke_model":
			return ingress != "openai"
		case "converse":
			// Converse consumes the raw Anthropic-shaped tools, not Parsed.
			return !feature && !(ingress == "openai" && s != nil && s.HasTools)
		case "mantle":
			return !feature
		default:
			return false
		}
	case "mock":
		return true
	default:
		if hasDeclaration {
			return true
		}
		// Preserve existing extensible providers for Anthropic/OpenAI text ingress,
		// but never infer native Bedrock or feature-preserving translation support.
		return ingress != "bedrock" && !feature
	}
}

// requestChain applies ResolveChain's priority/breaker semantics AFTER filtering
// on the supplied state. An unsafe, healthy provider cannot suppress the
// all-open recovery of safe providers. Never call ResolveChain/Allows here:
// both load live and could change the topology/pricing generation.
func (r *Router) requestChain(boundary requestBoundary, model string) []ChainTarget {
	st := boundary.in.State
	mc, ok := st.Route(model)
	if !ok || len(mc.Targets) == 0 {
		return nil
	}
	models := []string{model}
	if fb := st.FallbackFor(model); fb != "" && fb != model {
		models = append(models, st.Canonical(fb))
	}
	var all, allowed []ChainTarget
	for _, name := range models {
		mc, ok := st.Route(name)
		if !ok {
			continue
		}
		for _, t := range mc.Targets {
			p, ok := st.Provider(t.Provider)
			if !ok {
				continue
			}
			id, _ := st.Identity(t.Provider)
			ct := ChainTarget{Provider: p, ProviderName: t.Provider, Identity: id, Upstream: t.Model, Model: name, Region: st.Region(t.Provider), DataBoundary: st.DataBoundary(t.Provider)}
			all = append(all, ct)
		}
	}
	all = boundary.filter(all, true)
	for _, ct := range all {
		if r.brk.Allow(ct.Identity) {
			allowed = append(allowed, ct)
		}
	}
	if len(allowed) > 0 {
		return allowed
	}
	return all
}

func boundedCategories(categories []string) []string {
	var out []string
	for _, c := range categories {
		switch c {
		case "email", "phone", "credit_card", "ssn", "ipv4", "korean_rrn":
			out = append(out, c)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}
