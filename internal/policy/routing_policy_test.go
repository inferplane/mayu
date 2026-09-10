package policy

import (
	"encoding/json"
	"errors"
	"testing"

	v1alpha1 "github.com/inferplane/inferplane/api/v1alpha1"
)

func routingDoc(t *testing.T, rules string) v1alpha1.GovernancePolicy {
	t.Helper()
	var d v1alpha1.GovernancePolicy
	if err := json.Unmarshal([]byte(`{"apiVersion":"inferplane.dev/v1alpha1","kind":"GovernancePolicy","metadata":{"name":"routing","generation":1},"spec":{"subject":{"team":"eng"},"rules":`+rules+`}}`), &d); err != nil {
		t.Fatal(err)
	}
	return d
}

const privacyRules = `[{"name":"privacy","failurePolicy":"FailClosed","sensitiveData":{"onDetected":"InternalOnly","onUninspectable":"Block","internalModels":["private"]}}]`
const contextRules = `[{"name":"context","failurePolicy":"FailOpen","routing":{"context":{"fromModels":["premium"],"simpleModel":"cheap","complexModel":"premium","maxSimpleInputTokens":4096,"complexKeywords":["security"]}}}]`

func TestRoutingPolicyConversion(t *testing.T) {
	for _, raw := range []string{privacyRules, contextRules} {
		d := routingDoc(t, raw)
		p, err := FromV1Alpha1(&d)
		if err != nil {
			t.Errorf("valid routing policy: %v", err)
			continue
		}
		b, _ := json.Marshal(p)
		var got map[string]any
		_ = json.Unmarshal(b, &got)
		if raw == contextRules {
			rule := got["Rules"].([]any)[0].(map[string]any)
			mode := rule["Routing"].(map[string]any)["Context"].(map[string]any)["Mode"]
			if mode != "Shadow" {
				t.Errorf("default mode = %v", mode)
			}
		}
	}
	invalid := []string{
		`{"name":"p","failurePolicy":"FailOpen","sensitiveData":{"onDetected":"Block","onUninspectable":"Block"}}`,
		`{"name":"p","failurePolicy":"FailClosed","sensitiveData":{"onUninspectable":"Block"}}`,
		`{"name":"p","failurePolicy":"FailClosed","sensitiveData":{"onDetected":"Mask","onUninspectable":"Block"}}`,
		`{"name":"p","failurePolicy":"FailClosed","sensitiveData":{"onDetected":"Block","onUninspectable":"Allow"}}`,
		`{"name":"p","failurePolicy":"FailClosed","sensitiveData":{"onDetected":"InternalOnly","onUninspectable":"Block"}}`,
		`{"name":"p","failurePolicy":"FailClosed","sensitiveData":{"onDetected":"Block","onUninspectable":"InternalOnly","internalModels":["*"]}}`,
		`{"name":"p","failurePolicy":"FailClosed","sensitiveData":{"onDetected":"Block","onUninspectable":"InternalOnly","internalModels":[" "]}}`,
		`{"name":"p","failurePolicy":"FailClosed","budget":{"limitMilliUSD":10},"sensitiveData":{"onDetected":"Block","onUninspectable":"Block"}}`,
	}
	for _, raw := range invalid {
		d := routingDoc(t, "["+raw+"]")
		if _, err := FromV1Alpha1(&d); err == nil {
			t.Errorf("accepted invalid: %s", raw)
		}
	}
	// Mutate the JSON fixture to exercise independent context guards.
	for _, tc := range []struct {
		key   string
		value any
	}{
		{"mode", "Auto"}, {"maxSimpleInputTokens", 0}, {"maxSimpleInputTokens", -1},
		{"complexKeywords", []string{" "}}, {"fromModels", []string{}}, {"fromModels", []string{"*"}},
		{"simpleModel", ""}, {"complexModel", "*"},
	} {
		var rules []map[string]any
		_ = json.Unmarshal([]byte(contextRules), &rules)
		rules[0]["routing"].(map[string]any)["context"].(map[string]any)[tc.key] = tc.value
		raw, _ := json.Marshal(rules)
		d := routingDoc(t, string(raw))
		if _, err := FromV1Alpha1(&d); err == nil {
			t.Errorf("accepted context %s=%v", tc.key, tc.value)
		}
	}
	for _, extra := range []string{`"budgetTiers":{"budgetRef":"b","tiers":[]}`, `"onAffinityConflict":"PreferFallback"`} {
		var rules []map[string]any
		_ = json.Unmarshal([]byte(contextRules), &rules)
		var added map[string]any
		_ = json.Unmarshal([]byte("{"+extra+"}"), &added)
		for k, v := range added {
			rules[0]["routing"].(map[string]any)[k] = v
		}
		raw, _ := json.Marshal(rules)
		d := routingDoc(t, string(raw))
		if _, err := FromV1Alpha1(&d); err == nil {
			t.Error("accepted mixed routing subtypes")
		}
	}
	d := routingDoc(t, contextRules)
	d.Spec.Rules[0].FailurePolicy = v1alpha1.FailClosed
	if _, err := FromV1Alpha1(&d); err == nil {
		t.Error("accepted fail-closed context")
	}
}

