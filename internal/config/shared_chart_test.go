package config

import (
	"os/exec"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestSharedChartEnforcesProfileAndPrivateReplicaStorage(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm unavailable; operator chart validation requires helm")
	}
	for _, args := range [][]string{
		{"--set", "replicaCount=2"},
		{"--set", "replicaCount=0"},
		{"-f", "../../examples/helm.shared-governance.yaml", "--set", "persistence.existingClaim=shared-wal"},
		{"-f", "../../examples/helm.shared-governance.yaml", "--set", "config.control_plane.require_sync=false"},
	} {
		cmd := exec.Command(helm, append([]string{"template", "test", "../../charts/inferplane"}, args...)...)
		if out, err := cmd.CombinedOutput(); err == nil {
			t.Fatalf("unsafe chart profile rendered: %v\n%s", args, out)
		}
	}
	out, err := exec.Command(helm, "template", "test", "../../charts/inferplane", "-f", "../../examples/helm.shared-governance.yaml").CombinedOutput()
	if err != nil {
		t.Fatalf("shared chart failed: %s", out)
	}
	stateful, disruption := false, false
	for _, doc := range strings.Split(string(out), "\n---") {
		var object map[string]any
		if err := yaml.Unmarshal([]byte(doc), &object); err != nil {
			t.Fatal(err)
		}
		switch object["kind"] {
		case "StatefulSet":
			stateful = true
			spec := object["spec"].(map[string]any)
			if spec["replicas"] != float64(2) || len(spec["volumeClaimTemplates"].([]any)) != 1 {
				t.Fatal("replicas lack separate claim templates")
			}
			pod := spec["template"].(map[string]any)["spec"].(map[string]any)
			if pod["affinity"] == nil {
				t.Fatal("shared replicas can be scheduled on one node")
			}
			for _, volume := range pod["volumes"].([]any) {
				if volume.(map[string]any)["name"] == "data" {
					t.Fatal("replicas share one data volume")
				}
			}
		case "PodDisruptionBudget":
			disruption = true
		case "PersistentVolumeClaim":
			t.Fatal("shared mode rendered one standalone claim for all replicas")
		}
	}
	if !stateful || !disruption {
		t.Fatal("shared persistent deployment is missing HA resources")
	}
}
