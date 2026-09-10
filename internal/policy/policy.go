// Package policy is the single shared truth for governance rules and budget
// leases between the control plane (cmd/inferplaned) and the data plane
// (cmd/mayu). Both binaries compile against this package, so a schema change
// that only one side understands is caught at compile time instead of
// surfacing as silent version skew in production (ADR-031).
//
// The wire form lives in api/v1alpha1; this package owns conversion into the
// internal form and — critically — explicit rejection: a document the data
// plane does not fully support is refused with an *UnsupportedError meant to
// be reported back to the control plane, never silently dropped.
//
// Unit boundary (ADR-032): the wire speaks integer milliUSD (1000 = $1, the
// operator-facing resolution); this package converts to integer microUSD
// (×1000, exact) because internal cost accounting settles per-token amounts
// that are sub-milliUSD — settling coarser re-opens the ADR-030 zero-cost
// bug class.
package policy

import (
	"fmt"
	"math"
	"strings"
	"time"

	v1alpha1 "github.com/inferplane/inferplane/api/v1alpha1"
)

// Cadence decisions (ADR-032). Cost control is near-real-time: worst-case
// budget overshoot is bounded by lease grant × connected data planes, so the
// lease defaults keep grants small and renewals frequent. Policy delivery is
// push-based whenever the control-plane stream is up; the poll interval is
// only the reconcile fallback for disconnected proxies.
const (
	// DefaultLeaseRenewInterval is how often a data plane reports
	// consumption and renews its budget lease when the rule doesn't say.
	DefaultLeaseRenewInterval = 10 * time.Second
	// MinLeaseRenewInterval is the floor for an explicit renew interval.
	MinLeaseRenewInterval = time.Second
	// DefaultLeaseGrantBP is the default lease grant as basis points of
	// the budget limit: 10 bp = 0.1%.
	DefaultLeaseGrantBP = 10
	// DefaultPolicySyncInterval is the reconcile-poll default for policy
	// distribution when the push stream is down.
	DefaultPolicySyncInterval = time.Minute
	// MinPolicySyncInterval is the floor an operator may set the
	// reconcile poll to.
	MinPolicySyncInterval = 15 * time.Second
)

// SupportedAPIVersions lists the config API generations this build
// understands. The control plane exposes the version distribution of
// connected data planes so an operator can check coverage before
// propagating a rule that needs a newer generation.
var SupportedAPIVersions = []string{v1alpha1.APIVersion}

// Supports reports whether this build understands apiVersion.
func Supports(apiVersion string) bool {
	for _, v := range SupportedAPIVersions {
		if v == apiVersion {
			return true
		}
	}
	return false
}

// UnsupportedError is the explicit rejection of a policy document (or one
// rule in it). The data plane MUST surface it to the control plane; silent
// ignoring is the worst failure mode a governance tool can have.
type UnsupportedError struct {
	APIVersion string // apiVersion of the offending document
	Kind       string // kind of the offending document
	Rule       string // rule name, empty when the whole document is rejected
	Reason     string // human-readable reason, safe to report upstream
}

func (e *UnsupportedError) Error() string {
	if e.Rule == "" {
		return fmt.Sprintf("unsupported policy document %s/%s: %s", e.APIVersion, e.Kind, e.Reason)
	}
	return fmt.Sprintf("unsupported rule %q in %s/%s: %s", e.Rule, e.APIVersion, e.Kind, e.Reason)
}

// Policy is the internal, version-independent form of one policy document.
type Policy struct {
	Name       string
	Generation int64
	Subject    Subject
	Rules      []Rule
}

// Subject is who a policy governs: a team (department) and/or a user. User-
// and team-level governance are equal citizens; when several policies match
// one request, the most restrictive outcome wins (block beats warn).
type Subject struct {
	Team string
	User string
}

// Rule is the internal form of one governance rule.
type Rule struct {
	Name          string
	FailurePolicy v1alpha1.FailurePolicy
	Budget        *Budget
	Routing       *Routing
	ModelAccess   *ModelAccess
	SensitiveData *SensitiveData
	Rate          *Rate
}

