package main

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/inferplane/inferplane/internal/config"
	"github.com/inferplane/inferplane/internal/live"
	"github.com/inferplane/inferplane/internal/policy"
	"github.com/inferplane/inferplane/internal/providerstore"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/inferplane/inferplane/api/v1alpha1"
	"github.com/inferplane/inferplane/providers"
	"github.com/inferplane/inferplane/providers/testing/mockprovider"
)

// Registered once; each topology generation gets independent, in-memory spies.
func init() {
	providers.Register("routing-acceptance", func(c providers.Config) (providers.Provider, error) {
		return &routingAcceptanceProvider{Provider: mockprovider.New(c.BaseURL)}, nil
	})
}

type routingAcceptanceProvider struct {
	providers.Provider
	calls atomic.Int64
}

func (p *routingAcceptanceProvider) Complete(ctx context.Context, req *providers.ProxyRequest) (*providers.ProxyResponse, error) {
	p.calls.Add(1)
	return p.Provider.Complete(ctx, req)
}
func (*routingAcceptanceProvider) SupportsIngress(protocol string) bool {
	return protocol == "anthropic"
}

func routingPrivacyDoc() v1alpha1.GovernancePolicy {
	return v1alpha1.GovernancePolicy{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: "inferplane.dev/v1alpha1", Kind: "GovernancePolicy"},
		Metadata: v1alpha1.ObjectMeta{Name: "routing-acceptance", Generation: 1},
		Spec: v1alpha1.GovernancePolicySpec{Subject: v1alpha1.Subject{Team: "engineering"}, Rules: []v1alpha1.Rule{{
			Name: "privacy", FailurePolicy: v1alpha1.FailClosed,
			SensitiveData: &v1alpha1.SensitiveDataRule{OnDetected: v1alpha1.InternalOnly, OnUninspectable: v1alpha1.Block, InternalModels: []string{"private"}},
		}}},
	}
}

func writeRoutingPolicy(t *testing.T, path string, doc v1alpha1.GovernancePolicy) {
	t.Helper()
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
}

func routingAcceptanceConfig(t *testing.T, doc *v1alpha1.GovernancePolicy, mutate func(map[string]any, string)) string {
	t.Helper()
	return writeTestConfig(t, func(cfg map[string]any, dir string) {
		cfg["analytics"] = map[string]any{"disabled": true}
		cfg["teams"] = map[string]any{"engineering": map[string]any{"allowed_models": []string{"*"}}}
		cfg["providers"] = map[string]any{
			"public":  map[string]any{"type": "routing-acceptance", "base_url": "public", "data_boundary": "external"},
			"private": map[string]any{"type": "routing-acceptance", "base_url": "private", "data_boundary": "internal"},
		}
		models := map[string]any{}
		rates := map[string]any{}
		for _, name := range []string{"public", "private"} {
			models[name] = map[string]any{"aliases": []string{name + "-alias"}, "context_window": 8192, "capabilities": []string{"tools"}, "targets": []map[string]any{{"provider": name, "model": name}}}
			rates[name] = map[string]any{name: map[string]any{"input_per_mtok": 1, "output_per_mtok": 2}}
		}
		cfg["models"] = models
		cfg["pricing"] = map[string]any{"on_missing": "allow", "overrides": rates}
		if doc != nil {
			path := filepath.Join(dir, "governance.yaml")
			writeRoutingPolicy(t, path, *doc)
			cfg["policies"] = []string{path}
		} else {
			cfg["control_plane"] = map[string]any{"url": "http://127.0.0.1:1", "dataplane": "acceptance"}
		}
		if mutate != nil {
			mutate(cfg, dir)
		}
	})
}

