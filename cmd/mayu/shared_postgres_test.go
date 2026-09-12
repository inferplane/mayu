package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	budgetpg "github.com/inferplane/inferplane/internal/authority/pgstore"
	"github.com/inferplane/inferplane/internal/controlplane"
	"github.com/inferplane/inferplane/internal/governance"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/policy"
	"github.com/inferplane/inferplane/internal/policystore"
)

func sharedControlPlane(t *testing.T, dsn, rules string) *httptest.Server {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy.yaml")
	doc := `apiVersion: inferplane.dev/v1alpha1
kind: GovernancePolicy
metadata: {name: shared-test, generation: 1}
spec:
  subject: {team: fleet}
  rules:
    - name: meter
      failurePolicy: FailOpen
      budget: {limitMilliUSD: 1000000, lease: {renewInterval: 1s}}
` + rules
	if err := os.WriteFile(path, []byte(doc), 0600); err != nil {
		t.Fatal(err)
	}
	cp, err := controlplane.NewServer("shared-test-control-token", path)
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
	ledger, err := budgetpg.New(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ledger.Close)
	if err := ledger.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	cp.SetBudgetAuthority(ledger)
	mux := http.NewServeMux()
	cp.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

type sharedTestGateway struct {
	g           *gateway
	path        string
	data, admin string
	stop        func()
}

func bootSharedPath(t *testing.T, path string) sharedTestGateway {
	t.Helper()
	g, err := newGateway(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- g.serve(ctx) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			if err := <-done; err != nil {
				t.Error(err)
			}
		})
	}
	t.Cleanup(stop)
	return sharedTestGateway{g: g, path: path, data: "http://" + g.DataAddr(), admin: "http://" + g.AdminAddr(), stop: stop}
}

func sharedGateway(t *testing.T, dsn, cp, upstream string, team map[string]any) sharedTestGateway {
	t.Helper()
	t.Setenv("SHARED_E2E_DSN", dsn)
	t.Setenv("SHARED_E2E_CONTROL", "shared-test-control-token")
	t.Setenv("E2E_UPSTREAM_KEY", e2eUpstreamKey)
	t.Setenv("E2E_ADMIN_TOKEN", e2eAdminToken)
	path := writeTestConfig(t, func(cfg map[string]any, dir string) {
		withAnthropicProvider(upstream)(cfg, dir)
		cfg["models"].(map[string]any)["claude-test"].(map[string]any)["context_window"] = 100
		cfg["server"].(map[string]any)["admin_auth"] = map[string]any{"token_refs": []any{map[string]any{"env": "E2E_ADMIN_TOKEN"}}}
		cfg["key_store"] = map[string]any{"type": "postgres", "dsn_ref": map[string]any{"env": "SHARED_E2E_DSN"}}
		cfg["governance_store"] = map[string]any{"type": "postgres"}
		cfg["control_plane"] = map[string]any{"url": cp, "require_sync": true, "token_ref": map[string]any{"env": "SHARED_E2E_CONTROL"}}
		cfg["teams"] = map[string]any{"fleet": team}
		cfg["analytics"] = map[string]any{"disabled": true}
	})
	return bootSharedPath(t, path)
}

func sharedResponseStatus(t *testing.T, node sharedTestGateway, key string) int {
	t.Helper()
	resp := postMessages(t, node.data, key, "claude-test")
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode >= 500 {
		t.Logf("shared response status=%d body=%s", resp.StatusCode, b)
	}
	return resp.StatusCode
}

func sharedReady(t *testing.T, nodes ...sharedTestGateway) {
	t.Helper()
	for _, node := range nodes {
		waitHTTP(t, node.admin+"/readyz", 200)
	}
}

func TestSharedGatewaysKeysAndRPMRemainGlobalAcrossRestart(t *testing.T) {
	dsn := isolatedPostgresDSN(t)
	cp := sharedControlPlane(t, dsn, "")
	up := newAnthropicUpstream(t)
	team := map[string]any{"allowed_models": []string{"claude-test"}, "rate_limit": map[string]any{"requests_per_minute": 2}}
	a := sharedGateway(t, dsn, cp.URL, up.srv.URL, team)
	b := sharedGateway(t, dsn, cp.URL, up.srv.URL, team)
	sharedReady(t, a, b)
	id, key := createKey(t, a.admin, "fleet", []string{"claude-test"})
	for i, node := range []sharedTestGateway{a, b, a, b} {
		want := 429
		if i < 2 {
			want = 200
		}
		if got := sharedResponseStatus(t, node, key); got != want {
			t.Fatalf("request %d global RPM: got %d want %d", i, got, want)
		}
	}
	a.stop()
	a = bootSharedPath(t, a.path)
	sharedReady(t, a)
	if got := sharedResponseStatus(t, a, key); got != 429 {
		t.Fatalf("restart minted request capacity: %d", got)
	}
	req, _ := http.NewRequest(http.MethodDelete, b.admin+"/admin/keys/"+id, nil)
	req.Header.Set("Authorization", "Bearer "+e2eAdminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatal(resp.StatusCode)
	}
	if got := sharedResponseStatus(t, a, key); got != 401 {
		t.Fatalf("revocation did not reach peer: %d", got)
	}
}

