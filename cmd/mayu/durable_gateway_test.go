package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v1alpha1 "github.com/inferplane/inferplane/api/v1alpha1"
	"github.com/inferplane/inferplane/internal/governance"
	"github.com/inferplane/inferplane/internal/policy"
	"github.com/inferplane/inferplane/internal/providerstore"
	"github.com/inferplane/inferplane/providers"
	"github.com/inferplane/inferplane/providers/testing/mockprovider"
)

func durableGatewayConfig(t *testing.T, cpURL string, mutate func(map[string]any, string)) string {
	t.Helper()
	t.Setenv("DURABLE_TEST_CP_TOKEN", "durable-test-control-token")
	t.Setenv("E2E_UPSTREAM_KEY", e2eUpstreamKey)
	t.Setenv("E2E_ADMIN_TOKEN", e2eAdminToken)
	return writeTestConfig(t, func(cfg map[string]any, dir string) {
		withAnthropicProvider("http://127.0.0.1:1")(cfg, dir)
		cfg["models"].(map[string]any)["claude-test"].(map[string]any)["context_window"] = 4096
		cfg["server"].(map[string]any)["admin_auth"] = map[string]any{
			"token_refs": []any{map[string]string{"env": "E2E_ADMIN_TOKEN"}},
		}
		cfg["analytics"] = map[string]any{"disabled": true}
		cfg["teams"] = map[string]any{"durable-team": map[string]any{"allowed_models": []string{"*"}}}
		cfg["control_plane"] = map[string]any{
			"url": cpURL, "dataplane": "durable-node", "require_sync": true,
			"token_ref": map[string]string{"env": "DURABLE_TEST_CP_TOKEN"},
			"authority": map[string]any{"journal_path": filepath.Join(dir, "authority.sqlite")},
		}
		if mutate != nil {
			mutate(cfg, dir)
		}
	})
}

func durableRewrite(t *testing.T, path string, mutate func(map[string]any)) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	mutate(cfg)
	b, err = json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
}

func durableUnservedCleanup(t *testing.T, g *gateway) {
	t.Helper()
	t.Cleanup(func() {
		_ = g.dataLn.Close()
		_ = g.adminLn.Close()
		if g.syncer != nil && g.syncer.Authority != nil {
			if c, ok := g.syncer.Authority.(io.Closer); ok {
				_ = c.Close()
			}
		}
		closeAll(g.pstore, g.pgstoreQ, g.store, g.aud)
	})
}

func TestDurableGatewayOpensPrivateJournalAndLegacyDoesNot(t *testing.T) {
	path := durableGatewayConfig(t, "http://127.0.0.1:1", nil)
	g, err := newGateway(path)
	if err != nil {
		t.Fatal(err)
	}
	durableUnservedCleanup(t, g)
	if g.syncer.Authority == nil {
		t.Fatal("durable syncer did not receive the local journal")
	}
	if g.syncer.Leases != nil {
		t.Fatal("durable mode still installs legacy lease clamps")
	}
	authority, ok := g.syncer.Authority.(governance.BudgetAuthority)
	if !ok {
		t.Fatal("syncer and admission do not share a BudgetAuthority")
	}
	if _, err := authority.Reserve(context.Background(), governance.Subject{Team: "durable-team"}, 1); !errors.Is(err, governance.ErrAuthorityUnavailable) {
		t.Fatalf("uninitialized journal admitted: %v", err)
	}
	fi, err := os.Stat(filepath.Join(filepath.Dir(path), "authority.sqlite"))
	if err != nil || fi.Mode().Perm() != 0600 {
		t.Fatalf("private persistent journal: %v %v", fi, err)
	}
	legacyPath := durableGatewayConfig(t, "http://127.0.0.1:1", func(cfg map[string]any, _ string) {
		delete(cfg["control_plane"].(map[string]any), "authority")
	})
	legacy, err := newGateway(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	durableUnservedCleanup(t, legacy)
	if legacy.syncer.Authority != nil || legacy.syncer.Leases == nil {
		t.Fatal("legacy gateway acquired durable authority or lost its lease table")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(legacyPath), "authority.sqlite")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy boot opened a journal: %v", err)
	}
}