// A malformed listen address is a socket-free sentinel: target validation must
// reject before assembly even tries to bind. A generic boot error is not enough.
func TestGatewayRoutingRejectsTargetsBeforeListen(t *testing.T) {
	for _, kind := range []string{"privacy", "simple", "complex"} {
		for _, invalid := range []string{"unrouted", "unpriced"} {
			t.Run(kind+"/"+invalid, func(t *testing.T) {
				target := "private"
				if invalid == "unrouted" {
					target = "missing"
				}
				doc := routingPrivacyDoc()
				if kind == "privacy" {
					doc.Spec.Rules[0].SensitiveData.InternalModels = []string{target}
				} else {
					c := &v1alpha1.ContextRule{FromModels: []string{"public"}, SimpleModel: "public", ComplexModel: "public", MaxSimpleInputTokens: 256}
					if kind == "simple" {
						c.SimpleModel = target
					} else {
						c.ComplexModel = target
					}
					doc.Spec.Rules = []v1alpha1.Rule{{Name: "context", FailurePolicy: v1alpha1.FailOpen, Routing: &v1alpha1.RoutingRule{Context: c}}}
				}
				path := routingAcceptanceConfig(t, &doc, func(cfg map[string]any, _ string) {
					cfg["server"].(map[string]any)["listen"] = "invalid-listen-address"
					if invalid == "unpriced" {
						delete(cfg["pricing"].(map[string]any)["overrides"].(map[string]any), "private")
					}
				})
				g, err := newGateway(path)
				if g != nil {
					t.Fatal("invalid policy reached a serving gateway")
				}
				if err == nil || !strings.Contains(err.Error(), "policies:") || !strings.Contains(err.Error(), "target") || !strings.Contains(err.Error(), invalid) {
					t.Fatalf("want %s policy target rejection before listen, got %v", invalid, err)
				}
			})
		}
	}
}

// These tests use the actual bound gateway, but drive its mux directly and never
// contact an upstream or control plane. The controller runs the listener gate.
func bootRoutingAcceptance(t *testing.T, path string) (*gateway, string) {
	t.Helper()
	g, err := newGateway(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		g.dataLn.Close()
		g.adminLn.Close()
		closeAll(g.pstore, g.pgstoreQ, g.store, g.aud)
	})
	key, _, err := g.store.Create(context.Background(), "engineering", []string{"*"})
	if err != nil {
		t.Fatal(err)
	}
	return g, key
}

func routingCall(t *testing.T, g *gateway, key, text string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]any{"model": "public", "max_tokens": 32, "messages": []map[string]string{{"role": "user", "content": text}}})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	req.Header.Set("x-api-key", key)
	rec := httptest.NewRecorder()
	g.dataSrv.Handler.ServeHTTP(rec, req)
	return rec
}

func routingProvider(t *testing.T, g *gateway, name string) *routingAcceptanceProvider {
	t.Helper()
	p, ok := g.holder.Load().Provider(name)
	if !ok {
		t.Fatalf("missing provider %s", name)
	}
	return p.(*routingAcceptanceProvider)
}

func TestGatewayRoutingDistributionRecovery(t *testing.T) {
	for _, invalid := range []string{"unrouted", "unpriced"} {
		t.Run(invalid, func(t *testing.T) {
			path := routingAcceptanceConfig(t, nil, func(cfg map[string]any, _ string) {
				// A route may be unpriced in legacy config; policy targets still reject it.
				cfg["models"].(map[string]any)["unpriced"] = map[string]any{"targets": []map[string]string{{"provider": "private", "model": "unpriced"}}}
			})
			g, key := bootRoutingAcceptance(t, path)
			doc := routingPrivacyDoc()
			if rejections := g.polStore.ApplyWire([]v1alpha1.GovernancePolicy{doc}); len(rejections) != 0 {
				t.Fatal(rejections)
			}
			if rec := routingCall(t, g, key, "contact person@example.test"); rec.Code != 200 {
				t.Fatalf("valid generation: %d %s", rec.Code, rec.Body)
			}
			private := routingProvider(t, g, "private")
			public := routingProvider(t, g, "public")
			if private.calls.Load() != 1 || public.calls.Load() != 0 {
				t.Fatal("local policy did not restrict destination")
			}
			doc.Metadata.Generation++
			doc.Spec.Rules[0].SensitiveData.InternalModels = []string{invalid}
			if rejections := g.polStore.ApplyWire([]v1alpha1.GovernancePolicy{doc}); len(rejections) != 1 || !strings.Contains(rejections[0].Reason, invalid) {
				t.Fatalf("invalid generation: %+v", rejections)
			}
			// Even clean text is denied while privacy policy delivery is rejected.
			if rec := routingCall(t, g, key, "hello"); rec.Code != 403 {
				t.Fatalf("rejection gate: %d %s", rec.Code, rec.Body)
			}
			if private.calls.Load() != 1 || public.calls.Load() != 0 {
				t.Fatal("rejected generation made an upstream call")
			}
			doc.Metadata.Generation++
			doc.Spec.Rules[0].SensitiveData.InternalModels = []string{"private-alias"}
			if rejections := g.polStore.ApplyWire([]v1alpha1.GovernancePolicy{doc}); len(rejections) != 0 {
				t.Fatal(rejections)
			}
			if rec := routingCall(t, g, key, "contact person@example.test"); rec.Code != 200 {
				t.Fatalf("recovery: %d %s", rec.Code, rec.Body)
			}
			if private.calls.Load() != 2 || public.calls.Load() != 0 {
				t.Fatal("valid recovery did not restore internal routing")
			}
		})
	}
}

