package policy

import (
	"testing"
	"time"

	"github.com/inferplane/inferplane/api/v1alpha1"
)

func authorityDoc() v1alpha1.GovernancePolicy {
	return v1alpha1.GovernancePolicy{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: v1alpha1.KindGovernancePolicy},
		Metadata: v1alpha1.ObjectMeta{Name: "budget", Generation: 1},
		Spec: v1alpha1.GovernancePolicySpec{Subject: v1alpha1.Subject{Team: "team", User: "user"}, Rules: []v1alpha1.Rule{
			{Name: "cap", FailurePolicy: v1alpha1.FailClosed, Budget: &v1alpha1.BudgetRule{LimitMilliUSD: 100, HardCap: true}},
		}},
	}
}

func TestAuthorityWindowsAndStableIdentity(t *testing.T) {
	doc := authorityDoc()
	now := time.Date(2026, 9, 30, 23, 59, 59, 0, time.UTC)
	first, err := AuthorityBudgets([]v1alpha1.GovernancePolicy{doc}, now)
	if err != nil || len(first) != 1 {
		t.Fatalf("definitions=%+v err=%v", first, err)
	}
	a := first[0]
	if a.WindowStart != time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC) ||
		a.WindowEnd != time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC) ||
		a.LimitMicroUSD != 100000 || !a.HardCap || len(a.Key) != 64 {
		t.Fatalf("invalid authority definition: %+v", a)
	}
	doc.Metadata.Generation++
	doc.Spec.Rules[0].Budget.LimitMilliUSD = 200
	edited, _ := AuthorityBudgets([]v1alpha1.GovernancePolicy{doc}, now)
	if edited[0].Key != a.Key || edited[0].WindowID != a.WindowID || edited[0].Revision == a.Revision {
		t.Fatal("policy edit reset budget identity or retained stale revision")
	}
	next, _ := AuthorityBudgets([]v1alpha1.GovernancePolicy{doc}, now.Add(time.Second))
	if next[0].Key != a.Key || next[0].WindowID == a.WindowID {
		t.Fatal("calendar rollover did not create a distinct owned window")
	}
	doc.Spec.Rules[0].Budget.Period = v1alpha1.PeriodCalendarDay
	daily, _ := AuthorityBudgets([]v1alpha1.GovernancePolicy{doc}, now)
	if daily[0].Key == a.Key || daily[0].WindowEnd.Sub(daily[0].WindowStart) != 24*time.Hour {
		t.Fatal("day and month authority collided")
	}
}

func TestAuthorityBudgetsRejectInvalidAndDuplicateSources(t *testing.T) {
	doc := authorityDoc()
	if _, err := AuthorityBudgets([]v1alpha1.GovernancePolicy{doc, doc}, time.Now()); err == nil {
		t.Fatal("duplicate policy accepted")
	}
	doc.Spec.Rules[0].Budget.HardCap = true
	doc.Spec.Rules[0].FailurePolicy = v1alpha1.FailOpen
	if _, err := AuthorityBudgets([]v1alpha1.GovernancePolicy{doc}, time.Now()); err == nil {
		t.Fatal("fail-open hard authority accepted")
	}
}

func TestAuthorityBundleCannotOmitBudgetOrChangeLimit(t *testing.T) {
	docs := []v1alpha1.GovernancePolicy{authorityDoc()}
	now := time.Now().UTC()
	budgets, err := AuthorityBudgets(docs, now)
	if err != nil {
		t.Fatal(err)
	}
	response := AuthorityResponse{Protocol: AuthorityProtocol, ServerTime: now, Generation: GenerationOf(docs), Policies: docs, Budgets: budgets}
	if err := ValidateAuthorityBundle(&response); err != nil {
		t.Fatal(err)
	}
	response.Budgets = nil
	if err := ValidateAuthorityBundle(&response); err == nil {
		t.Fatal("omitted hard budget accepted")
	}
	response.Budgets = budgets
	response.Budgets[0].LimitMicroUSD++
	if err := ValidateAuthorityBundle(&response); err == nil {
		t.Fatal("forged limit accepted")
	}
	response.Policies = nil
	if err := ValidateAuthorityBundle(&response); err == nil {
		t.Fatal("missing policy bundle accepted")
	}
	empty := AuthorityResponse{Protocol: AuthorityProtocol, ServerTime: now, Generation: GenerationOf(nil), Policies: []v1alpha1.GovernancePolicy{}, Budgets: []AuthorityBudget{}}
	if err := ValidateAuthorityBundle(&empty); err != nil {
		t.Fatalf("explicit empty bundle rejected: %v", err)
	}
}
