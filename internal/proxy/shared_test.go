package proxy

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/inferplane/inferplane/api/v1alpha1"
	"github.com/inferplane/inferplane/internal/policy"
)

func TestSharedClientRequiresSameAuthorityAndPolicyWithoutIssuingGrants(t *testing.T) {
	var fail bool
	gen := policy.GenerationOf(nil)
	client, err := NewSharedAuthorityClient(func(context.Context) (string, string, error) {
		if fail {
			return "", "", errors.New("unavailable")
		}
		return "same-database", gen, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	req, err := client.Request(context.Background())
	if err != nil || req.Instance == "" || len(req.Requests)+len(req.Reports)+len(req.Meters) != 0 {
		t.Fatal("shared profile requested local authority")
	}
	bundle := policy.AuthorityResponse{Protocol: policy.AuthorityProtocol, AuthorityID: "same-database", ServerTime: time.Now().UTC(),
		Generation: gen, Policies: []v1alpha1.GovernancePolicy{}, Budgets: []policy.AuthorityBudget{}}
	if err := client.Apply(context.Background(), bundle, 0); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"", "other-database"} {
		bad := bundle
		bad.AuthorityID = id
		if err := client.Apply(context.Background(), bad, 0); err == nil {
			t.Fatal("unbound monetary authority accepted")
		}
	}
	fail = true
	if err := client.Apply(context.Background(), bundle, 0); err == nil {
		t.Fatal("failed binding read accepted")
	}
}