func TestGatewayRoutingDBMetadataReload(t *testing.T) {
	doc := routingPrivacyDoc()
	path := routingAcceptanceConfig(t, &doc, func(cfg map[string]any, dir string) {
		cfg["provider_store"] = map[string]any{"type": "sqlite", "path": filepath.Join(dir, "providers.db")}
	})
	g, key := bootRoutingAcceptance(t, path)
	assertRoute := func() {
		t.Helper()
		st := g.holder.Load()
		mc, ok := st.Route("private")
		if !ok || mc.ContextWindow != 8192 || !slices.Equal(mc.Capabilities, []string{"tools"}) || st.DataBoundary("private") != "internal" || st.Canonical("private-alias") != "private" {
			t.Fatalf("metadata lost: %+v", mc)
		}
		if rec := routingCall(t, g, key, "person@example.test"); rec.Code != 200 || rec.Header().Get("x-inferplane-routed-model") != "private" {
			t.Fatalf("privacy route: %d %s", rec.Code, rec.Body)
		}
		if routingProvider(t, g, "public").calls.Load() != 0 {
			t.Fatal("protected content escaped")
		}
	}
	assertRoute()
	// File metadata becomes stale; the seeded DB must remain authoritative.
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	delete(raw["providers"].(map[string]any)["private"].(map[string]any), "data_boundary")
	delete(raw["models"].(map[string]any)["private"].(map[string]any), "context_window")
	delete(raw["models"].(map[string]any)["private"].(map[string]any), "capabilities")
	b, err = json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	if err := g.reload(); err != nil {
		t.Fatal(err)
	}
	assertRoute()
	// A DB change must affect runtime safety after the next reload.
	p, err := g.pstore.GetProvider(context.Background(), "private")
	if err != nil {
		t.Fatal(err)
	}
	p.DataBoundary = "external"
	if err := g.pstore.UpsertProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if err := g.reload(); err != nil {
		t.Fatal(err)
	}
	if rec := routingCall(t, g, key, "person@example.test"); rec.Code != 403 {
		t.Fatalf("changed boundary accepted: %d %s", rec.Code, rec.Body)
	}
	if routingProvider(t, g, "private").calls.Load() != 0 || routingProvider(t, g, "public").calls.Load() != 0 {
		t.Fatal("unsafe reload called provider")
	}
	p.DataBoundary = "internal"
	if err := g.pstore.UpsertProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	// A newly added DB-only route proves the installed validator reads the holder
	// after reload instead of closing over the boot State.
	if err := g.pstore.SetModel(context.Background(), "new-private", providerstore.ModelRoute{ContextWindow: 8192, Capabilities: []string{"tools"}, Targets: []providerstore.Target{{Provider: "private", Model: "private"}}}); err != nil {
		t.Fatal(err)
	}
	if err := g.reload(); err != nil {
		t.Fatal(err)
	}
	doc.Spec.Rules[0].SensitiveData.InternalModels = []string{"new-private"}
	writeRoutingPolicy(t, filepath.Join(filepath.Dir(path), "governance.yaml"), doc)
	if err := g.polStore.Reload(); err != nil {
		t.Fatalf("validator did not see new topology: %v", err)
	}
	if rec := routingCall(t, g, key, "person@example.test"); rec.Code != 200 || rec.Header().Get("x-inferplane-routed-model") != "new-private" {
		t.Fatalf("DB-only policy route: %d %s", rec.Code, rec.Body)
	}
}

