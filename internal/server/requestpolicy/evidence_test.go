package requestpolicy

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/inferplane/inferplane/internal/audit"
	"github.com/inferplane/inferplane/internal/router"
	"github.com/inferplane/inferplane/providers"
)

type marshalTrap struct{ providers.Provider }

func (marshalTrap) MarshalJSON() ([]byte, error) { panic("provider object must never be marshaled") }

func TestEvidenceProjectionNeverMarshalsProvider(t *testing.T) {
	result := router.RequestRoutingResult{
		Decision: router.RoutingDecision{RequestedModel: "premium", SelectedModel: "premium", Mode: "Shadow", Reason: "context_shadow", Inspection: "complete",
			Policies:   []router.RoutingPolicyRef{{Name: "selection", Generation: 2, Rule: "short"}},
			Categories: []string{"email"}},
		Chain: []router.ChainTarget{{Provider: marshalTrap{}, ProviderName: "configured", Identity: "not-for-audit", DataBoundary: ""}},
	}
	req := Observe(httptest.NewRecorder(), httptest.NewRequest("POST", "/", nil), result, nil)
	ref := audit.RoutingFrom(req.Context())
	body, err := json.Marshal(ref)
	if err != nil || strings.Contains(string(body), "not-for-audit") || !strings.Contains(string(body), `"planned_provider":"configured"`) {
		t.Fatalf("unsafe projection: %s %v", body, err)
	}
	if ref.PlannedBoundary != "unknown" {
		t.Fatalf("unattested boundary must be explicitly unknown: %+v", ref)
	}
	result.Decision.Categories[0] = "mutated"
	if audit.RoutingFrom(req.Context()).Categories[0] != "email" {
		t.Fatal("projection retains result slice")
	}
	actual := audit.WithRoutingAttempt(req.Context(), "premium", "configured", "")
	if audit.RoutingFrom(actual).ActualBoundary != "unknown" {
		t.Fatal("unknown actual boundary omitted")
	}
}