func TestRoutingPolicyTargetValidation(t *testing.T) {
	for _, raw := range []string{privacyRules, contextRules} {
		s := NewEmptyStore()
		s.SetRoutedAndPriced(func(string) error { return errors.New("unpriced") })
		if got := s.ApplyWire([]v1alpha1.GovernancePolicy{routingDoc(t, raw)}); len(got) != 1 {
			t.Fatal("accepted unpriced target")
		}
	}
}

func TestRoutingPolicyStoreIsolationAndRejection(t *testing.T) {
	s := NewEmptyStore()
	lookup, ok := any(s).(interface {
		MatchingRoutingPolicies(string, string) ([]*Policy, error)
	})
	if !ok {
		t.Fatal("store has no fail-closed routing lookup")
	}
	privacy := routingDoc(t, privacyRules)
	context := routingDoc(t, contextRules)
	context.Metadata.Name = "user-context"
	context.Spec.Subject = v1alpha1.Subject{User: "u"}
	if r := s.ApplyWire([]v1alpha1.GovernancePolicy{privacy, context}); len(r) != 0 {
		t.Fatal(r)
	}
	for _, tc := range []struct {
		team, user string
		want       int
	}{{"eng", "u", 2}, {"eng", "", 1}, {"other", "u", 1}, {"other", "other", 0}} {
		got, err := lookup.MatchingRoutingPolicies(tc.team, tc.user)
		if err != nil || len(got) != tc.want {
			t.Fatalf("matching %v: %v %v", tc, got, err)
		}
	}
	got, _ := lookup.MatchingRoutingPolicies("eng", "u")
	got[0].Name = "mutated"
	got[0].Rules[0].Name = "mutated"
	got, _ = lookup.MatchingRoutingPolicies("eng", "u")
	if got[0].Name != "routing" || got[0].Rules[0].Name != "privacy" {
		t.Fatal("caller mutated policy snapshot")
	}
	// Replacing previously active privacy with a malformed non-privacy document must fail closed.
	bad := routingDoc(t, `[{"name":"bad","failurePolicy":"FailOpen"}]`)
	bad.Spec.Subject = v1alpha1.Subject{Team: "other"}
	rejectedSet := []v1alpha1.GovernancePolicy{bad, context}
	for delivery := 1; delivery <= 2; delivery++ {
		if r := s.ApplyWire(rejectedSet); len(r) != 1 {
			t.Fatalf("delivery %d: %v", delivery, r)
		}
		if _, err := lookup.MatchingRoutingPolicies("eng", "u"); !errors.Is(err, ErrSensitivePolicyRejected) {
			t.Fatalf("delivery %d lost prior sensitive provenance: %v", delivery, err)
		}
		for _, p := range s.Policies() {
			if p.Name == privacy.Metadata.Name {
				t.Fatal("rejected document unexpectedly still accepted")
			}
		}
	}
	// Omission cannot erase the pending rejection either.
	s.ApplyWire(nil)
	if _, err := lookup.MatchingRoutingPolicies("eng", "u"); err == nil {
		t.Fatal("rejection cleared before valid generation")
	}
	privacy.Metadata.Generation = 2
	if r := s.ApplyWire([]v1alpha1.GovernancePolicy{privacy}); len(r) != 0 {
		t.Fatal(r)
	}
	if _, err := lookup.MatchingRoutingPolicies("eng", "u"); err != nil {
		t.Fatal("valid recovery", err)
	}
	// First delivery of a rejected sensitive doc also protects its selected subject.
	s = NewEmptyStore()
	lookup = any(s).(interface {
		MatchingRoutingPolicies(string, string) ([]*Policy, error)
	})
	privacy.Spec.Rules[0].FailurePolicy = v1alpha1.FailOpen
	s.ApplyWire([]v1alpha1.GovernancePolicy{privacy})
	if _, err := lookup.MatchingRoutingPolicies("eng", "u"); err == nil {
		t.Fatal("rejected new privacy failed open")
	}
	if _, err := lookup.MatchingRoutingPolicies("other", "u"); err != nil {
		t.Fatal("unrelated subject blocked", err)
	}
	// A valid replacement (even removing privacy) is an explicit correction.
	valid := routingDoc(t, contextRules)
	valid.Metadata.Generation = 3
	s.ApplyWire([]v1alpha1.GovernancePolicy{valid})
	if _, err := lookup.MatchingRoutingPolicies("eng", "u"); err != nil {
		t.Fatal(err)
	}
}