func TestDurableGatewayRejectsMissingContextIncludingFreeModels(t *testing.T) {
	for _, free := range []bool{false, true} {
		t.Run(fmt.Sprintf("free=%v", free), func(t *testing.T) {
			path := durableGatewayConfig(t, "http://127.0.0.1:1", func(cfg map[string]any, _ string) {
				delete(cfg["models"].(map[string]any)["claude-test"].(map[string]any), "context_window")
				if free {
					cfg["pricing"] = map[string]any{"overrides": map[string]any{"up": map[string]any{
						"claude-test": map[string]any{"free": true},
					}}}
				}
			})
			g, err := newGateway(path)
			if err == nil {
				durableUnservedCleanup(t, g)
				t.Fatal("durable profile admitted a route without a context bound")
			}
			if !strings.Contains(err.Error(), "context_window") {
				t.Fatalf("wrong metadata error: %v", err)
			}
		})
	}
}

func TestDurableGatewayRejectsPublicJournal(t *testing.T) {
	path := durableGatewayConfig(t, "http://127.0.0.1:1", func(_ map[string]any, dir string) {
		if err := os.WriteFile(filepath.Join(dir, "authority.sqlite"), nil, 0644); err != nil {
			t.Fatal(err)
		}
	})
	if g, err := newGateway(path); err == nil {
		durableUnservedCleanup(t, g)
		t.Fatal("gateway accepted an insecure journal path")
	}
}

func TestDurableGatewayBootFailureClosesJournal(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	path := durableGatewayConfig(t, "http://127.0.0.1:1", func(cfg map[string]any, _ string) {
		cfg["server"].(map[string]any)["listen"] = busy.Addr().String()
	})
	if _, err := newGateway(path); err == nil {
		t.Fatal("expected occupied-listener boot failure")
	}
	journalPath := filepath.Join(filepath.Dir(path), "authority.sqlite")
	if _, err := os.Stat(journalPath); err != nil {
		t.Fatalf("test did not reach journal acquisition: %v", err)
	}
	// A surviving connection keeps the WAL live. Last-connection close
	// checkpoints and removes it, proving this boot failure released ownership.
	if _, err := os.Stat(journalPath + "-wal"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("boot failure leaked the SQLite connection: %v", err)
	}
}

func TestDurableGatewayReloadRejectsAuthorityIdentityChanges(t *testing.T) {
	for _, change := range []string{"path", "disable", "dataplane", "url", "token"} {
		t.Run(change, func(t *testing.T) {
			path := durableGatewayConfig(t, "http://127.0.0.1:1", nil)
			g, err := newGateway(path)
			if err != nil {
				t.Fatal(err)
			}
			durableUnservedCleanup(t, g)
			previous := g.holder.Load()
			durableRewrite(t, path, func(cfg map[string]any) {
				cp := cfg["control_plane"].(map[string]any)
				switch change {
				case "path":
					cp["authority"].(map[string]any)["journal_path"] = filepath.Join(filepath.Dir(path), "other.sqlite")
				case "disable":
					delete(cp, "authority")
				case "dataplane":
					cp["dataplane"] = "another-node"
				case "url":
					cp["url"] = "http://127.0.0.1:2"
				case "token":
					t.Setenv("DURABLE_OTHER_TOKEN", "other-owner-token")
					cp["token_ref"] = map[string]any{"env": "DURABLE_OTHER_TOKEN"}
				}
			})
			if err := g.reload(); err == nil {
				t.Fatal("reload silently changed boot authority identity")
			}
			if g.holder.Load() != previous {
				t.Fatal("rejected reload replaced live state")
			}
		})
	}
}

func TestDurableGatewayCannotEnableAuthorityThroughReload(t *testing.T) {
	path := durableGatewayConfig(t, "http://127.0.0.1:1", func(cfg map[string]any, _ string) {
		delete(cfg["control_plane"].(map[string]any), "authority")
	})
	g, err := newGateway(path)
	if err != nil {
		t.Fatal(err)
	}
	durableUnservedCleanup(t, g)
	durableRewrite(t, path, func(cfg map[string]any) {
		cfg["control_plane"].(map[string]any)["authority"] = map[string]any{"journal_path": filepath.Join(filepath.Dir(path), "enabled.sqlite")}
	})
	if err := g.reload(); err == nil {
		t.Fatal("reload enabled a profile without assembling authority")
	}
}