func TestGatewayRoutingBudgetThenPrivacy(t *testing.T) {
	doc := routingPrivacyDoc()
	doc.Spec.Rules = append(doc.Spec.Rules,
		v1alpha1.Rule{Name: "spend", FailurePolicy: v1alpha1.FailOpen, Budget: &v1alpha1.BudgetRule{LimitMilliUSD: 1}},
		v1alpha1.Rule{Name: "economy", FailurePolicy: v1alpha1.FailOpen, Routing: &v1alpha1.RoutingRule{BudgetTiers: &v1alpha1.BudgetTiersRule{BudgetRef: "spend", Tiers: []v1alpha1.BudgetTier{{ThresholdPercent: 1, Substitute: map[string]string{"public": "economy"}}}}}},
	)
	path := routingAcceptanceConfig(t, &doc, func(cfg map[string]any, _ string) {
		cfg["models"].(map[string]any)["economy"] = map[string]any{"context_window": 8192, "targets": []map[string]string{{"provider": "public", "model": "public"}}}
	})
	g, key := bootRoutingAcceptance(t, path)
	if rec := routingCall(t, g, key, "hello"); rec.Code != 200 || rec.Header().Get("x-inferplane-substituted-model") != "" {
		t.Fatalf("initial request: %d %s", rec.Code, rec.Body)
	}
	// First call settles 20 microUSD, crossing 1% of the 1000 microUSD budget.
	if rec := routingCall(t, g, key, "hello again"); rec.Code != 200 || rec.Header().Get("x-inferplane-substituted-model") != "economy" {
		t.Fatalf("budget tier did not activate: %d %v %s", rec.Code, rec.Header(), rec.Body)
	}
	rec := routingCall(t, g, key, "person@example.test")
	if rec.Code != 200 || rec.Header().Get("x-inferplane-substituted-model") != "economy" || rec.Header().Get("x-inferplane-routed-model") != "private" {
		t.Fatalf("privacy after tier: %d %v %s", rec.Code, rec.Header(), rec.Body)
	}
	if routingProvider(t, g, "public").calls.Load() != 2 || routingProvider(t, g, "private").calls.Load() != 1 {
		t.Fatal("tier bypassed privacy destination ceiling")
	}
}

func TestGatewayRoutingAdmissionBeforeProvider(t *testing.T) {
	doc := routingPrivacyDoc()
	path := routingAcceptanceConfig(t, &doc, func(cfg map[string]any, _ string) {
		cfg["teams"].(map[string]any)["engineering"].(map[string]any)["rate_limit"] = map[string]int{"tokens_per_minute": 1}
	})
	g, key := bootRoutingAcceptance(t, path)
	rec := routingCall(t, g, key, "person@example.test")
	if rec.Code != 429 || rec.Header().Get("x-inferplane-routed-model") != "private" {
		t.Fatalf("expected admission denial after safe routing: %d %v %s", rec.Code, rec.Header(), rec.Body)
	}
	if routingProvider(t, g, "private").calls.Load() != 0 || routingProvider(t, g, "public").calls.Load() != 0 {
		t.Fatal("governance denial made a provider call")
	}
}

