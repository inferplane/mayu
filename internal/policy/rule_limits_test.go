package policy

import (
	"fmt"
	"testing"

	"github.com/inferplane/inferplane/api/v1alpha1"
)

func TestPolicyRuleCountBound(t *testing.T) {
	for _, count := range []int{256, 257} {
		doc := routingDoc(t, contextRules)
		doc.Spec.Rules = make([]v1alpha1.Rule, count)
		for i := range doc.Spec.Rules {
			doc.Spec.Rules[i] = v1alpha1.Rule{
				Name: fmt.Sprintf("rule-%d", i), FailurePolicy: v1alpha1.FailOpen,
				ModelAccess: &v1alpha1.ModelAccessRule{Allow: []string{"*"}},
			}
		}
		_, err := FromV1Alpha1(&doc)
		if (err == nil) != (count == 256) {
			t.Errorf("%d rules: conversion err=%v", count, err)
		}
		rejected := NewEmptyStore().ApplyWire([]v1alpha1.GovernancePolicy{doc})
		if (len(rejected) == 0) != (count == 256) {
			t.Errorf("%d rules: distribution rejections=%v", count, rejected)
		}
	}
}

func TestBudgetTierCountBound(t *testing.T) {
	for _, strict := range []bool{false, true} {
		maximum := 99
		if strict {
			maximum = 100
		}
		for _, count := range []int{maximum, maximum + 1} {
			doc := routingDoc(t, routingBudgetRules)
			bt := doc.Spec.Rules[2].Routing.BudgetTiers
			bt.EnforceTargets = strict
			bt.Tiers = make([]v1alpha1.BudgetTier, count)
			for i := range bt.Tiers {
				bt.Tiers[i] = v1alpha1.BudgetTier{
					ThresholdPercent: i + 1, Substitute: map[string]string{"premium": "cheap"},
				}
			}
			_, err := FromV1Alpha1(&doc)
			if (err == nil) != (count == maximum) {
				t.Errorf("strict=%v tiers=%d: err=%v", strict, count, err)
			}
		}
	}
}
