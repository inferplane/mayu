package pgstore

import (
	"encoding/json"
	"testing"

	"github.com/inferplane/inferplane/internal/policy"
	"github.com/inferplane/inferplane/internal/tier"
)

func TestActiveTiersPreserveUserSubjects(t *testing.T) {
	for _, team := range []string{"", "alpha"} {
		t.Run("team="+team, func(t *testing.T) {
			dsn, db := testDatabase(t)
			putBudget(t, db, 100, 10, false, true)
			s := openStore(t, dsn)
			doc := syncOK(t, s, "seed", heartbeat("boot")).Policies[0]
			doc.Spec.Subject.Team, doc.Spec.Subject.User = team, "alice"
			doc.Spec.Rules[1].Routing.BudgetTiers.EnforceTargets = false
			putDocument(t, db, doc)
			b := currentBudget(t, s)
			req := heartbeat("boot")
			req.Meters = []policy.AuthorityMeter{{
				Instance: "boot", Key: b.Key, WindowID: b.WindowID,
				Sequence: 1, Consumed: 60_000, Observed: 60_000,
			}}
			resp := syncOK(t, s, "node", req)
			if len(resp.ActiveTiers) != 1 {
				t.Fatal("user budget did not activate its tier")
			}
			encoded, err := json.Marshal(resp.ActiveTiers[0])
			if err != nil {
				t.Fatal(err)
			}
			var wire map[string]any
			if err := json.Unmarshal(encoded, &wire); err != nil {
				t.Fatal(err)
			}
			if wire["team"] != team || wire["user"] != "alice" {
				t.Fatalf("tier wire response lost subject selectors: %s", encoded)
			}
			if resp.ActiveTiers[0].ThresholdPercent != 50 {
				t.Fatal("user-scoped utilization activated the wrong tier")
			}
			var decoded policy.ActiveTier
			if err := json.Unmarshal(encoded, &decoded); err != nil {
				t.Fatal(err)
			}
			table := tier.NewTable()
			table.Set([]policy.ActiveTier{decoded})
			if table.GetForSubject("alpha", "alice")["expensive"] != "cheap" {
				t.Fatal("matching user did not receive the distributed substitution")
			}
			if table.GetForSubject("alpha", "bob") != nil || table.Get("alpha") != nil {
				t.Fatal("distributed user tier affected another user or the entire team")
			}
			if got := table.GetForSubject("beta", "alice"); (team == "" && got["expensive"] != "cheap") ||
				(team != "" && got != nil) {
				t.Fatal("distributed tier did not preserve its cross-team user scope")
			}
		})
	}
}
