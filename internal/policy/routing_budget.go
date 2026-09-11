package policy

// IsRoutingOnlyBudget reports whether a validated team budget is an accounting
// and routing threshold rather than an admission cap: it must be a finite,
// non-hard budget referenced by an EnforceTargets tier in this same document.
// Legacy references, hard caps and unsupported user-scoped rules never qualify.
// This classification is shared by data-plane limit folding and control-plane
// lease issuance; it does not depend on whether the tier is currently active.
func IsRoutingOnlyBudget(p *Policy, ruleName string) bool {
	if p == nil || ruleName == "" || p.Subject.Team == "" || p.Subject.User != "" {
		return false
	}
	var budget *Budget
	for _, rule := range p.Rules {
		if rule.Name == ruleName {
			budget = rule.Budget
			break
		}
	}
	if budget == nil || budget.HardCap || budget.Unlimited || budget.LimitMicroUSD <= 0 {
		return false
	}
	for _, rule := range p.Rules {
		if rule.Routing != nil && rule.Routing.BudgetTiers != nil {
			tier := rule.Routing.BudgetTiers
			if tier.EnforceTargets && tier.BudgetRef == ruleName {
				return true
			}
		}
	}
	return false
}
