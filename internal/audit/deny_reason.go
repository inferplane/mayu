package audit

// DenyReason constrains the closed set of machine-readable deny-reason codes
// written to OutcomeRef.Error. OutcomeRef.Error keeps its existing *string JSON
// wire shape as a documented external schema contract; this type only limits
// which string values are written there, avoiding a wire-format break.
type DenyReason string

const (
	DenyModelNotAllowed      DenyReason = "model_not_allowed"
	DenyTeamRateLimited      DenyReason = "team_rate_limited"
	DenyTeamTokenRateLimited DenyReason = "team_token_rate_limited"
	DenyTeamQuotaExceeded    DenyReason = "team_quota_exceeded"
	DenyKeyRateLimited       DenyReason = "key_rate_limited"
	DenyKeyTokenRateLimited  DenyReason = "key_token_rate_limited"
	DenyTeamBudgetExceeded   DenyReason = "team_budget_exceeded"
	DenyKeyBudgetExceeded    DenyReason = "key_budget_exceeded"
	DenyRegionBlocked        DenyReason = "region_blocked"
)

// Ptr returns d as a plain string pointer.
func (d DenyReason) Ptr() *string {
	s := string(d)
	return &s
}
