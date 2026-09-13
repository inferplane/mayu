// Package v1alpha1 holds the versioned wire types of the inferplane config
// API — the contract between the control plane (inferplaned) and the
// node-local data plane (mayu).
//
// The schema borrows the Kubernetes resource shape (apiVersion / kind /
// metadata / spec) so documents are kubectl-friendly and CNCF-idiomatic, but
// DELIVERY is inferplane's own gRPC/HTTP channel: workstation mode has no
// K8s API server, so nothing here may depend on Kubernetes machinery
// (ADR-031). The same document can be applied as a K8s CRD, loaded from a
// local file (hot reload), or pushed by inferplaned. v1alpha1 is unstable —
// fields may change without notice until v1beta1.
//
// Money on this API is integer milliUSD: 1000 = $1 (ADR-032). That is the
// operator-facing resolution for limits and lease grants. INTERNAL cost
// accounting stays integer microUSD: per-token costs are sub-milliUSD (e.g.
// $0.25/MTok input is 0.25 µUSD per token), and settling in milliUSD would
// round small requests to zero — the ADR-030 bug class. The boundary
// conversion (×1000) is exact and lives in internal/policy.
//
// Version-skew stance: a data plane that receives a document it does not
// fully understand must reject it explicitly and report the rejection to the
// control plane — silent ignoring is the worst failure mode a governance
// tool can have. The rejection logic lives in internal/policy, which both
// binaries import; this package is types only.
package v1alpha1

// APIVersion identifies this schema generation on the wire.
const APIVersion = "inferplane.dev/v1alpha1"

// KindGovernancePolicy is the kind of the rule-set document.
const KindGovernancePolicy = "GovernancePolicy"

// TypeMeta mirrors the Kubernetes resource envelope.
type TypeMeta struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
}

// ObjectMeta names a document and tracks its control-plane edit generation.
type ObjectMeta struct {
	Name string `json:"name"`
	// Generation increments on every control-plane edit; the data plane
	// echoes it in consumption reports so operators can see which revision
	// each proxy is enforcing before propagating a new one.
	Generation int64 `json:"generation,omitempty"`
}

// GovernancePolicy is a versioned set of governance rules distributed from
// the control plane to data planes.
type GovernancePolicy struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta           `json:"metadata"`
	Spec     GovernancePolicySpec `json:"spec"`
}

// GovernancePolicySpec carries the subject and rules of one policy document.
type GovernancePolicySpec struct {
	Subject Subject `json:"subject"`
	Rules   []Rule  `json:"rules"`
}

// Subject selects who a policy governs. At least one selector is required —
// user-level and team-level governance are equal citizens (ADR-032).
//
// Team is the organizational unit (a department maps here); User is an
// individual, matched against the request's resolved identity (OIDC sub, or
// a virtual key's owner). Setting both scopes the policy to that user within
// that team. Several policies may match one request; enforcement applies all
// of them and the most restrictive outcome wins (block beats warn — the same
// tie rule two-phase governance already uses).
type Subject struct {
	Team string `json:"team,omitempty"`
	User string `json:"user,omitempty"`
}

// FailurePolicy is what a rule does when the control plane is unreachable
// and the rule's lease (if any) has expired. It is PER RULE, never global:
// a global fail-open voids budget control, a global fail-closed voids the
// fault-isolation argument for a node-local data plane.
type FailurePolicy string

const (
	// FailOpen keeps serving when the control plane is unreachable.
	FailOpen FailurePolicy = "FailOpen"
	// FailClosed blocks when the rule can no longer be enforced locally.
	FailClosed FailurePolicy = "FailClosed"
)

// Rule is one governance rule. Exactly one of the kind-specific fields
// (Budget, Routing, ModelAccess, Rate, SensitiveData) is set. FailurePolicy is REQUIRED —
// there is no default, because a defaulted failure mode is a silent one.
type Rule struct {
	Name          string             `json:"name"`
	FailurePolicy FailurePolicy      `json:"failurePolicy"`
	Budget        *BudgetRule        `json:"budget,omitempty"`
	Routing       *RoutingRule       `json:"routing,omitempty"`
	ModelAccess   *ModelAccessRule   `json:"modelAccess,omitempty"`
	Rate          *RateRule          `json:"rate,omitempty"`
	SensitiveData *SensitiveDataRule `json:"sensitiveData,omitempty"`
	TokenQuota    *TokenQuotaRule    `json:"tokenQuota,omitempty"`
}

// TokenQuotaRule is a global calendar token cap. Only the shared-governance
// profile can enforce it; other data planes explicitly reject the document.
type TokenQuotaRule struct {
	LimitTokens int64        `json:"limitTokens"`
	Period      BudgetPeriod `json:"period"`
}

// BudgetPeriod is the calendar window a budget rule's limit applies to. It is
// PER RULE, not per document: hardCap, failurePolicy, lease and adminContact
// are all per-rule knobs, so "day = soft, month = hard" can only be expressed
// as two rules — and the control plane's lease ledger is keyed per rule, so one
// rule cannot hold two independent leases either. A day cap and a month cap are
// two entries in spec.rules.
type BudgetPeriod string

