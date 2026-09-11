package router

import (
	"bytes"
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
	// SessionHint is untrusted, scoped to authenticated identity and hashed
	// locally. It never authorizes access or appears in decision evidence.
	SessionHint string
	// Redactor optionally accumulates an ingress's separately configured
	// legacy filter with policy redaction. It cannot skip reinspection.
	Redactor RequestRedactor `json:"-"`
	// Compatible is an optional request-specific transport check supplied by
	// ingress. It only narrows the built-in checks, including every fallback
	// and affinity hit. Responses canonical adapters require this approval.
	Compatible func(ChainTarget) bool
}

// RequestRoutingResult owns its chain and decision slices. State is exactly the
// supplied immutable generation: use it for attempts, context checks and pricing.
// Without privacy or an applied context selection, Model retains the input's
// preflight/request-model meaning, even if region filtering promoted a fallback.
// Decision.SelectedModel names the first planned target; attempts use ct.Model.
// On error Model/Chain/Decision.SelectedModel are empty; Decision describes denial.
type RequestRoutingResult struct {
	Model    string
	Chain    []ChainTarget
	State    *live.State
	Decision RoutingDecision
	// MaskRequired is an obligation; SanitizedBody is populated only after
	// successful redaction and independent complete/zero-signal reinspection.
	// Callers must use SanitizedBody for forwarding AND regenerate Parsed.
	MaskRequired  bool
	SanitizedBody []byte         `json:"-"`
	AffinityToken *AffinityToken `json:"-"`
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
	Masked          bool                    `json:"masked,omitempty"`
}

// RequestRedactor transforms a complete ingress body locally. Errors and
// incomplete/unmasked output refuse the entire request. Implementations need
// not import router: this is a structural contract.
type RequestRedactor interface {
	Redact(context.Context, string, []byte) ([]byte, error)
}

