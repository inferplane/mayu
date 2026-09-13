package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	budgetpg "github.com/inferplane/inferplane/internal/authority/pgstore"
	"github.com/inferplane/inferplane/internal/controlplane"
	"github.com/inferplane/inferplane/internal/policy"
	"github.com/inferplane/inferplane/internal/policystore"
)

type replaceableAuthority struct {
	current atomic.Pointer[budgetpg.Store]
}

func (a *replaceableAuthority) Sync(ctx context.Context, node string, r policy.AuthorityRequest) (policy.SyncResponse, error) {
	return a.current.Load().Sync(ctx, node, r)
}

func TestDurableTwoGatewaysTwoControlPlanesShareOneBudget(t *testing.T) {
	dsn := isolatedPostgresDSN(t)
	t.Setenv("DURABLE_LIVE_TOKEN", "local-budget-machine")
	t.Setenv("E2E_UPSTREAM_KEY", e2eUpstreamKey)
	seed := filepath.Join(t.TempDir(), "policy.yaml")
	if err := os.WriteFile(seed, []byte(`apiVersion: inferplane.dev/v1alpha1
kind: GovernancePolicy
metadata: {name: fleet, generation: 1}
spec:
  subject: {team: fleet}
  rules:
    - name: cap
      failurePolicy: FailClosed
      budget:
        limitMilliUSD: 4
        hardCap: true
        lease: {grantMilliUSD: 1, renewInterval: 1s}
`), 0600); err != nil {
		t.Fatal(err)
	}
	newLedger := func() *budgetpg.Store {
		s, err := budgetpg.New(dsn)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Initialize(context.Background()); err != nil {
			s.Close()
			t.Fatal(err)
		}
		t.Cleanup(s.Close)
		return s
	}
	var endpoints []string
	var backends []*replaceableAuthority
	for i := 0; i < 2; i++ {
		cp, err := controlplane.NewServer("local-budget-machine", seed)
		if err != nil {
			t.Fatal(err)
		}
		ps, err := policystore.NewPostgres(dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(ps.Close)
		if err := cp.AttachPolicyStore(context.Background(), ps); err != nil {
			t.Fatal(err)
		}
		backend := &replaceableAuthority{}
		backend.current.Store(newLedger())
		cp.SetBudgetAuthority(backend)
		mux := http.NewServeMux()
		cp.Mount(mux)
		server := httptest.NewServer(mux)
		t.Cleanup(server.Close)
		endpoints = append(endpoints, server.URL)
		backends = append(backends, backend)
	}
	var upstreamCalls atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"m","type":"message","role":"assistant","model":"claude-test","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":40,"output_tokens":10}}`)
	}))
	defer up.Close()
	var urls, keys []string
	for i := 0; i < 2; i++ {
		i := i
		data, admin, _ := bootGateway(t, func(cfg map[string]any, dir string) {
			withAnthropicProvider(up.URL)(cfg, dir)
			cfg["analytics"] = map[string]any{"disabled": true}
			cfg["models"].(map[string]any)["claude-test"].(map[string]any)["context_window"] = 100
			cfg["pricing"] = map[string]any{"overrides": map[string]any{"up": map[string]any{"claude-test": map[string]any{"input_per_mtok": 1, "output_per_mtok": 1}}}}
			cfg["control_plane"] = map[string]any{"url": endpoints[i], "dataplane": fmt.Sprintf("node-%d", i), "require_sync": true,
				"token_ref": map[string]any{"env": "DURABLE_LIVE_TOKEN"}, "authority": map[string]any{"journal_path": filepath.Join(dir, "authority.sqlite")}}
		})
		_, key := createKey(t, admin, "fleet", []string{"claude-test"})
		urls = append(urls, data)
		keys = append(keys, key)
	}
	success := []int{0, 0}
	denied := []bool{false, false}
	restarted := false
	deadline := time.Now().Add(15 * time.Second)
	for i := 0; time.Now().Before(deadline); i++ {
		node := i % 2
		resp := postMessages(t, urls[node], keys[node], "claude-test")
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		switch resp.StatusCode {
		case 200:
			success[node]++
			denied[node] = false
		case 402:
			denied[node] = true
		case 503:
			time.Sleep(10 * time.Millisecond)
		default:
			t.Fatalf("unexpected status=%d body=%s", resp.StatusCode, body)
		}
		if !restarted && success[0]+success[1] >= 10 {
			old := backends[0].current.Swap(newLedger())
			old.Close()
			restarted = true
		}
		if denied[0] && denied[1] && success[0] > 0 && success[1] > 0 {
			break
		}
	}
	calls := upstreamCalls.Load()
	if !restarted || success[0] == 0 || success[1] == 0 || !denied[0] || !denied[1] || calls*50 > 4000 {
		t.Fatalf("global budget violated or not enforced: success=%v denied=%v restart=%v calls=%d spent=%d", success, denied, restarted, calls, calls*50)
	}
}