// Budget is the internal form of a budget rule. All amounts are integer
// microUSD (converted ×1000 from the wire's milliUSD, defaults applied).
// Unlimited mirrors the wire field: when true, LimitMicroUSD is always 0
// (the pre-existing "no cap" sentinel every consumer already understands)
// and every other field is zero-value — the flag exists purely so a policy
// diff/audit can distinguish "explicitly declared unlimited" from
// "no budget rule was ever written for this dimension".
type Budget struct {
	Unlimited          bool
	LimitMicroUSD      int64
	HardCap            bool
	LeaseGrantMicroUSD int64
	LeaseRenewInterval time.Duration
	AdminContact       string
	// Period is the calendar window this rule's limit applies to. NEVER empty
	// after conversion: an empty wire value is normalized to
	// v1alpha1.PeriodCalendarMonth here, so no consumer has to re-implement
	// the default (and none may treat "" as a third window).
	Period v1alpha1.BudgetPeriod
}

// ModelAccess is the internal form of a model allow-list rule. Entries match
// after alias canonicalization; "*" allows every configured model.
type ModelAccess struct {
	Allow []string
}

// Rate is the internal form of a throughput rule. 0 = unlimited dimension
// (both when Unlimited is explicitly set and, pre-existing, whenever a
// dimension simply wasn't given a limit).
type Rate struct {
	Unlimited bool
	RPM       int64
	TPM       int64
}

// Routing is the internal form of a routing rule: exactly one of Affinity,
// BudgetTiers, or Context is set.
type Routing struct {
	Affinity    *Affinity
	BudgetTiers *BudgetTiers
	Context     *Context
}

// Affinity is the internal form of the cache-affinity half of a routing
// rule.
type Affinity struct {
	OnAffinityConflict v1alpha1.ConflictPreference
}

// BudgetTiers is the internal form of the ADR-041 budget-tier substitution
// half of a routing rule.
type BudgetTiers struct {
	BudgetRef string
	Tiers     []BudgetTier
}

// BudgetTier is the internal form of one tier: at ThresholdPercent
// utilization of BudgetRef, Substitute takes effect.
type BudgetTier struct {
	ThresholdPercent int
	Substitute       map[string]string
}

// Lease is one budget grant issued by the control plane to one data plane.
// Within GrantMicroUSD and until ExpiresAt the data plane enforces locally
// with no network round trip; consumption is reported and the lease renewed
// asynchronously. Lease STATE (remaining grant, expiry, counters) persists
// in the data plane's SQLite metadata store across restarts — prompt/response
// payloads never do (they live in the VolatileStore, see internal/cache).
type Lease struct {
	Rule          string
	GrantMicroUSD int64
	ExpiresAt     time.Time
}