// RequestRoutingError is a security refusal (StatusCode 403). Ingresses translate
// it into their own error shape. Count-only endpoints instead estimate locally
// with HTTP 200 and no upstream call. Error/Unwrap never expose dependency text;
// only policy.ErrRoutingPolicyRejected (including its legacy alias) is
// preserved as an optional cause.
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
	out := RequestRoutingResult{Model: in.Model, State: in.State, Decision: RoutingDecision{RequestedModel: requested, Reason: "unchanged", Inspection: "not_inspected"}}
	// Lookup errors must be checked even for count-only/otherwise ineligible
	// requests. Never expose arbitrary dependency error text in a routing error.
	var docs []*policy.Policy
	if r.routingPolicies != nil {
		var err error
		docs, err = r.routingPolicies(in.Principal.Team, in.Principal.Owner)
		if err != nil {
			var cause error
			if errors.Is(err, policy.ErrRoutingPolicyRejected) {
				cause = policy.ErrRoutingPolicyRejected
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
	hasPrivacy := false
	for _, rule := range rules {
		out.Decision.Policies = append(out.Decision.Policies, rule.ref)
		hasPrivacy = hasPrivacy || rule.sensitive != nil
	}
	if !r.allowsInState(in.Principal, st.Canonical(requested), st) || !r.allowsInState(in.Principal, in.Model, st) {
		return denyRouting(out, "model_forbidden", nil)
	}
	budgetModel, strictBudget := r.budgetConstraint(in, requested)
	if strictBudget && budgetModel == "" {
		return denyRouting(out, "budget_target_unavailable", nil)
	}
	inspector := r.requestInspector
	if inspector == nil {
		inspector = sensitivity.NewInspector()
	}
	// Responses has no pre-existing permissive legacy transport path. Inspect
	// once even without policy so canonical adapters can never infer that an
	// opaque native request is portable. Native providers may retain opaque
	// content; malformed inspection still refuses.
	var responsesInspection *sensitivity.Result
	if in.Protocol == "responses" {
		result, err := inspector.Inspect(ctx, in.Protocol, in.RawBody)
		if err != nil {
			out.Decision.Inspection = "failed"
			return denyRouting(out, "inspection_failed", nil)
		}
		responsesInspection = &result
		out.Decision.Inspection = "complete"
		if !result.Complete {
			out.Decision.Inspection = "incomplete"
		}
		out.Decision.Categories = boundedCategories(result.Categories)
	}
	if !hasPrivacy && !strictBudget {
		// Optional context observes this complete authorized legacy chain.
		// New compatibility/metadata gates apply only if a new chain is
		// selected. Preserve the input preflight model independently of
		// the first target (an ingress filter may have promoted a fallback).
		if responsesInspection != nil {
			transport := requestBoundary{router: r, in: in, apis: requestAPIs(st), inspection: responsesInspection}
			out.Chain = transport.filter(in.Chain, false)
		} else {
			out.Chain = r.legacyChain(in)
		}
		if len(out.Chain) == 0 {
			return denyRouting(out, "no_safe_route", nil)
		}
		out.Decision.SelectedModel = out.Chain[0].Model
		if len(rules) == 0 {
			return out, nil
		}
	}
	boundary := requestBoundary{router: r, in: in, apis: requestAPIs(st), budgetModel: budgetModel}
	var inspected sensitivity.Result
	var err error
	if responsesInspection != nil {
		inspected = *responsesInspection
	} else {
		inspected, err = inspector.Inspect(ctx, in.Protocol, in.RawBody)
	}
	if err != nil {
		out.Decision.Inspection = "failed"
		if hasPrivacy || strictBudget {
			return denyRouting(out, "inspection_failed", nil)
		}
		out.Decision.Reason = "context_inspection_failed"
		return out, nil
	}
	out.Decision.Inspection = "complete"
	if !inspected.Complete {
		out.Decision.Inspection = "incomplete"
	}
	out.Decision.Categories = boundedCategories(inspected.Categories)
	boundary.inspection = &inspected
	boundary.converseSafe = conversePreservesRequest(in.RawBody)
	if !hasPrivacy && !strictBudget {
		if in.CountOnly {
			return out, nil
		}
		return r.applyContext(out, boundary, rules, inspected), nil
	}
	// All triggered privacy rules apply, regardless of document delivery order.
	// A nil model set means unrestricted; a non-nil empty intersection denies.
	block := false
	for _, rule := range rules {
		if rule.sensitive == nil {
			continue
		}
		sd := rule.sensitive
		internal := false
		for index, trigger := range []struct {
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
			case v1alpha1.Mask:
				if index == 0 {
					out.MaskRequired = true
				} else {
					block = true
				}
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
	if out.MaskRequired {
		redactor := r.requestRedactor
		if in.Redactor != nil {
			redactor = in.Redactor
		}
		if redactor == nil {
			return denyRouting(out, "mask_failed", nil)
		}
		sanitized, maskErr := redactor.Redact(ctx, in.Protocol, bytes.Clone(in.RawBody))
		if maskErr != nil || ctx.Err() != nil {
			return denyRouting(out, "mask_failed", nil)
		}
		// Keep the original categories and obligations for audit and routing.
		// Only the final body supplies compatibility and context-size signals.
		checked, inspectErr := inspector.Inspect(ctx, in.Protocol, sanitized)
		if inspectErr != nil || !checked.Complete || len(checked.Categories) != 0 || ctx.Err() != nil {
			return denyRouting(out, "mask_failed", nil)
		}
		out.SanitizedBody = bytes.Clone(sanitized)
		out.Decision.Masked = true
		boundary.in.RawBody = out.SanitizedBody
		boundary.inspection = &checked
		boundary.converseSafe = conversePreservesRequest(out.SanitizedBody)
		inspected = checked
	}
	if boundary.models != nil {
		out.Decision.Privacy = "internal_only"
		out.Decision.Reason = "internal_only"
	}
	safe := boundary.filter(in.Chain, false)
	if strictBudget {
		safe = r.requestChain(boundary, budgetModel)
		if len(safe) == 0 {
			return denyRouting(out, "budget_target_unavailable", nil)
		}
		if boundary.models == nil {
			out.Decision.Reason = "budget_target"
		}
	} else if len(safe) == 0 && boundary.models != nil {
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

// legacyChain owns its returned slice and retains existing attempt order,
// including provider retries and cross-model fallbacks. It enforces the
// established RBAC/region constraints without opting passive traffic into new
// transport, context, capability or pricing requirements. It never rebuilds
// an empty input or consults a different topology generation.
func (r *Router) legacyChain(in RequestRoutingInput) []ChainTarget {
	safe := make([]ChainTarget, 0, len(in.Chain))
	for _, ct := range in.Chain {
		if !r.allowsInState(in.Principal, in.State.Canonical(ct.Model), in.State) {
			continue
		}
		region := in.State.Region(ct.ProviderName)
		if len(in.AllowedRegions) > 0 && (region == "" || !slices.Contains(in.AllowedRegions, region)) {
			continue
		}
		if in.Compatible != nil && !in.Compatible(ct) {
			continue
		}
		safe = append(safe, ct)
	}
	return safe
}

func denyRouting(out RequestRoutingResult, reason string, cause error) (RequestRoutingResult, error) {
	out.Model = ""
	out.Chain = nil
	out.SanitizedBody = nil
	out.AffinityToken = nil
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
	stable := true
	for _, rule := range rules {
		c := rule.context
		if c == nil {
			continue
		}
		stable = stable && c.Stability != nil
		if c.Mode != v1alpha1.Enforce {
			mode = "Shadow"
		}
		model, reason := c.SimpleModel, "simple"
		if c.NormalModel != "" && contextKeywordMatch(c, boundary.in) {
			model, reason = c.ComplexModel, "complex_keyword"
		} else if inspected.InputTokens > c.MaxSimpleInputTokens {
			model, reason = c.ComplexModel, "input_threshold"
			if c.NormalModel != "" && inspected.InputTokens <= c.MaxNormalInputTokens {
				model, reason = c.NormalModel, "normal_input"
			}
		} else if c.NormalModel == "" && contextKeywordMatch(c, boundary.in) {
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
	settings := stabilityOf(rules)
	stable = stable && settings != nil
	token, pinned := r.prepareAffinity(out, boundary, rules, settings)
	if len(pinned) > 0 {
		out.Model, out.Chain = pinned[0].Model, pinned
		out.Decision.SelectedModel = out.Model
		out.Decision.Reason = "context_affinity"
		return finishAffinity(out, token, boundary)
	}
	if !inspected.Complete || inspected.UserTurns < 1 ||
		(!stable && (inspected.UserTurns != 1 || inspected.HasHistory || inspected.HasTools || inspected.HasVision || inspected.HasReasoning || inspected.HasStructuredOutput)) {
		out.Decision.Reason = "context_ineligible"
		return out
	}
	if proposed == boundary.in.State.Canonical(out.Model) {
		out.Decision.Reason = "context_unchanged"
		return finishAffinity(out, token, boundary)
	}
	// Context optimization is optional. If the recommendation's leading path
	// cannot preserve this request, retain the already-safe route instead of
	// promoting another provider to make that recommendation appear available.
	// Privacy recovery is independent and can use any approved safe attempt.
	mc, ok := boundary.in.State.Route(proposed)
	if !ok || len(mc.Targets) == 0 {
		out.Decision.Reason = "context_unavailable"
		return finishAffinity(out, token, boundary)
	}
	first := mc.Targets[0]
	if api, ok := boundary.apis[requestTargetKey{first.Provider, first.Model}]; ok {
		first.API = api
	}
	provider, ok := boundary.in.State.Provider(first.Provider)
	identity, _ := boundary.in.State.Identity(first.Provider)
	leading := ChainTarget{Provider: provider, ProviderName: first.Provider, Identity: identity, Model: proposed, Upstream: first.Model,
		Region: boundary.in.State.Region(first.Provider), DataBoundary: boundary.in.State.DataBoundary(first.Provider)}
	if !ok || provider == nil || !boundary.compatible(leading, first) {
		out.Decision.Reason = "context_unavailable"
		return finishAffinity(out, token, boundary)
	}
	candidate := r.requestChain(boundary, proposed)
	// An unavailable recommendation may not silently switch to its fallback.
	if len(candidate) == 0 || candidate[0].Model != proposed {
		out.Decision.Reason = "context_unavailable"
		return finishAffinity(out, token, boundary)
	}
	out.Model = proposed
	out.Chain = candidate
	out.Decision.SelectedModel = proposed
	out.Decision.Reason = "context_selected"
	return finishAffinity(out, token, boundary)
}

// Extended context rules classify the latest actual user instruction. Legacy
// rules retain their original whole-document keyword semantics. InputTokens and
// fitsRequest still account for the complete request, including tools/history.
func contextKeywordMatch(c *policy.Context, in RequestRoutingInput) bool {
	if c.NormalModel != "" || c.Stability != nil {
		return sensitivity.MatchesContextKeywords(in.RawBody, in.Protocol, c.ComplexKeywords)
	}
	return sensitivity.MatchesKeywords(in.RawBody, c.ComplexKeywords)
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
	router       *Router
	in           RequestRoutingInput
	inspection   *sensitivity.Result
	models       map[string]bool
	apis         map[requestTargetKey]string
	converseSafe bool
	budgetModel  string
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
	// An internal model label covers the model destination, not additional
	// egress performed by client-selected hosted/MCP tools. An InternalOnly
	// obligation cannot approve those secondary destinations. Opaque images
	// and other content without remote-tool capability keep their existing
	// policy behavior.
	if b.models != nil && b.inspection != nil && b.inspection.HasRemoteTools {
		return nil
	}
	st := b.in.State
	var out []ChainTarget
	for _, ct := range chain {
		ct.Model = st.Canonical(ct.Model)
		if b.budgetModel != "" && ct.Model != b.budgetModel {
			continue
		}
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
		if !b.compatible(ct, target) {
			continue
		}
		if b.inspection != nil && (automatic || ct.Model != b.in.Model || b.budgetModel != "" ||
			(b.in.Protocol == "responses" && p.Name() != "openai_responses")) {
			if !fitsRequest(mc, *b.inspection) || st.Pricing() == nil || !st.Pricing().HasRate(ct.ProviderName, ct.Upstream) {
				continue
			}
		}
		out = append(out, ct)
	}
	return out
}

func (b requestBoundary) compatible(ct ChainTarget, target config.Target) bool {
	if !requestCompatible(b.in.Protocol, ct.Provider, target, b.inspection, b.converseSafe) {
		return false
	}
	if b.in.Protocol == "responses" && ct.Provider.Name() != "openai_responses" && b.in.Compatible == nil {
		return false
	}
	return b.in.Compatible == nil || b.in.Compatible(ct)
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
func requestCompatible(ingress string, provider providers.Provider, target config.Target, s *sensitivity.Result, converseSafe bool) bool {
	declared, hasDeclaration := provider.(IngressSupporter)
	if hasDeclaration && !declared.SupportsIngress(ingress) {
		return false
	}
	if ingress == "responses" {
		if provider.Name() == "openai_responses" {
			return true
		}
		if provider.Name() == "bedrock" || s == nil || !s.Complete || s.HasVision || s.HasReasoning || s.HasStructuredOutput {
			return false
		}
		// Known canonical text/tool paths and explicit provider contracts can
		// bridge Responses only with the ingress callback's additional approval.
		return provider.Name() == "anthropic" || provider.Name() == "openai_compatible" || hasDeclaration
	}
	if provider.Name() == "openai_responses" {
		return false
	}
	if ingress != "anthropic" && ingress != "openai" && ingress != "bedrock" {
		return false
	}
	feature := s != nil && (!s.Complete || s.HasVision || s.HasReasoning || s.HasStructuredOutput)
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
			return !feature && !(ingress == "openai" && s != nil && s.HasTools) &&
				(s == nil || converseSafe)
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

func (r *Router) budgetConstraint(in RequestRoutingInput, requested string) (string, bool) {
	if r.budgetConstraintGate == nil {
		return "", false
	}
	source := in.State.Canonical(requested)
	target, active := "", false
	for from, to := range r.budgetConstraintGate(in.Principal) {
		if in.State.Canonical(from) != source {
			continue
		}
		to = in.State.Canonical(to)
		if to == "" || (active && to != target) {
			return "", true
		}
		target, active = to, true
	}
	return target, active
}

// BudgetTargetAllowed rechecks only the current mandatory cost ceiling before
// a later attempt. It cannot broaden the request's already-filtered chain.
func (r *Router) BudgetTargetAllowed(p keystore.Principal, requested, candidate string, st *live.State) bool {
	if st == nil {
		return false
	}
	target, strict := r.budgetConstraint(RequestRoutingInput{Principal: p, State: st}, requested)
	return !strict || target != "" && target == st.Canonical(candidate)
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
	// Rebuilding the explicitly selected model after its safe breakers opened
	// is recovery, not model substitution. Keep its metadata exemption; filter
	// still requires metadata for every other model appended to that chain.
	all = boundary.filter(all, model != boundary.in.Model)
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
