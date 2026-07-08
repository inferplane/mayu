// Package audit implements inferplane's tamper-evident audit log: two-phase
// records (request_started / request_completed), an instance-local SHA-256
// hash chain, a disk-backed WAL, and pluggable sinks (§5.4). cost is nil in M3
// (filled by the M5 BudgetStore); trace_id is reserved (v0.2 OTel); prev_hash
// is computed for real in M3.
package audit

import "encoding/json"

// PrincipalRef identifies the acting principal. For data-plane records KeyID
// is the virtual key; for admin-plane records (ADR-004) User carries the
// opaque OIDC `sub` (never email — PII stays out of the chain) and AuthMethod
// records "oidc" vs "break_glass". AuthMethod is appended at the END of the
// struct: the hash chain verifies exact line bytes, and an omitempty pointer
// keeps pre-change records byte-identical (mixed-version chains still verify).
type PrincipalRef struct {
	KeyID      string  `json:"key_id"`
	Team       string  `json:"team"`
	User       *string `json:"user,omitempty"` // OIDC sub (admin plane) — opaque, never email
	AuthMethod *string `json:"auth_method,omitempty"`
}

type RequestRef struct {
	Ingress        string `json:"ingress"` // "anthropic" | "openai"
	ModelRequested string `json:"model_requested"`
	ModelResolved  string `json:"model_resolved,omitempty"`
	Provider       string `json:"provider,omitempty"`
	ProviderAPI    string `json:"provider_api,omitempty"`
	Stream         bool   `json:"stream"`
	// PIIMasked records that the request text was PII-masked before forwarding
	// (ADR-009). A boolean only — never the redacted values (the count is a
	// metric label); omitted when masking was off.
	PIIMasked bool `json:"pii_masked,omitempty"`
}

type OutcomeRef struct {
	Status        int      `json:"status"`
	FallbackUsed  bool     `json:"fallback_used"`
	FallbackChain []string `json:"fallback_chain,omitempty"`
	Partial       bool     `json:"partial"`
	Error         *string  `json:"error"`
}

type UsageRef struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens,omitempty"`
	Estimated                bool  `json:"estimated"`
}

type LatencyRef struct {
	TTFTMs  int64 `json:"ttft_ms,omitempty"`
	TotalMs int64 `json:"total_ms"`
}

// CostRef is the settled per-request cost in integer µUSD (filled by the M5
// governance Settle path). PricingMissing marks an on_missing=allow request
// whose (provider,model) rate was absent (cost reported 0); PricingVersion
// records the rate table version used.
type CostRef struct {
	AmountUSDMicros int64  `json:"amount_usd_micros"`
	PricingMissing  bool   `json:"pricing_missing"`
	PricingVersion  string `json:"pricing_version,omitempty"`
}

// Record is one audit entry. Field order here defines the canonical JSON used
// for hashing (encoding/json marshals struct fields in declaration order).
type Record struct {
	SchemaVersion int          `json:"schema_version"`
	Event         string       `json:"event"` // request_started | request_completed | admin_key_created | admin_key_revoked | admin_denied | body_accessed | body_deleted
	ID            string       `json:"id"`    // ULID
	TS            string       `json:"ts"`
	Instance      string       `json:"instance"`
	Principal     PrincipalRef `json:"principal"`
	Request       RequestRef   `json:"request"`
	Outcome       *OutcomeRef  `json:"outcome,omitempty"`
	Usage         *UsageRef    `json:"usage,omitempty"`
	Cost          *CostRef     `json:"cost,omitempty"` // nil until settled (M5)
	Latency       *LatencyRef  `json:"latency,omitempty"`
	TraceID       *string      `json:"trace_id"` // reserved (v0.2 OTel)
	PrevHash      string       `json:"prev_hash"`
	// BodyRef and RecordRef are appended at the END (D4, ADR-018) — an
	// omitempty pointer keeps pre-change records byte-identical, so mixed-
	// version chains still verify (AuthMethod precedent above).
	//
	// BodyRef is the opaque body-store reference, set only on a
	// request_completed record whose body was captured (audit.log_bodies
	// on). BodyRef is NEVER set on body_accessed/body_deleted — those events
	// are metadata-only BY SCHEMA (§4.7 anti-recursion: a body view can never
	// itself be body-logged), enforced by the emitting code path and tested.
	BodyRef *string `json:"body_ref,omitempty"`
	// RecordRef is the request_completed record's ULID that a body_accessed
	// or body_deleted event refers to (access accountability, §6.3).
	RecordRef *string `json:"record_ref,omitempty"`
}

// Canonical returns the deterministic JSON used both for the on-disk record and
// as the input to the next record's prev_hash. encoding/json emits struct
// fields in declaration order, so this is byte-stable.
func (r Record) Canonical() ([]byte, error) { return json.Marshal(r) }