const (
	// PeriodCalendarDay resets at the next midnight in the data plane's
	// configured budget_timezone.
	PeriodCalendarDay BudgetPeriod = "CalendarDay"
	// PeriodCalendarMonth resets at the first instant of next month in that
	// same timezone. It is also the meaning of an EMPTY period, so every
	// document written before this field existed keeps its exact meaning.
	PeriodCalendarMonth BudgetPeriod = "CalendarMonth"
)

// BudgetRule enforces spend via budget leases: the control plane grants the
// data plane a slice of budget it may burn without a network round trip, the
// data plane reports consumption asynchronously and renews (ADR-031). Cost
// control is near-real-time by default: worst-case overshoot is bounded by
// lease grant × number of data planes, and the defaults (0.1% grant, 10s
// renew — ADR-032) keep that bound tight without putting the control plane
// on the request path.
type BudgetRule struct {
	// Unlimited declares, explicitly and auditably, that this dimension has
	// no cap — the alternative to simply omitting the rule (which reads as
	// "no policy decision was made" rather than "we decided not to cap
	// this"). When true, LimitMilliUSD/HardCap/Lease/AdminContact/Period must all
	// be unset; the rule exists only to be enforced=false in an observation
	// sense — its presence is itself the record.
	Unlimited bool `json:"unlimited,omitempty"`
	// LimitMilliUSD is the global limit this rule enforces across all
	// data planes for the budget window. 1000 = $1. Required unless
	// Unlimited is true.
	LimitMilliUSD int64 `json:"limitMilliUSD,omitempty"`
	// HardCap marks the limit as inviolable. A hard-cap rule MUST carry
	// FailurePolicy=FailClosed: when its lease expires and the control
	// plane is unreachable, serving stops. Soft budgets fail open.
	HardCap bool `json:"hardCap,omitempty"`
	// Lease tunes the local-enforcement grant. Optional — zero values take
	// the ADR-032 defaults.
	Lease LeaseSpec `json:"lease,omitempty"`
	// AdminContact is an opaque string surfaced verbatim in the 402 response
	// and GET /v1/usage once this rule is the binding budget (e.g. an email
	// or a Slack channel) — where to go when a hard cap blocks. Optional;
	// empty means the error carries no contact hint.
	AdminContact string `json:"adminContact,omitempty"`
	// Period is the calendar window this rule's limit applies to. Empty means
	// CalendarMonth — the window every budget rule enforced before this field
	// existed — so omitting it preserves an existing document's meaning
	// exactly. Must not be combined with Unlimited: "no cap" is a statement
	// about the budget dimension as a whole and has no window.
	Period BudgetPeriod `json:"period,omitempty"`
}

// LeaseSpec sizes a budget lease. Both fields are optional; defaults are
// fixed by ADR-032 and applied in internal/policy.
type LeaseSpec struct {
	// GrantMilliUSD is how much budget one lease grants one data plane.
	// 0 = default: 0.1% of LimitMilliUSD, floored at 1 milliUSD.
	GrantMilliUSD int64 `json:"grantMilliUSD,omitempty"`
	// RenewInterval is how often the data plane reports consumption and
	// renews, as a Go duration string. Empty = default "10s"; values below
	// 1s are rejected.
	RenewInterval string `json:"renewInterval,omitempty"`
}

// ModelAccessRule restricts which models the subject may request. Matching
// happens after alias canonicalization, mirroring the existing key/team RBAC
// (`allowed_models`); "*" allows every configured model.
type ModelAccessRule struct {
	Allow []string `json:"allow"`
}

// RateRule limits request and token throughput for the subject. At least one
// of the two must be positive; 0 means "not limited on this dimension".
type RateRule struct {
	// Unlimited declares, explicitly and auditably, that this dimension has
	// no cap — see BudgetRule.Unlimited for the rationale. When true, RPM
	// and TPM must both be unset.
	Unlimited bool `json:"unlimited,omitempty"`
	// RPM is requests per minute.
	RPM int64 `json:"rpm,omitempty"`
	// TPM is tokens per minute.
	TPM int64 `json:"tpm,omitempty"`
}

// ConflictPreference resolves the collision between cache-affinity routing
// and provider fallback: failing over to another region/profile cold-starts
// the server-side prompt cache and can RAISE cost. The preference is
// explicit per rule — neither side wins by default.
type ConflictPreference string

const (
	// PreferAffinity keeps the session pinned to its cache-warm target
	// even when fallback would otherwise fire.
	PreferAffinity ConflictPreference = "PreferAffinity"
	// PreferFallback lets fallback move the session, accepting the
	// server-cache cold start.
	PreferFallback ConflictPreference = "PreferFallback"
)