func TestDurableGatewayReloadAndUIWriteKeepBoundedMetadata(t *testing.T) {
	path := durableGatewayConfig(t, "http://127.0.0.1:1", func(cfg map[string]any, dir string) {
		cfg["provider_store"] = map[string]any{"type": "sqlite", "path": filepath.Join(dir, "providers.sqlite")}
	})
	g, err := newGateway(path)
	if err != nil {
		t.Fatal(err)
	}
	durableUnservedCleanup(t, g)
	before := g.holder.Load()
	route := providerstore.ModelRoute{Targets: []providerstore.Target{{Provider: "up", Model: "claude-test"}}}
	if err := g.WriteModel(context.Background(), "claude-test", route); err == nil {
		t.Fatal("UI write removed conservative context metadata")
	}
	models, err := g.pstore.ListModels(context.Background())
	if err != nil || models["claude-test"].ContextWindow != 4096 || g.holder.Load() != before {
		t.Fatalf("invalid UI write persisted/published: %v %#v", err, models)
	}
	if err := g.pstore.SetModel(context.Background(), "claude-test", route); err != nil {
		t.Fatal(err)
	}
	if err := g.reload(); err == nil || g.holder.Load() != before {
		t.Fatal("reload published unbounded DB metadata")
	}
}

// This fake returns complete protocol bundles; real local Store + Syncer own
// journal persistence, nonce retries, background wakeups and grant accounting.
func durableFakeCP(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var reports atomic.Int64
	docs := []v1alpha1.GovernancePolicy{{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: "inferplane.dev/v1alpha1", Kind: "GovernancePolicy"},
		Metadata: v1alpha1.ObjectMeta{Name: "durable"},
		Spec: v1alpha1.GovernancePolicySpec{Subject: v1alpha1.Subject{Team: "durable-team"}, Rules: []v1alpha1.Rule{{
			Name: "cap", FailurePolicy: v1alpha1.FailClosed,
			Budget: &v1alpha1.BudgetRule{LimitMilliUSD: 1000, HardCap: true, Lease: v1alpha1.LeaseSpec{GrantMilliUSD: 100, RenewInterval: "1s"}},
		}}},
	}}
	var mu sync.Mutex
	grants := map[string]policy.AuthorityGrant{}
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1alpha1/sync" {
			w.WriteHeader(http.StatusOK)
			return
		}
		var req policy.SyncRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if req.Authority == nil || req.Dataplane != "durable-node" || r.Header.Get("Authorization") != "Bearer durable-test-control-token" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		now := time.Now().UTC()
		budgets, err := policy.AuthorityBudgets(docs, now)
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		out := policy.AuthorityResponse{Protocol: policy.AuthorityProtocol, ServerTime: now,
			Generation: policy.GenerationOf(docs), Policies: docs, Budgets: budgets}
		mu.Lock()
		for _, want := range req.Authority.Requests {
			g, ok := grants[want.RequestID]
			if !ok {
				g = policy.AuthorityGrant{ID: "grant-" + want.RequestID, RequestID: want.RequestID, Key: want.Key,
					Revision: want.Revision, WindowID: want.WindowID, Amount: max(want.WantMicroUSD, budgets[0].GrantMicroUSD),
					ExpiresAt: now.Add(time.Second)}
				grants[want.RequestID] = g
			}
			out.Grants = append(out.Grants, g)
		}
		mu.Unlock()
		for _, report := range req.Authority.Reports {
			if report.Observed > 0 {
				reports.Add(1)
			}
			out.ReportAcks = append(out.ReportAcks, policy.AuthorityAck{Instance: report.Instance, ID: report.GrantID, Sequence: report.Sequence})
		}
		_ = json.NewEncoder(w).Encode(policy.SyncResponse{Generation: out.Generation, Authority: &out, SyncIntervalSeconds: 1})
	}))
	t.Cleanup(cp.Close)
	return cp, &reports
}