func TestRoutingPolicyDeepCopies(t *testing.T) {
	privacy := routingDoc(t, privacyRules)
	context := routingDoc(t, contextRules)
	context.Metadata.Name = "context"
	s := NewEmptyStore()
	if r := s.ApplyWire([]v1alpha1.GovernancePolicy{privacy, context}); len(r) != 0 {
		t.Fatal(r)
	}
	privacy.Spec.Rules[0].SensitiveData.InternalModels[0] = "external"
	context.Spec.Rules[0].Routing.Context.FromModels[0] = "other"
	context.Spec.Rules[0].Routing.Context.ComplexKeywords[0] = "changed"
	mutate := func(ps []*Policy) {
		for _, p := range ps {
			for _, r := range p.Rules {
				if r.SensitiveData != nil {
					r.SensitiveData.InternalModels[0] = "external"
				}
				if r.Routing != nil && r.Routing.Context != nil {
					r.Routing.Context.FromModels[0] = "other"
					r.Routing.Context.ComplexKeywords[0] = "changed"
				}
			}
		}
	}
	assert := func() {
		ps, err := s.MatchingRoutingPolicies("eng", "")
		if err != nil {
			t.Fatal(err)
		}
		if ps[0].Rules[0].SensitiveData.InternalModels[0] != "private" || ps[1].Rules[0].Routing.Context.FromModels[0] != "premium" || ps[1].Rules[0].Routing.Context.ComplexKeywords[0] != "security" {
			t.Fatal("routing snapshot mutated", ps)
		}
	}
	assert()
	mutate(s.Policies())
	assert()
	ps, _ := s.MatchingRoutingPolicies("eng", "")
	mutate(ps)
	assert()
}

func TestRoutingPolicyDuplicateAndMalformedSubject(t *testing.T) {
	for _, mode := range []string{"duplicate", "missing-name", "missing-subject"} {
		t.Run(mode, func(t *testing.T) {
			s := NewEmptyStore()
			d := routingDoc(t, privacyRules)
			docs := []v1alpha1.GovernancePolicy{d}
			switch mode {
			case "duplicate":
				docs = append(docs, d)
			case "missing-name":
				docs[0].Metadata.Name = ""
			case "missing-subject":
				docs[0].Spec.Subject = v1alpha1.Subject{}
			}
			if r := s.ApplyWire(docs); len(r) == 0 {
				t.Fatal("accepted malformed privacy")
			}
			if _, err := s.MatchingRoutingPolicies("eng", ""); !errors.Is(err, ErrSensitivePolicyRejected) {
				t.Fatal("privacy gate missing", err)
			}
			if mode == "missing-subject" {
				if _, err := s.MatchingRoutingPolicies("any", ""); err == nil {
					t.Fatal("unknown scope failed open")
				}
			}
			d.Metadata.Generation = 2
			s.ApplyWire([]v1alpha1.GovernancePolicy{d})
			if _, err := s.MatchingRoutingPolicies("eng", ""); err != nil {
				t.Fatal("valid recovery failed", err)
			}
		})
	}
}