// FromV1Alpha1 converts a wire document to the internal form, rejecting
// anything this build does not fully support. It validates the invariants
// the design fixes:
//
//   - the subject must select a team and/or a user;
//   - spec.rules must contain at least one rule, matching the CRD;
//   - failurePolicy is required per rule — no silent defaults;
//   - a hard-cap budget rule must be FailClosed (soft budgets fail open);
//   - a budget rule's period is CalendarDay or CalendarMonth (empty defaults to
//     CalendarMonth, the window every rule enforced before the field existed);
//   - lease grant / renew interval take the ADR-032 defaults when unset,
//     but an explicit sub-floor renew interval is rejected;
//   - a routing rule sets exactly one of onAffinityConflict (affinity vs
//     fallback preference, required for that half) or budgetTiers (ADR-041:
//     budgetRef names a numeric-limit budget rule in the same document,
//     thresholds strictly increase, substitution maps are well-formed);
//   - exactly one rule kind per rule.
func FromV1Alpha1(doc *v1alpha1.GovernancePolicy) (*Policy, error) {
	reject := func(rule, reason string) *UnsupportedError {
		return &UnsupportedError{APIVersion: doc.APIVersion, Kind: doc.Kind, Rule: rule, Reason: reason}
	}
	if !Supports(doc.APIVersion) {
		return nil, reject("", fmt.Sprintf("apiVersion not supported by this build (supported: %v)", SupportedAPIVersions))
	}
	if doc.Kind != v1alpha1.KindGovernancePolicy {
		return nil, reject("", fmt.Sprintf("unknown kind %q", doc.Kind))
	}
	if doc.Spec.Subject.Team == "" && doc.Spec.Subject.User == "" {
		return nil, reject("", "spec.subject must select a team and/or a user")
	}
	if len(doc.Spec.Rules) == 0 {
		return nil, reject("", "spec.rules must contain at least one rule")
	}

	p := &Policy{
		Name:       doc.Metadata.Name,
		Generation: doc.Metadata.Generation,
		Subject:    Subject{Team: doc.Spec.Subject.Team, User: doc.Spec.Subject.User},
		Rules:      make([]Rule, 0, len(doc.Spec.Rules)),
	}
	ruleNames := make(map[string]bool, len(doc.Spec.Rules))
	for _, wr := range doc.Spec.Rules {
		if wr.Name == "" {
			return nil, reject("", "rule with empty name")
		}
		if ruleNames[wr.Name] {
			// Rule names key leases and consumption reports later — a
			// duplicate would silently alias two rules' state.
			return nil, reject(wr.Name, "duplicate rule name within one policy")
		}
		ruleNames[wr.Name] = true
		switch wr.FailurePolicy {
		case v1alpha1.FailOpen, v1alpha1.FailClosed:
		case "":
			return nil, reject(wr.Name, "failurePolicy is required; a defaulted failure mode is a silent one")
		default:
			return nil, reject(wr.Name, fmt.Sprintf("unknown failurePolicy %q", wr.FailurePolicy))
		}
		kinds := 0
		for _, set := range []bool{wr.Budget != nil, wr.Routing != nil, wr.ModelAccess != nil, wr.Rate != nil, wr.SensitiveData != nil} {
			if set {
				kinds++
			}
		}
		if kinds != 1 {
			return nil, reject(wr.Name, "exactly one of budget, routing, modelAccess, rate, or sensitiveData must be set")
		}

		r := Rule{Name: wr.Name, FailurePolicy: wr.FailurePolicy}
		switch {
		case wr.SensitiveData != nil:
			sd, err := sensitiveDataFromV1Alpha1(wr, reject)
			if err != nil {
				return nil, err
			}
			r.SensitiveData = sd
		case wr.Budget != nil:
			b, err := budgetFromV1Alpha1(wr, reject)
			if err != nil {
				return nil, err
			}
			r.Budget = b
		case wr.Routing != nil:
			rt, err := routingFromV1Alpha1(wr, doc, reject)
			if err != nil {
				return nil, err
			}
			r.Routing = rt
		case wr.ModelAccess != nil:
			if len(wr.ModelAccess.Allow) == 0 {
				return nil, reject(wr.Name, `modelAccess.allow must be non-empty (use ["*"] for all): an empty list is deny-all and must be written deliberately`)
			}
			for _, m := range wr.ModelAccess.Allow {
				if m == "" {
					return nil, reject(wr.Name, "modelAccess.allow contains an empty model name")
				}
			}
			r.ModelAccess = &ModelAccess{Allow: append([]string(nil), wr.ModelAccess.Allow...)}
		case wr.Rate != nil:
			if wr.Rate.RPM < 0 || wr.Rate.TPM < 0 {
				return nil, reject(wr.Name, "rate.rpm and rate.tpm must be >= 0")
			}
			if wr.Rate.Unlimited {
				if wr.Rate.RPM != 0 || wr.Rate.TPM != 0 {
					return nil, reject(wr.Name, "rate.unlimited must not be combined with rpm or tpm")
				}
				r.Rate = &Rate{Unlimited: true}
				break
			}
			if wr.Rate.RPM == 0 && wr.Rate.TPM == 0 {
				return nil, reject(wr.Name, "rate rule must limit at least one of rpm or tpm (or set unlimited: true to declare no cap deliberately)")
			}
			r.Rate = &Rate{RPM: wr.Rate.RPM, TPM: wr.Rate.TPM}
		}
		p.Rules = append(p.Rules, r)
	}
	return p, nil
}

