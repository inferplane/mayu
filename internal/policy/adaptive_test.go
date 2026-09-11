package policy

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/inferplane/inferplane/api/v1alpha1"
)

const adaptiveContext = `[{"name":"context","failurePolicy":"FailOpen","routing":{"context":{"fromModels":["premium"],"simpleModel":"cheap","normalModel":"normal","complexModel":"premium","maxSimpleInputTokens":100,"maxNormalInputTokens":1000,"stability":{}}}}]`

func TestAdaptivePolicyPreservesNormalAndStability(t *testing.T) {
	d := routingDoc(t, adaptiveContext)
	p, err := FromV1Alpha1(&d)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(p.Rules[0].Routing.Context)
	var got map[string]any
	_ = json.Unmarshal(b, &got)
	if got["NormalModel"] != "normal" || got["MaxNormalInputTokens"] != float64(1000) {
		t.Fatalf("normal class lost: %s", b)
	}
	stability, ok := got["Stability"].(map[string]any)
	if !ok || stability["MinHold"] != float64(300000000000) || stability["MinRequests"] != float64(3) || stability["SessionTTL"] != float64(1800000000000) {
		t.Fatalf("stability defaults lost: %s", b)
	}
}

func TestAdaptivePolicyRejectsInvalidBounds(t *testing.T) {
	for _, raw := range []string{
		strings.Replace(adaptiveContext, `"maxNormalInputTokens":1000`, `"maxNormalInputTokens":100`, 1),
		strings.Replace(adaptiveContext, `"normalModel":"normal",`, "", 1),
		strings.Replace(adaptiveContext, `"normalModel":"normal"`, `"normalModel":"*"`, 1),
		strings.Replace(adaptiveContext, `"stability":{}`, `"stability":{"minHold":"-1s"}`, 1),
		strings.Replace(adaptiveContext, `"stability":{}`, `"stability":{"minHold":"31m","sessionTTL":"30m"}`, 1),
		strings.Replace(adaptiveContext, `"stability":{}`, `"stability":{"sessionTTL":"0s"}`, 1),
		strings.Replace(adaptiveContext, `"stability":{}`, `"stability":{"sessionTTL":"25h"}`, 1),
		strings.Replace(adaptiveContext, `"stability":{}`, `"stability":{"minRequests":-1}`, 1),
		strings.Replace(adaptiveContext, `"stability":{}`, `"stability":{"minRequests":10001}`, 1),
	} {
		d := routingDoc(t, raw)
		if _, err := FromV1Alpha1(&d); err == nil {
			t.Errorf("accepted invalid adaptive policy: %s", raw)
		}
	}
}

func TestAdaptivePolicyNormalTargetValidated(t *testing.T) {
	s := NewEmptyStore()
	s.SetRoutedAndPriced(func(name string) error {
		if name == "normal" {
			return errors.New("unavailable")
		}
		return nil
	})
	if r := s.ApplyWire([]v1alpha1.GovernancePolicy{routingDoc(t, adaptiveContext)}); len(r) != 1 {
		t.Fatalf("normal target was not validated: %v", r)
	}
}

func TestAdaptivePolicyMaskDetectedOnly(t *testing.T) {
	for _, tt := range []struct {
		detected, opaque string
		valid            bool
	}{
		{"Mask", "Block", true}, {"Mask", "InternalOnly", true},
		{"Mask", "Mask", false}, {"Block", "Mask", false},
	} {
		d := routingDoc(t, `[{"name":"p","failurePolicy":"FailClosed","sensitiveData":{"onDetected":"`+tt.detected+`","onUninspectable":"`+tt.opaque+`","internalModels":["private"]}}]`)
		_, err := FromV1Alpha1(&d)
		if (err == nil) != tt.valid {
			t.Errorf("%s/%s: %v", tt.detected, tt.opaque, err)
		}
	}
}

func TestAdaptivePolicyStrictThreshold(t *testing.T) {
	for _, tt := range []struct {
		strict    bool
		threshold int
		valid     bool
	}{
		{false, 99, true}, {false, 100, false}, {true, 100, true}, {true, 101, false}, {true, 0, false},
	} {
		d := routingDoc(t, `[{"name":"b","failurePolicy":"FailOpen","budget":{"limitMilliUSD":100}},{"name":"r","failurePolicy":"FailOpen","routing":{"budgetTiers":{"budgetRef":"b","enforceTargets":true,"tiers":[{"thresholdPercent":100,"substitute":{"premium":"cheap"}}]}}}]`)
		// Set through JSON so this test also catches omitted wire fields.
		var raw map[string]any
		b, _ := json.Marshal(d)
		_ = json.Unmarshal(b, &raw)
		bt := raw["spec"].(map[string]any)["rules"].([]any)[1].(map[string]any)["routing"].(map[string]any)["budgetTiers"].(map[string]any)
		bt["enforceTargets"] = tt.strict
		bt["tiers"].([]any)[0].(map[string]any)["thresholdPercent"] = tt.threshold
		b, _ = json.Marshal(raw)
		_ = json.Unmarshal(b, &d)
		p, err := FromV1Alpha1(&d)
		if (err == nil) != tt.valid {
			t.Errorf("strict=%v threshold=%d: %v", tt.strict, tt.threshold, err)
		}
		if err == nil && tt.strict {
			b, _ := json.Marshal(p.Rules[1].Routing.BudgetTiers)
			if !strings.Contains(string(b), `"EnforceTargets":true`) {
				t.Errorf("strict constraint lost: %s", b)
			}
		}
	}
}

func TestAdaptivePolicySnapshotOwnsStability(t *testing.T) {
	s := NewEmptyStore()
	d := routingDoc(t, adaptiveContext)
	if rejected := s.ApplyWire([]v1alpha1.GovernancePolicy{d}); len(rejected) != 0 {
		t.Fatal(rejected)
	}
	first, err := s.MatchingRoutingPolicies("eng", "")
	if err != nil {
		t.Fatal(err)
	}
	first[0].Rules[0].Routing.Context.Stability.MinRequests = 999
	second, _ := s.MatchingRoutingPolicies("eng", "")
	if second[0].Rules[0].Routing.Context.Stability.MinRequests != 3 {
		t.Fatal("caller changed policy snapshot stability")
	}
}

func TestAdaptiveActiveTierWireRetainsStrictFlag(t *testing.T) {
	a := ActiveTier{EnforceTargets: true}
	b, _ := json.Marshal(a)
	var decoded ActiveTier
	if err := json.Unmarshal(b, &decoded); err != nil || !decoded.EnforceTargets {
		t.Fatalf("strict sync flag lost: %s, %v", b, err)
	}
	b, _ = json.Marshal(ActiveTier{})
	if strings.Contains(string(b), "enforceTargets") {
		t.Fatal("legacy active tier acquired a new wire field")
	}
}