func TestPolicyRoutingExampleLoadsWithMatchingTopology(t *testing.T) {
	// Exercise the checked-in config/policy through their real loaders and builder,
	// substituting only local secret references; no provider call or socket needed.
	t.Setenv("INFERPLANE_ADMIN_TOKEN", "example-placeholder")
	cfg, err := config.LoadRaw("../../examples/config.policy-routing.json")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.Policies, []string{"examples/policy-routing/governance.yaml"}) {
		t.Fatalf("example policy must stay isolated: %v", cfg.Policies)
	}
	t.Setenv("POLICY_ROUTING_PUBLIC_KEY", "example-placeholder")
	privateKey := filepath.Join(t.TempDir(), "private-key")
	if err := os.WriteFile(privateKey, []byte("example-placeholder"), 0600); err != nil {
		t.Fatal(err)
	}
	p := cfg.Providers["approved-private"]
	p.APIKeyRef = &config.SecretRef{File: privateKey}
	cfg.Providers["approved-private"] = p
	if err := config.ResolveProviders(cfg); err != nil {
		t.Fatal(err)
	}
	st, _, err := live.BuildState(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h := &live.Holder{}
	h.Swap(st)
	store, err := policy.NewStore(filepath.Join("../..", cfg.Policies[0]))
	if err != nil {
		t.Fatal(err)
	}
	store.SetRoutedAndPriced(h.RoutedAndPriced)
	if err := store.Reload(); err != nil {
		t.Fatalf("example target validation: %v", err)
	}
	policies, err := store.MatchingRoutingPolicies("engineering", "")
	if err != nil || len(policies) != 1 {
		t.Fatalf("example policies: %v %v", policies, err)
	}
	var privacy, shadow bool
	for _, rule := range policies[0].Rules {
		if rule.SensitiveData != nil {
			privacy = rule.SensitiveData.OnDetected == v1alpha1.InternalOnly && rule.SensitiveData.OnUninspectable == v1alpha1.Block
		}
		if rule.Routing != nil && rule.Routing.Context != nil {
			shadow = rule.Routing.Context.Mode == v1alpha1.Shadow
		}
	}
	if !privacy || !shadow {
		t.Fatal("example must enforce privacy and default context to Shadow")
	}
	for name, mc := range st.Models() {
		if mc.ContextWindow <= 0 || !slices.Equal(mc.Capabilities, []string{"tools"}) {
			t.Fatalf("invalid example metadata %s: %+v", name, mc)
		}
		for _, alias := range mc.Aliases {
			if err := h.RoutedAndPriced(alias); err != nil {
				t.Fatal(err)
			}
		}
	}
	if st.DataBoundary("approved-private") != "internal" || st.DataBoundary("approved-public") != "external" {
		t.Fatal("example boundaries lost")
	}
}

func TestGatewayRoutingContextDistributionChecksTargets(t *testing.T) {
	path := routingAcceptanceConfig(t, nil, func(cfg map[string]any, _ string) {
		cfg["models"].(map[string]any)["unpriced"] = map[string]any{"targets": []map[string]string{{"provider": "private", "model": "unpriced"}}}
	})
	g, key := bootRoutingAcceptance(t, path)
	for _, field := range []string{"simple", "complex"} {
		for _, target := range []string{"unrouted", "unpriced"} {
			t.Run(field+"/"+target, func(t *testing.T) {
				doc := routingPrivacyDoc()
				c := &v1alpha1.ContextRule{FromModels: []string{"public"}, SimpleModel: "public", ComplexModel: "public", MaxSimpleInputTokens: 256}
				if field == "simple" {
					c.SimpleModel = target
				} else {
					c.ComplexModel = target
				}
				doc.Spec.Rules = []v1alpha1.Rule{{Name: "context", FailurePolicy: v1alpha1.FailOpen, Routing: &v1alpha1.RoutingRule{Context: c}}}
				if rejected := g.polStore.ApplyWire([]v1alpha1.GovernancePolicy{doc}); len(rejected) != 1 || !strings.Contains(rejected[0].Reason, target) {
					t.Fatalf("invalid context delivery accepted: %+v", rejected)
				}
				if rec := routingCall(t, g, key, "hello"); rec.Code != 200 {
					t.Fatalf("context rejection created privacy gate: %d %s", rec.Code, rec.Body)
				}
			})
		}
	}
	if routingProvider(t, g, "private").calls.Load() != 0 || routingProvider(t, g, "public").calls.Load() != 4 {
		t.Fatal("context rejection changed the actual route")
	}
}