// microPerMilli converts wire milliUSD to internal microUSD (exact).
const microPerMilli = 1000

// maxWireMilliUSD is the largest milliUSD amount whose µUSD conversion still
// fits in int64. Anything larger would overflow into a negative internal
// limit — a nonsense figure ($9.2 quadrillion) that can only be a typo, so
// it is rejected rather than clamped.
const maxWireMilliUSD = math.MaxInt64 / microPerMilli

func budgetFromV1Alpha1(wr v1alpha1.Rule, reject func(rule, reason string) *UnsupportedError) (*Budget, error) {
	wb := wr.Budget
	if wb.Unlimited {
		if wb.LimitMilliUSD != 0 || wb.HardCap || wb.Lease != (v1alpha1.LeaseSpec{}) || wb.AdminContact != "" || wb.Period != "" {
			return nil, reject(wr.Name, "budget.unlimited must not be combined with limitMilliUSD, hardCap, lease, adminContact, or period")
		}
		// Period is normalized even here. An unlimited rule has no window to
		// speak of, but leaving it "" would make the struct's "never empty
		// after conversion" contract false and push a default into every
		// consumer; the value is simply never read for an unlimited rule.
		return &Budget{Unlimited: true, Period: v1alpha1.PeriodCalendarMonth}, nil
	}
	if wb.LimitMilliUSD <= 0 || wb.LimitMilliUSD > maxWireMilliUSD {
		return nil, reject(wr.Name, fmt.Sprintf("budget.limitMilliUSD must be in (0, %d] (1000 = $1) (or set unlimited: true to declare no cap deliberately)", int64(maxWireMilliUSD)))
	}
	if wb.HardCap && wr.FailurePolicy != v1alpha1.FailClosed {
		return nil, reject(wr.Name, "a hard-cap budget rule must be FailClosed: fail-open on lease expiry voids the cap")
	}

	period := wb.Period
	switch period {
	case v1alpha1.PeriodCalendarDay, v1alpha1.PeriodCalendarMonth:
	case "":
		// Empty is the pre-existing meaning, not a missing decision: every
		// budget rule written before this field existed capped the month.
		period = v1alpha1.PeriodCalendarMonth
	default:
		return nil, reject(wr.Name, fmt.Sprintf("unknown budget.period %q (supported: %q, %q; empty means %q)", period, v1alpha1.PeriodCalendarDay, v1alpha1.PeriodCalendarMonth, v1alpha1.PeriodCalendarMonth))
	}

	grantMilli := wb.Lease.GrantMilliUSD
	switch {
	case grantMilli < 0 || grantMilli > maxWireMilliUSD:
		return nil, reject(wr.Name, fmt.Sprintf("budget.lease.grantMilliUSD must be in [0, %d] (0 = default)", int64(maxWireMilliUSD)))
	case grantMilli == 0:
		// ADR-032 default: 0.1% of the limit, floored at 1 milliUSD.
		grantMilli = wb.LimitMilliUSD * DefaultLeaseGrantBP / 10_000
		if grantMilli < 1 {
			grantMilli = 1
		}
	}

	iv := DefaultLeaseRenewInterval
	if wb.Lease.RenewInterval != "" {
		parsed, err := time.ParseDuration(wb.Lease.RenewInterval)
		if err != nil {
			return nil, reject(wr.Name, fmt.Sprintf("budget.lease.renewInterval %q is not a Go duration", wb.Lease.RenewInterval))
		}
		if parsed < MinLeaseRenewInterval {
			return nil, reject(wr.Name, fmt.Sprintf("budget.lease.renewInterval must be >= %s", MinLeaseRenewInterval))
		}
		iv = parsed
	}

	return &Budget{
		LimitMicroUSD:      wb.LimitMilliUSD * microPerMilli,
		HardCap:            wb.HardCap,
		LeaseGrantMicroUSD: grantMilli * microPerMilli,
		LeaseRenewInterval: iv,
		AdminContact:       wb.AdminContact,
		Period:             period,
	}, nil
}

