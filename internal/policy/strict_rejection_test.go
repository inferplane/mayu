package policy

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/inferplane/inferplane/api/v1alpha1"
)

func requireRoutingRejection(t *testing.T, s *Store, team, user string) {
	t.Helper()
	rules, err := s.MatchingRoutingPolicies(team, user)
	if !errors.Is(err, ErrSensitivePolicyRejected) || len(rules) != 0 {
		t.Fatalf("mandatory routing rejection did not fail closed: %v %v", rules, err)
	}
}

func TestStrictRejectedFirstDistributionIsGated(t *testing.T) {
	for _, failure := range []string{"target", "threshold", "version", "subject", "name", "duplicate"} {
		t.Run(failure, func(t *testing.T) {
			s := NewEmptyStore()
			valid := routingDoc(t, routingBudgetRules)
			bad := routingDoc(t, routingBudgetRules)
			docs := []v1alpha1.GovernancePolicy{bad}
			switch failure {
			case "target":
				s.SetRoutedAndPriced(func(string) error { return errors.New("missing target") })
			case "threshold":
				docs[0].Spec.Rules[2].Routing.BudgetTiers.Tiers[0].ThresholdPercent = 101
			case "version":
				docs[0].APIVersion = "inferplane.dev/v9"
			case "subject":
				docs[0].Spec.Subject = v1alpha1.Subject{}
			case "name":
				docs[0].Metadata.Name = ""
			case "duplicate":
				docs = append(docs, bad)
			}
			if rejected := s.ApplyWire(docs); len(rejected) == 0 {
				t.Fatal("invalid strict document accepted")
			}
			requireRoutingRejection(t, s, "eng", "")
			if failure == "subject" {
				requireRoutingRejection(t, s, "any-team", "any-user")
			} else if _, err := s.MatchingRoutingPolicies("unrelated", ""); err != nil {
				t.Fatal("rejection blocked unrelated subject")
			}
			s.ApplyWire(nil)
			requireRoutingRejection(t, s, "eng", "")
			s.SetRoutedAndPriced(nil)
			valid.Metadata.Generation = 2
			if rejected := s.ApplyWire([]v1alpha1.GovernancePolicy{valid}); len(rejected) != 0 {
				t.Fatal(rejected)
			}
			if _, err := s.MatchingRoutingPolicies("eng", ""); err != nil {
				t.Fatalf("valid replacement did not recover: %v", err)
			}
		})
	}
}

func TestStrictRejectedReplacementRetainsOldAndNewScope(t *testing.T) {
	for _, retainsStrict := range []bool{false, true} {
		s := NewEmptyStore()
		valid := routingDoc(t, routingBudgetRules)
		if rejected := s.ApplyWire([]v1alpha1.GovernancePolicy{valid}); len(rejected) != 0 {
			t.Fatal(rejected)
		}
		bad := routingDoc(t, routingBudgetRules)
		bad.Spec.Subject.Team = "new-team"
		bad.Metadata.Generation = 2
		if retainsStrict {
			bad.Spec.Rules[2].Routing.BudgetTiers.BudgetRef = "missing"
		} else {
			// Even removal of every strict field cannot hide the provenance
			// of the invalid replacement of an active strict document.
			bad.Spec.Rules = nil
		}
		for range 2 {
			if rejected := s.ApplyWire([]v1alpha1.GovernancePolicy{bad}); len(rejected) != 1 {
				t.Fatal(rejected)
			}
			requireRoutingRejection(t, s, "eng", "")
			if retainsStrict {
				requireRoutingRejection(t, s, "new-team", "")
			}
		}
		other := routingDoc(t, contextRules)
		other.Metadata.Name = "unrelated"
		s.ApplyWire([]v1alpha1.GovernancePolicy{other})
		requireRoutingRejection(t, s, "eng", "")
		// A valid explicit removal of strict routing is a legitimate recovery.
		recovery := routingDoc(t, contextRules)
		recovery.Metadata.Generation = 3
		if rejected := s.ApplyWire([]v1alpha1.GovernancePolicy{recovery}); len(rejected) != 0 {
			t.Fatal(rejected)
		}
		for _, team := range []string{"eng", "new-team"} {
			if _, err := s.MatchingRoutingPolicies(team, ""); err != nil {
				t.Fatalf("valid correction left %s gated: %v", team, err)
			}
		}
	}
}

func TestStrictRejectionGateDoesNotApplyToLegacyOptionalTiers(t *testing.T) {
	s := NewEmptyStore()
	legacy := routingDoc(t, routingBudgetRules)
	legacy.Spec.Rules[2].Routing.BudgetTiers.EnforceTargets = false
	legacy.Spec.Rules[2].Routing.BudgetTiers.Tiers[0].ThresholdPercent = 99
	if rejected := s.ApplyWire([]v1alpha1.GovernancePolicy{legacy}); len(rejected) != 0 {
		t.Fatal(rejected)
	}
	legacy.Spec.Rules[2].Routing.BudgetTiers.Tiers[0].ThresholdPercent = 100
	if rejected := s.ApplyWire([]v1alpha1.GovernancePolicy{legacy}); len(rejected) != 1 {
		t.Fatal("legacy invalid threshold was accepted")
	}
	if _, err := s.MatchingRoutingPolicies("eng", ""); err != nil {
		t.Fatalf("optional tier rejection acquired mandatory semantics: %v", err)
	}
}

func TestStrictUnknownWireFieldsRemainRejected(t *testing.T) {
	doc := routingDoc(t, routingBudgetRules)
	raw, _ := json.Marshal(doc)
	raw = []byte(strings.Replace(string(raw), `"enforceTargets":true`, `"enforceTargets":true,"futureMandatoryMode":true`, 1))
	if _, err := ParseWireDocs(raw); err == nil {
		t.Fatal("strict document's unsupported wire field was silently accepted")
	}
}