// RoutingRule is one of three mutually exclusive shapes, selected by which
// field is set:
//
//   - OnAffinityConflict — cache-affinity routing (pins sessions/prefixes to
//     keep server-side prompt caches warm). Rejected by every data plane
//     build today (internal/policy checkEnforceable); parked behind the
//     unimplemented internal/cache.VolatileStore.
//
//   - BudgetTiers — cost-driven model substitution keyed on budget
//     utilization (ADR-041). Enforceable independently of the affinity half.
//
//   - Context — optional request-context model selection (Shadow by default).
//
// Exactly one must be set; internal/policy.FromV1Alpha1 rejects a rule that
// sets multiple or none.
type RoutingRule struct {
	// OnAffinityConflict: what to do when fallback wants to move a session
	// that cache affinity wants to keep pinned. Required when this half of
	// the rule is used.
	OnAffinityConflict ConflictPreference `json:"onAffinityConflict,omitempty"`
	// BudgetTiers (ADR-041): substitute designated requested-model names for
	// a cheaper target once a named budget rule's utilization crosses a
	// threshold. Required when this half of the rule is used.
	BudgetTiers *BudgetTiersRule `json:"budgetTiers,omitempty"`
	// Context selects a model from locally inspected request complexity.
	Context *ContextRule `json:"context,omitempty"`
}

// BudgetTiersRule maps budget-utilization thresholds of one named budget
// rule (BudgetRef) to a per-requested-model substitution map. Substitution
// is keyed by REQUESTED model name, never by session, so a long-conversation
// (prompt-cached) model can simply be left out of every tier's map and is
// never touched — the intended targets are subagent/background model names
// (ADR-041 §Context, constraint 2).
type BudgetTiersRule struct {
	// BudgetRef names a budget rule IN THE SAME DOCUMENT whose utilization
	// this rule is judged against. That rule must carry a numeric
	// limitMilliUSD — a tier against an unlimited budget is meaningless.
	BudgetRef string `json:"budgetRef"`
	// Tiers must be strictly increasing by ThresholdPercent; the highest
	// tier whose threshold the current utilization has crossed is active.
	Tiers []BudgetTier `json:"tiers"`
	// EnforceTargets constrains every attempt to the active substitution target.
	// Unlike legacy substitution, an unavailable target refuses the request.
	EnforceTargets bool `json:"enforceTargets,omitempty"`
}

// BudgetTier activates its Substitute map once utilization reaches
// ThresholdPercent of the referenced budget rule's limit.
type BudgetTier struct {
	// ThresholdPercent must be in [1, 99], or [1, 100] with EnforceTargets,
	// and strictly greater than the previous tier's in the same rule.
	ThresholdPercent int `json:"thresholdPercent"`
	// Substitute maps requested model name -> substitution target. A model
	// may not appear as both a key and a value in one rule (no chains); the
	// map must be non-empty with no empty key or value.
	Substitute map[string]string `json:"substitute"`
}

// SensitiveDataAction is a mandatory destination or transformation obligation.
type SensitiveDataAction string

const (
	InternalOnly SensitiveDataAction = "InternalOnly"
	Block        SensitiveDataAction = "Block"
	Mask         SensitiveDataAction = "Mask"
)

// SensitiveDataRule restricts detected or uninspectable request content.
// Both actions are required. InternalOnly requires explicit internalModels.
type SensitiveDataRule struct {
	OnDetected      SensitiveDataAction `json:"onDetected"`
	OnUninspectable SensitiveDataAction `json:"onUninspectable"`
	InternalModels  []string            `json:"internalModels,omitempty"`
}

// ContextMode selects observation or optional model substitution.
type ContextMode string

const (
	Shadow  ContextMode = "Shadow"
	Enforce ContextMode = "Enforce"
)

// ContextRule recommends among explicit models based on local inspection.
// NormalModel adds a third class; Stability opts in to compatible history/tool
// routing and local affinity. Omitted mode defaults to Shadow; nil Stability and
// omitted NormalModel preserve the legacy behavior. Context requires FailOpen.
type ContextRule struct {
	Mode                 ContextMode       `json:"mode,omitempty"`
	FromModels           []string          `json:"fromModels"`
	SimpleModel          string            `json:"simpleModel"`
	ComplexModel         string            `json:"complexModel"`
	MaxSimpleInputTokens int64             `json:"maxSimpleInputTokens"`
	ComplexKeywords      []string          `json:"complexKeywords,omitempty"`
	NormalModel          string            `json:"normalModel,omitempty"`
	MaxNormalInputTokens int64             `json:"maxNormalInputTokens,omitempty"`
	Stability            *ContextStability `json:"stability,omitempty"`
}

// ContextStability explicitly enables compatible history/tool routing and local
// session affinity. An omitted object preserves legacy context behavior.
type ContextStability struct {
	MinHold     string `json:"minHold,omitempty"`
	MinRequests int    `json:"minRequests,omitempty"`
	SessionTTL  string `json:"sessionTTL,omitempty"`
}