// routingFromV1Alpha1 converts the routing rule's exactly-one-of-three shapes.
// doc is the whole document so BudgetTiers.budgetRef can be resolved against
// a budget rule declared elsewhere in the same policy (ADR-041 D1).
func routingFromV1Alpha1(wr v1alpha1.Rule, doc *v1alpha1.GovernancePolicy, reject func(rule, reason string) *UnsupportedError) (*Routing, error) {
	wrt := wr.Routing
	affinitySet := wrt.OnAffinityConflict != ""
	tiersSet := wrt.BudgetTiers != nil
	shapes := 0
	for _, set := range []bool{affinitySet, tiersSet, wrt.Context != nil} {
		if set {
			shapes++
		}
	}
	if shapes != 1 {
		return nil, reject(wr.Name, "routing rule must set exactly one of onAffinityConflict, budgetTiers, or context")
	}
	if wrt.Context != nil {
		c, err := contextFromV1Alpha1(wr, reject)
		if err != nil {
			return nil, err
		}
		return &Routing{Context: c}, nil
	}
	if affinitySet {
		switch wrt.OnAffinityConflict {
		case v1alpha1.PreferAffinity, v1alpha1.PreferFallback:
		default:
			return nil, reject(wr.Name, fmt.Sprintf("unknown routing.onAffinityConflict %q", wrt.OnAffinityConflict))
		}
		return &Routing{Affinity: &Affinity{OnAffinityConflict: wrt.OnAffinityConflict}}, nil
	}

	bt := wrt.BudgetTiers
	if bt.BudgetRef == "" {
		return nil, reject(wr.Name, "routing.budgetTiers.budgetRef is required: it names the budget rule this tier's utilization is judged against")
	}
	var ref *v1alpha1.Rule
	for i := range doc.Spec.Rules {
		if doc.Spec.Rules[i].Name == bt.BudgetRef {
			ref = &doc.Spec.Rules[i]
			break
		}
	}
	if ref == nil || ref.Budget == nil {
		return nil, reject(wr.Name, fmt.Sprintf("routing.budgetTiers.budgetRef %q must name a budget rule in the same document", bt.BudgetRef))
	}
	if ref.Budget.Unlimited {
		return nil, reject(wr.Name, fmt.Sprintf("routing.budgetTiers.budgetRef %q must name a budget rule with a numeric limitMilliUSD: a tier against an unlimited budget is meaningless", bt.BudgetRef))
	}
	if len(bt.Tiers) == 0 {
		return nil, reject(wr.Name, "routing.budgetTiers.tiers must be non-empty")
	}

	tiers := make([]BudgetTier, 0, len(bt.Tiers))
	prevThreshold := 0
	for _, t := range bt.Tiers {
		if t.ThresholdPercent < 1 || t.ThresholdPercent > 99 {
			return nil, reject(wr.Name, fmt.Sprintf("routing.budgetTiers.tiers.thresholdPercent must be in [1, 99], got %d", t.ThresholdPercent))
		}
		if t.ThresholdPercent <= prevThreshold {
			return nil, reject(wr.Name, "routing.budgetTiers.tiers.thresholdPercent must be strictly increasing across tiers")
		}
		prevThreshold = t.ThresholdPercent
		if len(t.Substitute) == 0 {
			return nil, reject(wr.Name, "routing.budgetTiers.tiers.substitute must be non-empty")
		}
		sub := make(map[string]string, len(t.Substitute))
		values := make(map[string]bool, len(t.Substitute))
		for k, v := range t.Substitute {
			if k == "" || v == "" {
				return nil, reject(wr.Name, "routing.budgetTiers.tiers.substitute must not contain an empty key or value")
			}
			if k == v {
				return nil, reject(wr.Name, fmt.Sprintf("routing.budgetTiers.tiers.substitute maps %q to itself", k))
			}
			sub[k] = v
			values[v] = true
		}
		for k := range sub {
			if values[k] {
				return nil, reject(wr.Name, fmt.Sprintf("routing.budgetTiers.tiers.substitute: %q may not appear as both a key and a value (no chains)", k))
			}
		}
		tiers = append(tiers, BudgetTier{ThresholdPercent: t.ThresholdPercent, Substitute: sub})
	}

	return &Routing{BudgetTiers: &BudgetTiers{BudgetRef: bt.BudgetRef, Tiers: tiers}}, nil
}