func TestSharedGatewaysCalendarQuotaAndUsageAreGlobal(t *testing.T) {
	dsn := isolatedPostgresDSN(t)
	cp := sharedControlPlane(t, dsn, `    - name: tokens
      failurePolicy: FailClosed
      tokenQuota: {limitTokens: 520, period: CalendarDay}
`)
	up := newAnthropicUpstream(t)
	team := map[string]any{"allowed_models": []string{"claude-test"}}
	a := sharedGateway(t, dsn, cp.URL, up.srv.URL, team)
	b := sharedGateway(t, dsn, cp.URL, up.srv.URL, team)
	sharedReady(t, a, b)
	_, key := createKey(t, a.admin, "fleet", []string{"claude-test"})
	for i, node := range []sharedTestGateway{a, b, a, b} {
		want := 429
		if i < 2 {
			want = 200 // each needs a 500-token bound and settles 15 tokens
		}
		if got := sharedResponseStatus(t, node, key); got != want {
			t.Fatalf("request %d global quota: got %d want %d", i, got, want)
		}
	}
	req, _ := http.NewRequest(http.MethodGet, b.data+"/v1/usage", nil)
	req.Header.Set("x-api-key", key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var status governance.UsageStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil || status.EnforcementMode != "shared" {
		t.Fatalf("shared usage missing: %+v %v", status, err)
	}
	found := false
	for _, limit := range status.SharedLimits {
		if limit.Kind == "tokens" && limit.Rule == "tokens" {
			found = true
			if limit.Used != 30 || limit.Reserved != 0 || limit.Remaining != 490 {
				t.Fatalf("peer reported local or incorrect usage: %+v", limit)
			}
		}
	}
	if !found {
		t.Fatal("quota absent from global usage")
	}
}

func TestSharedGatewayOutageCountsAndImmutableBootstrap(t *testing.T) {
	dsn := isolatedPostgresDSN(t)
	cp := sharedControlPlane(t, dsn, "")
	up := newAnthropicUpstream(t)
	team := map[string]any{"allowed_models": []string{"claude-test"}}
	node := sharedGateway(t, dsn, cp.URL, up.srv.URL, team)
	sharedReady(t, node)
	_, key := createKey(t, node.admin, "fleet", []string{"claude-test"})
	pg := node.g.store.(*keystore.PostgresStore)
	record, _, err := pg.GetTeam(context.Background(), "fleet")
	if err != nil {
		t.Fatal(err)
	}
	record.RPM = 1
	if err := pg.UpsertTeam(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	node.stop()
	node = bootSharedPath(t, node.path)
	sharedReady(t, node)
	record, _, err = node.g.store.(keystore.TeamStore).GetTeam(context.Background(), "fleet")
	if err != nil || record.RPM != 1 {
		t.Fatal("unchanged seed overwrote admin limits or prevented restart")
	}
	cp.Close() // no central HTTP call is needed by a previously synchronized request
	if got := sharedResponseStatus(t, node, key); got != 200 {
		t.Fatalf("control-plane process became an inference dependency: %d", got)
	}
	_ = node.g.store.Close()
	if got := sharedResponseStatus(t, node, key); got != 503 {
		t.Fatalf("identity storage failure did not fail closed: %d", got)
	}
	up.reset()
	body := []byte(`{"model":"claude-test","messages":[{"role":"user","content":"local"}]}`)
	req, _ := http.NewRequest(http.MethodPost, node.data+"/v1/messages/count_tokens", bytes.NewReader(body))
	req.Header.Set("x-api-key", key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || up.apiKey() != "" {
		t.Fatal("outage count path failed or contacted upstream")
	}
}

func TestSharedAuthorityBindingRejectsSeparateEqualPolicyDatabases(t *testing.T) {
	first, second := isolatedPostgresDSN(t), isolatedPostgresDSN(t)
	sharedControlPlane(t, first, "")
	other := sharedControlPlane(t, second, "")
	up := newAnthropicUpstream(t)
	node := sharedGateway(t, first, other.URL, up.srv.URL, map[string]any{"allowed_models": []string{"claude-test"}})
	_, key := createKey(t, node.admin, "fleet", []string{"claude-test"})
	wire, _ := json.Marshal(policy.SyncRequest{Dataplane: "binding-probe", APIVersions: policy.SupportedAPIVersions,
		Authority: &policy.AuthorityRequest{Protocol: policy.AuthorityProtocol, Instance: "binding-probe"}})
	req, _ := http.NewRequest(http.MethodPost, other.URL+"/v1alpha1/sync", bytes.NewReader(wire))
	req.Header.Set("Authorization", "Bearer shared-test-control-token")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var bundle policy.SyncResponse
	err = json.NewDecoder(response.Body).Decode(&bundle)
	response.Body.Close()
	if err != nil || bundle.Authority == nil {
		t.Fatal("probe did not receive a real authority bundle")
	}
	if err := node.g.syncer.Authority.Apply(context.Background(), *bundle.Authority, 0); err == nil {
		t.Fatal("separate databases passed the real binding check")
	}
	if got := sharedResponseStatus(t, node, key); got != 503 {
		t.Fatalf("separate equal-sized authority accepted: %d", got)
	}
	resp, err := http.Get(node.admin + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Fatal(fmt.Sprintf("unbound authority became ready: %d", resp.StatusCode))
	}
}
