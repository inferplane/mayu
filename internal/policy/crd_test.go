package policy

import (
	"os"
	"strings"
	"testing"

	sigyaml "sigs.k8s.io/yaml"

	v1alpha1 "github.com/inferplane/inferplane/api/v1alpha1"
)

// The shipped CRD manifest must stay in lockstep with the Go schema: same
// group/version as v1alpha1.APIVersion, GovernancePolicy kind, a served+
// stored v1alpha1, and the structural schema present. Field-level drift is
// caught in-cluster by kubectl validation; this guards the identity from
// rotting in the repo.
func TestCRDManifestMatchesAPIVersion(t *testing.T) {
	data, err := os.ReadFile("../../deploy/crd/inferplane.dev_governancepolicies.yaml")
	if err != nil {
		t.Fatalf("CRD manifest: %v", err)
	}
	var crd struct {
		Kind string `json:"kind"`
		Spec struct {
			Group string `json:"group"`
			Names struct {
				Kind string `json:"kind"`
			} `json:"names"`
			Versions []struct {
				Name    string `json:"name"`
				Served  bool   `json:"served"`
				Storage bool   `json:"storage"`
				Schema  struct {
					OpenAPIV3Schema map[string]any `json:"openAPIV3Schema"`
				} `json:"schema"`
			} `json:"versions"`
		} `json:"spec"`
	}
	if err := sigyaml.Unmarshal(data, &crd); err != nil {
		t.Fatalf("CRD manifest is not valid YAML: %v", err)
	}
	if crd.Kind != "CustomResourceDefinition" || crd.Spec.Names.Kind != v1alpha1.KindGovernancePolicy {
		t.Fatalf("CRD identity mangled: kind=%s names.kind=%s", crd.Kind, crd.Spec.Names.Kind)
	}
	if len(crd.Spec.Versions) != 1 || !crd.Spec.Versions[0].Served || !crd.Spec.Versions[0].Storage {
		t.Fatalf("CRD must serve+store exactly one version: %+v", crd.Spec.Versions)
	}
	gv := crd.Spec.Group + "/" + crd.Spec.Versions[0].Name
	if gv != v1alpha1.APIVersion {
		t.Fatalf("CRD group/version %q != v1alpha1.APIVersion %q", gv, v1alpha1.APIVersion)
	}
	if crd.Spec.Versions[0].Schema.OpenAPIV3Schema == nil {
		t.Fatal("CRD has no structural schema")
	}
	// The wire fields the Go schema requires must appear in the manifest —
	// a cheap tripwire against renaming one side only.
	for _, field := range []string{"failurePolicy", "limitMilliUSD", "grantMilliUSD", "renewInterval", "onAffinityConflict", "modelAccess", "unlimited", "period", "budgetTiers", "budgetRef", "thresholdPercent", "substitute"} {
		if !strings.Contains(string(data), field) {
			t.Fatalf("CRD manifest lost field %q", field)
		}
	}
}

// Unknown CRD properties are pruned by Kubernetes. Every new wire field must
// therefore have a structural schema, including required actions and mode enum.
func TestRoutingPolicyCRDSchema(t *testing.T) {
	data, err := os.ReadFile("../../deploy/crd/inferplane.dev_governancepolicies.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var crd map[string]any
	if err := sigyaml.Unmarshal(data, &crd); err != nil {
		t.Fatal(err)
	}
	at := func(m map[string]any, keys ...string) map[string]any {
		t.Helper()
		for _, k := range keys {
			next, ok := m[k].(map[string]any)
			if !ok {
				t.Fatalf("schema missing %s", k)
			}
			m = next
		}
		return m
	}
	spec := at(crd, "spec")
	version := spec["versions"].([]any)[0].(map[string]any)
	rule := at(version, "schema", "openAPIV3Schema", "properties", "spec", "properties", "rules", "items", "properties")
	sensitive := at(rule, "sensitiveData")
	props := at(sensitive, "properties")
	for _, field := range []string{"onDetected", "onUninspectable"} {
		action := at(props, field)
		enum := action["enum"].([]any)
		if len(enum) != 2 || enum[0] != "InternalOnly" || enum[1] != "Block" {
			t.Fatalf("unbounded action %s: %v", field, enum)
		}
	}
	required := sensitive["required"].([]any)
	if len(required) != 2 || required[0] != "onDetected" || required[1] != "onUninspectable" {
		t.Fatalf("actions not required: %v", required)
	}
	c := at(rule, "routing", "properties", "context")
	cp := at(c, "properties")
	if mode := at(cp, "mode"); mode["default"] != "Shadow" {
		t.Fatal("context default not Shadow")
	}
	for _, field := range []string{"fromModels", "simpleModel", "complexModel", "maxSimpleInputTokens", "complexKeywords"} {
		at(cp, field)
	}
	if at(cp, "maxSimpleInputTokens")["minimum"] != float64(1) {
		t.Fatal("nonpositive threshold allowed")
	}
}