func TestRoutingPolicyLegacyDuplicatePartialAcceptance(t *testing.T) {
	s := NewEmptyStore()
	d := routingDoc(t, `[{"name":"budget","failurePolicy":"FailOpen","budget":{"limitMilliUSD":10}}]`)
	if rejected := s.ApplyWire([]v1alpha1.GovernancePolicy{d, d}); len(rejected) != 1 {
		t.Fatalf("legacy per-document rejection changed: %v", rejected)
	}
	if limits, ok := s.TeamLimits("eng"); !ok || limits.BudgetMicrosPerMonth != 10000 {
		t.Fatalf("first valid duplicate was discarded: %+v %v", limits, ok)
	}
	if _, err := s.MatchingRoutingPolicies("eng", ""); err != nil {
		t.Fatal("non-sensitive rejection blocked routing", err)
	}
}

func TestRoutingPolicyAllTargetsMustBeRoutedAndPriced(t *testing.T) {
	for _, badTarget := range []string{"private", "backup", "cheap", "premium"} {
		t.Run(badTarget, func(t *testing.T) {
			d := routingDoc(t, privacyRules)
			d.Spec.Rules[0].SensitiveData.InternalModels = append(d.Spec.Rules[0].SensitiveData.InternalModels, "backup")
			c := routingDoc(t, contextRules)
			c.Metadata.Name = "context"
			s := NewEmptyStore()
			s.SetRoutedAndPriced(func(model string) error {
				if model == badTarget {
					return errors.New("missing route or price")
				}
				return nil
			})
			rejected := s.ApplyWire([]v1alpha1.GovernancePolicy{d, c})
			if len(rejected) != 1 {
				t.Fatalf("target %s was not checked: %v", badTarget, rejected)
			}
			_, err := s.MatchingRoutingPolicies("eng", "")
			if (err != nil) != (badTarget == "private" || badTarget == "backup") {
				t.Fatalf("optional/security rejection confused: %v", err)
			}
		})
	}
}

func TestRoutingPolicyScopedUserAndConcurrentCopies(t *testing.T) {
	d := routingDoc(t, privacyRules)
	d.Spec.Subject.User = "u"
	c := routingDoc(t, contextRules)
	c.Metadata.Name = "context"
	s := NewEmptyStore()
	s.ApplyWire([]v1alpha1.GovernancePolicy{d, c})
	for _, tc := range []struct {
		team, user string
		want       int
	}{{"eng", "u", 2}, {"eng", "", 1}, {"eng", "other", 1}, {"other", "u", 0}} {
		ps, err := s.MatchingRoutingPolicies(tc.team, tc.user)
		if err != nil || len(ps) != tc.want {
			t.Fatalf("scoped match %+v: %v %v", tc, ps, err)
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			s.ApplyWire([]v1alpha1.GovernancePolicy{d, c})
		}
	}()
	for i := 0; i < 50; i++ {
		ps, err := s.MatchingRoutingPolicies("eng", "u")
		if err != nil || len(ps) != 2 {
			t.Fatalf("torn snapshot: %v %v", ps, err)
		}
		ps[0].Rules[0].SensitiveData.InternalModels[0] = "external"
		ps[1].Rules[0].Routing.Context.ComplexKeywords[0] = "changed"
	}
	<-done
	ps, err := s.MatchingRoutingPolicies("eng", "u")
	if err != nil || ps[0].Rules[0].SensitiveData.InternalModels[0] != "private" || ps[1].Rules[0].Routing.Context.ComplexKeywords[0] != "security" {
		t.Fatal("snapshot leaked mutable copies")
	}
}