// SensitiveData is a validated privacy restriction. The slices are owned.
type SensitiveData struct {
	OnDetected      v1alpha1.SensitiveDataAction
	OnUninspectable v1alpha1.SensitiveDataAction
	InternalModels  []string
}

// Context is a validated optional model recommendation. Mode is never empty.
type Context struct {
	Mode                 v1alpha1.ContextMode
	FromModels           []string
	SimpleModel          string
	ComplexModel         string
	MaxSimpleInputTokens int64
	ComplexKeywords      []string
}

func explicitModel(name string) bool {
	return strings.TrimSpace(name) != "" && !strings.ContainsAny(name, "*?[]")
}

func sensitiveDataFromV1Alpha1(wr v1alpha1.Rule, reject func(string, string) *UnsupportedError) (*SensitiveData, error) {
	sd := wr.SensitiveData
	if wr.FailurePolicy != v1alpha1.FailClosed {
		return nil, reject(wr.Name, "sensitiveData requires FailClosed")
	}
	for _, a := range []v1alpha1.SensitiveDataAction{sd.OnDetected, sd.OnUninspectable} {
		if a != v1alpha1.InternalOnly && a != v1alpha1.Block {
			return nil, reject(wr.Name, "sensitiveData actions must be InternalOnly or Block (both required)")
		}
	}
	if (sd.OnDetected == v1alpha1.InternalOnly || sd.OnUninspectable == v1alpha1.InternalOnly) && len(sd.InternalModels) == 0 {
		return nil, reject(wr.Name, "InternalOnly requires non-empty internalModels")
	}
	for _, m := range sd.InternalModels {
		if !explicitModel(m) {
			return nil, reject(wr.Name, "internalModels requires explicit non-empty model names without wildcards")
		}
	}
	return &SensitiveData{OnDetected: sd.OnDetected, OnUninspectable: sd.OnUninspectable, InternalModels: append([]string(nil), sd.InternalModels...)}, nil
}

func contextFromV1Alpha1(wr v1alpha1.Rule, reject func(string, string) *UnsupportedError) (*Context, error) {
	c := wr.Routing.Context
	if wr.FailurePolicy != v1alpha1.FailOpen {
		return nil, reject(wr.Name, "routing.context requires FailOpen")
	}
	mode := c.Mode
	if mode == "" {
		mode = v1alpha1.Shadow
	}
	if mode != v1alpha1.Shadow && mode != v1alpha1.Enforce {
		return nil, reject(wr.Name, "routing.context.mode must be Shadow or Enforce")
	}
	if len(c.FromModels) == 0 || !explicitModel(c.SimpleModel) || !explicitModel(c.ComplexModel) {
		return nil, reject(wr.Name, "routing.context requires fromModels and explicit simpleModel/complexModel")
	}
	for _, m := range c.FromModels {
		if !explicitModel(m) {
			return nil, reject(wr.Name, "routing.context.fromModels requires explicit non-empty model names")
		}
	}
	if c.MaxSimpleInputTokens <= 0 {
		return nil, reject(wr.Name, "routing.context.maxSimpleInputTokens must be positive")
	}
	for _, k := range c.ComplexKeywords {
		if strings.TrimSpace(k) == "" {
			return nil, reject(wr.Name, "routing.context.complexKeywords must not contain blank strings")
		}
	}
	return &Context{Mode: mode, FromModels: append([]string(nil), c.FromModels...), SimpleModel: c.SimpleModel, ComplexModel: c.ComplexModel, MaxSimpleInputTokens: c.MaxSimpleInputTokens, ComplexKeywords: append([]string(nil), c.ComplexKeywords...)}, nil
}