func TestDurableGatewayReloadKeepsJournalAndAcceptsBoundedTopology(t *testing.T) {
	path := durableGatewayConfig(t, "http://127.0.0.1:1", nil)
	g, err := newGateway(path)
	if err != nil {
		t.Fatal(err)
	}
	durableUnservedCleanup(t, g)
	before, err := g.syncer.Authority.Request(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	durableRewrite(t, path, func(cfg map[string]any) {
		cfg["models"].(map[string]any)["claude-test"].(map[string]any)["context_window"] = 8192
	})
	if err := g.reload(); err != nil {
		t.Fatal(err)
	}
	mc, _ := g.holder.Load().Route("claude-test")
	after, err := g.syncer.Authority.Request(context.Background())
	if err != nil || before.Instance != after.Instance || mc.ContextWindow != 8192 {
		t.Fatalf("reload rebuilt authority or dropped topology: %v %v", mc, err)
	}
}

func TestDurableGatewayAssembledReserveSyncAndReport(t *testing.T) {
	cp, reports := durableFakeCP(t)
	up := newAnthropicUpstream(t)
	path := durableGatewayConfig(t, cp.URL, func(cfg map[string]any, _ string) {
		cfg["providers"].(map[string]any)["up"].(map[string]any)["base_url"] = up.srv.URL
	})
	g, err := newGateway(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- g.serve(ctx) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("durable gateway failed to stop")
		}
	}()
	waitHTTP(t, "http://"+g.AdminAddr()+"/readyz", http.StatusOK)
	key, _, err := g.store.Create(context.Background(), "durable-team", []string{"*"})
	if err != nil {
		t.Fatal(err)
	}
	// The initial policy sync grants no credit. The first real request must
	// fail locally and wake the background syncer rather than call upstream.
	r := postMessages(t, "http://"+g.DataAddr(), key, "claude-test")
	r.Body.Close()
	if r.StatusCode != http.StatusServiceUnavailable || up.apiKey() != "" {
		t.Fatalf("missing credit reached provider: status=%d called=%v", r.StatusCode, up.apiKey() != "")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		r = postMessages(t, "http://"+g.DataAddr(), key, "claude-test")
		r.Body.Close()
		if r.StatusCode == http.StatusOK {
			break
		}
		if r.StatusCode != http.StatusServiceUnavailable || time.Now().After(deadline) {
			t.Fatalf("background authority never enabled request: %d", r.StatusCode)
		}
		time.Sleep(10 * time.Millisecond)
	}
	for reports.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if reports.Load() == 0 {
		t.Fatal("the governor's observed cost never reached the syncer's journal reports")
	}
}

func init() {
	providers.Register("durable-drain-test", func(c providers.Config) (providers.Provider, error) {
		return &durableDrainProvider{Provider: mockprovider.New(c.BaseURL), entered: make(chan struct{}), release: make(chan struct{})}, nil
	})
}

type durableDrainProvider struct {
	providers.Provider
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (p *durableDrainProvider) Complete(ctx context.Context, req *providers.ProxyRequest) (*providers.ProxyResponse, error) {
	p.once.Do(func() { close(p.entered) })
	// Deliberately emulate work that needs cleanup after request cancellation.
	<-p.release
	return p.Provider.Complete(ctx, req)
}

func (*durableDrainProvider) SupportsIngress(protocol string) bool { return protocol == "anthropic" }

func TestDurableGatewayClosesJournalOnlyAfterInflightDrain(t *testing.T) {
	cp, _ := durableFakeCP(t)
	path := durableGatewayConfig(t, cp.URL, func(cfg map[string]any, _ string) {
		cfg["server"].(map[string]any)["drain_grace"] = "10ms"
		cfg["providers"].(map[string]any)["up"].(map[string]any)["type"] = "durable-drain-test"
	})
	g, err := newGateway(path)
	if err != nil {
		t.Fatal(err)
	}
	p := g.holder.Load().Providers()["up"].(*durableDrainProvider)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- g.serve(ctx) }()
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(p.release) }) }
	defer cancel()
	defer release()
	waitHTTP(t, "http://"+g.AdminAddr()+"/readyz", http.StatusOK)
	key, _, err := g.store.Create(context.Background(), "durable-team", []string{"*"})
	if err != nil {
		t.Fatal(err)
	}
	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		for ctx.Err() == nil {
			req, _ := http.NewRequest(http.MethodPost, "http://"+g.DataAddr()+"/v1/messages",
				strings.NewReader(`{"model":"claude-test","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`))
			req.Header.Set("x-api-key", key)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return
			}
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	select {
	case <-p.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("request never reached provider")
	}
	cancel()
	select {
	case <-done:
		t.Fatal("serve closed resources with a handler still in flight")
	case <-time.After(50 * time.Millisecond):
	}
	if _, err := g.syncer.Authority.Request(context.Background()); err != nil {
		t.Fatalf("journal closed while handler still owns a permit: %v", err)
	}
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not finish after handler drained")
	}
	<-clientDone
	if _, err := g.syncer.Authority.Request(context.Background()); !errors.Is(err, governance.ErrAuthorityUnavailable) {
		t.Fatalf("serve failed to close the journal: %v", err)
	}
}
