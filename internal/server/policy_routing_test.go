package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inferplane/inferplane/api/v1alpha1"
	"github.com/inferplane/inferplane/internal/adminauth"
	"github.com/inferplane/inferplane/internal/audit"
	"github.com/inferplane/inferplane/internal/bodystore"
	"github.com/inferplane/inferplane/internal/budget"
	"github.com/inferplane/inferplane/internal/config"
	"github.com/inferplane/inferplane/internal/filter"
	"github.com/inferplane/inferplane/internal/governance"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/limiter"
	"github.com/inferplane/inferplane/internal/live"
	"github.com/inferplane/inferplane/internal/metrics"
	"github.com/inferplane/inferplane/internal/policy"
	"github.com/inferplane/inferplane/internal/sensitivity"
	"github.com/inferplane/inferplane/internal/server/routingtest"
	"github.com/inferplane/inferplane/pkg/schema"
)

type policyIngress struct {
	protocol, path string
	stream         bool
}

var policyIngresses = []policyIngress{
	{"anthropic", "/v1/messages", false}, {"anthropic", "/v1/messages", true},
	{"openai", "/v1/chat/completions", false}, {"openai", "/v1/chat/completions", true},
	{"bedrock", "/model/premium/invoke", false}, {"bedrock", "/model/premium/invoke-with-response-stream", true},
}

func (i policyIngress) name() string {
	if i.stream {
		return i.protocol + "/stream"
	}
	return i.protocol + "/complete"
}

func policyMux(f *routingtest.Fixture, aud *audit.Writer, gov *governance.Governor, m *metrics.Metrics, bodies *bodystore.Recorder, opts ...DataMuxOption) http.Handler {
	store := stubStore{key: "dev-key", p: keystore.Principal{KeyID: "opaque-key", Team: "team", AllowedModels: []string{"*"}, KeyOptions: keystore.KeyOptions{Owner: "user"}}}
	return DataMux(f.Router, f.Holder, store, aud, gov, m, nil, nil, bodies, nil, adminauth.MappingConfig{}, nil, 0, opts...)
}
func policyDo(h http.Handler, path, body string, auth bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	if auth {
		req.Header.Set("x-api-key", "dev-key")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}
func policyAudit(t *testing.T) (*audit.Writer, *bytes.Buffer) {
	t.Helper()
	buf := new(bytes.Buffer)
	w, err := audit.NewWriter("task4", filepath.Join(t.TempDir(), "audit.wal"), []audit.Sink{audit.NewWriterSink("buffer", buf, true)})
	if err != nil {
		t.Fatal(err)
	}
	return w, buf
}
func countBody(protocol, inner string) string {
	if protocol == "anthropic" {
		return inner
	}
	return `{"input":{"invokeModel":{"body":"` + base64.StdEncoding.EncodeToString([]byte(inner)) + `"}}}`
}

func TestPolicyCountReadinessAndBodyLimit(t *testing.T) {
	for _, protocol := range []string{"anthropic", "bedrock"} {
		for _, state := range []string{"unsynced", "stale", "ready", "lookup-error", "oversize", "oversize-valid-prefix", "oversize-short-body"} {
			t.Run(protocol+"/"+state, func(t *testing.T) {
				f := routingtest.New(t, nil)
				if state == "lookup-error" {
					f.LookupError = errors.New("person@example.test")
				}
				ready := state != "unsynced" && state != "stale"
				opts := []DataMuxOption{WithGovernanceGate(func() (bool, string) { return ready, state })}
				path := "/v1/messages/count_tokens"
				field := "input_tokens"
				if protocol == "bedrock" {
					path = "/model/premium/count-tokens"
					field = "inputTokens"
				}
				body := countBody(protocol, routingtest.Body(protocol, "clean", false))
				if state == "oversize" {
					opts = append(opts, WithMaxRequestBytes(64))
				}
				if state == "oversize-valid-prefix" {
					opts = append(opts, WithMaxRequestBytes(int64(len(body))))
					body += strings.Repeat(" ", 100)
				}
				if state == "oversize-short-body" {
					opts = append(opts, WithMaxRequestBytes(int64(len(body)+10)))
				}
				h := policyMux(f, nil, nil, nil, nil, opts...)
				var rec *httptest.ResponseRecorder
				if state == "oversize-short-body" {
					req := httptest.NewRequest("POST", path, strings.NewReader(body))
					req.ContentLength = int64(len(body) + 100)
					req.Header.Set("x-api-key", "dev-key")
					rec = httptest.NewRecorder()
					h.ServeHTTP(rec, req)
				} else {
					rec = policyDo(h, path, body, true)
				}
				if rec.Code != 200 {
					t.Fatalf("count must stay local/200: %d %s", rec.Code, rec.Body)
				}
				var out map[string]int64
				if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out[field] < 1 {
					t.Fatalf("invalid count shape: %s", rec.Body)
				}
				want := 0
				if state == "ready" {
					want = 1
				}
				if len(f.Public.Calls) != want {
					t.Fatalf("%s count called upstream %d times, want %d", state, len(f.Public.Calls), want)
				}
				if rec := policyDo(h, path, body, false); rec.Code != 401 {
					t.Fatalf("count auth changed: %d", rec.Code)
				}
			})
		}
	}
	for _, ing := range policyIngresses {
		t.Run(ing.name(), func(t *testing.T) {
			f := routingtest.New(t, nil)
			body := routingtest.Body(ing.protocol, "clean", ing.stream)
			for _, test := range []struct {
				opt    DataMuxOption
				status int
			}{
				{WithMaxRequestBytes(32), 413},
				{WithGovernanceGate(func() (bool, string) { return false, "stale" }), 503},
			} {
				rec := policyDo(policyMux(f, nil, nil, nil, nil, test.opt), ing.path, body, true)
				if rec.Code != test.status || len(f.Public.Calls) != 0 {
					t.Fatalf("generation gate = %d, calls=%d", rec.Code, len(f.Public.Calls))
				}
			}
		})
	}
}

type admissionSpy struct {
	limiter.LimiterStore
	calls int
}

func (s *admissionSpy) AllowRate(key string, cost, rate, burst int64) bool {
	s.calls++
	return s.LimiterStore.AllowRate(key, cost, rate, burst)
}

type captureSpy struct {
	bodystore.Store
	puts int
}

func (s *captureSpy) Put(context.Context, bodystore.Row) error { s.puts++; return nil }

func TestPolicyBlockBeforeAdmissionAndCapture(t *testing.T) {
	for _, ing := range policyIngresses {
		t.Run(ing.name(), func(t *testing.T) {
			f := routingtest.New(t, nil)
			f.Policies = []*policy.Policy{routingtest.Privacy(v1alpha1.InternalOnly), routingtest.Privacy(v1alpha1.Block)}
			lim := &admissionSpy{LimiterStore: limiter.NewMemory()}
			gov := governance.NewGovernor(map[string]governance.TeamPolicy{"team": {RatePerMin: 1, RateBurst: 1}}, lim, budget.NewMemory(), nil)
			store := &captureSpy{}
			bodies := bodystore.NewRecorder(store, [32]byte{1}, 0, 1<<20)
			t.Cleanup(bodies.Close)
			aud, buf := policyAudit(t)
			rec := policyDo(policyMux(f, aud, gov, nil, bodies), ing.path, routingtest.Body(ing.protocol, "system", ing.stream), true)
			aud.Close()
			bodies.Close()
			if rec.Code != 403 || lim.calls != 0 || store.puts != 0 || len(f.Public.Calls)+len(f.Private.Calls) != 0 {
				t.Fatalf("Block must precede admission/capture: status=%d rate=%d captures=%d", rec.Code, lim.calls, store.puts)
			}
			if !strings.Contains(buf.String(), `"reason":"sensitive_blocked"`) || strings.Contains(buf.String(), "actual_provider") || strings.Contains(buf.String(), "person@example.test") {
				t.Fatalf("unsafe/missing denial evidence: %s", buf)
			}
			// A subsequent clean request can still consume the first rate slot.
			f.Policies = nil
			if rec := policyDo(policyMux(f, nil, gov, nil, nil), ing.path, routingtest.Body(ing.protocol, "clean", ing.stream), true); rec.Code != 200 {
				t.Fatalf("blocked request consumed admission: %d", rec.Code)
			}
		})
	}
}

func TestPolicyContextEvidence(t *testing.T) {
	for _, ing := range policyIngresses {
		for _, mode := range []v1alpha1.ContextMode{v1alpha1.Shadow, v1alpha1.Enforce} {
			for _, surface := range []string{"clean", "tool"} {
				t.Run(ing.name()+"/"+string(mode)+"/"+surface, func(t *testing.T) {
					f := routingtest.New(t, nil)
					f.Policies = []*policy.Policy{routingtest.Context(mode)}
					aud, buf := policyAudit(t)
					m := metrics.New()
					rec := policyDo(policyMux(f, aud, nil, m, nil), ing.path, routingtest.Body(ing.protocol, surface, ing.stream), true)
					aud.Close()
					selected := mode == v1alpha1.Enforce && surface == "clean"
					if rec.Code != 200 {
						t.Fatalf("context response %d %s", rec.Code, rec.Body)
					}
					if selected {
						if len(f.Private.Calls) != 1 || len(f.Public.Calls) != 0 || rec.Header().Get("x-inferplane-routed-model") != "economy" {
							t.Fatal("short Enforce did not apply economy")
						}
					} else if len(f.Public.Calls) != 1 || len(f.Private.Calls) != 0 || rec.Header().Get("x-inferplane-routed-model") != "" {
						t.Fatal("Shadow or tool history switched actual route")
					}
					if !strings.Contains(buf.String(), `"proposed_model":"economy"`) || !strings.Contains(buf.String(), `"routing":`) {
						t.Fatalf("missing recommendation audit: %s", buf)
					}
					if strings.Contains(buf.String(), "person@example.test") || strings.Contains(rec.Header().Get("x-inferplane-routing-reason"), "person@example.test") {
						t.Fatal("PII in decision evidence")
					}
					families, err := m.Registry().Gather()
					if err != nil {
						t.Fatal(err)
					}
					found := false
					for _, family := range families {
						if family.GetName() == "inferplane_routing_decisions_total" {
							found = true
							for _, metric := range family.Metric {
								if len(metric.Label) != 3 {
									t.Fatal("unbounded routing metric shape")
								}
							}
						}
					}
					if !found {
						t.Fatal("missing routing decision counter")
					}
				})
			}
		}
	}
}

// Shadow observes context selection but cannot hide an applied privacy route.
func TestPolicyPrivacySelectionAdvertisedWithShadow(t *testing.T) {
	for _, ing := range policyIngresses {
		t.Run(ing.name(), func(t *testing.T) {
			f := routingtest.New(t, nil)
			f.Policies = []*policy.Policy{routingtest.Privacy(v1alpha1.InternalOnly), routingtest.Context(v1alpha1.Shadow)}
			rec := policyDo(policyMux(f, nil, nil, nil, nil), ing.path, routingtest.Body(ing.protocol, "system", ing.stream), true)
			if rec.Code != 200 || len(f.Private.Calls) != 1 || len(f.Public.Calls) != 0 {
				t.Fatal("privacy was not applied before Shadow")
			}
			if rec.Header().Get("x-inferplane-routed-model") != "private" || rec.Header().Get("x-inferplane-routing-reason") != "context_shadow" {
				t.Fatalf("applied privacy route hidden by Shadow: %v", rec.Header())
			}
		})
	}
}

// The same model on different hosts must retain both planned and actual target.
func TestPolicySameModelAttemptEvidence(t *testing.T) {
	for _, ing := range policyIngresses {
		for _, partial := range []bool{false, true} {
			if partial && !ing.stream {
				continue
			}
			t.Run(ing.name()+map[bool]string{false: "/success", true: "/partial"}[partial], func(t *testing.T) {
				f := routingtest.New(t, func(cfg *config.Config) {
					m := cfg.Models["premium"]
					m.Targets = cfg.Models["private"].Targets
					cfg.Models["premium"] = m
				})
				p := routingtest.Privacy(v1alpha1.InternalOnly)
				p.Rules[0].SensitiveData.InternalModels = []string{"premium"}
				f.Policies = []*policy.Policy{p}
				f.Private.Fail = true
				f.Retry.Partial = partial
				aud, buf := policyAudit(t)
				rec := policyDo(policyMux(f, aud, nil, nil, nil), ing.path, routingtest.Body(ing.protocol, "tool", ing.stream), true)
				aud.Close()
				if rec.Code != 200 || len(f.Public.Calls) != 0 || len(f.Retry.Calls) != 1 {
					t.Fatalf("unsafe same-model route %d", rec.Code)
				}
				lines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))
				if len(lines) != 2 {
					t.Fatalf("expected started/completed: %s", buf)
				}
				for j, line := range lines {
					if !bytes.Contains(line, []byte(`"planned_provider":"private"`)) || !bytes.Contains(line, []byte(`"planned_boundary":"internal"`)) {
						t.Fatalf("planned target missing: %s", line)
					}
					if j == 0 && bytes.Contains(line, []byte(`"actual_provider"`)) {
						t.Fatalf("started record claims an attempt: %s", line)
					}
					if j == 1 && (!bytes.Contains(line, []byte(`"actual_provider":"retry"`)) || !bytes.Contains(line, []byte(`"actual_boundary":"internal"`))) {
						t.Fatalf("actual retry missing: %s", line)
					}
				}
				if partial && !strings.Contains(buf.String(), `"partial":true`) {
					t.Fatal("partial record missing")
				}
				if strings.Contains(buf.String(), "person@example.test") {
					t.Fatal("PII persisted in evidence")
				}
				if audit.RoutingFrom(f.Private.Calls[0].Context).ActualProvider != "private" || audit.RoutingFrom(f.Retry.Calls[0].Context).ActualProvider != "retry" {
					t.Fatal("retained first-attempt context drifted after retry")
				}
			})
		}
	}
}

type policyInspectorFunc func(context.Context, string, []byte) (sensitivity.Result, error)

func (f policyInspectorFunc) Inspect(ctx context.Context, protocol string, b []byte) (sensitivity.Result, error) {
	return f(ctx, protocol, b)
}

func TestPolicySnapshotContextAndPricing(t *testing.T) {
	for _, ing := range policyIngresses {
		t.Run(ing.name(), func(t *testing.T) {
			f := routingtest.New(t, func(cfg *config.Config) {
				cfg.Pricing.Version = "captured"
				cfg.Pricing.Overrides["private"]["cheap"] = config.RateConfig{InputPerMTok: 1}
			})
			f.Policies = []*policy.Policy{routingtest.Context(v1alpha1.Enforce)}
			n := int64(10)
			f.Private.Usage = &schema.Usage{InputTokens: &n}
			old := f.Holder.Load()
			reloads := 0
			f.Router.SetRequestInspector(policyInspectorFunc(func(ctx context.Context, protocol string, raw []byte) (sensitivity.Result, error) {
				reloads++
				// If ingress reloads after the decision, it will reject the new
				// one-token context or settle using the new, much higher rate.
				m := f.Config.Models["economy"]
				m.ContextWindow = 1
				f.Config.Models["economy"] = m
				f.Config.Pricing.Version = "new"
				f.Config.Pricing.Overrides["private"]["cheap"] = config.RateConfig{InputPerMTok: 999}
				newState, _, err := live.BuildState(f.Config)
				if err != nil {
					t.Fatal(err)
				}
				f.Holder.Swap(newState)
				return sensitivity.NewInspector().Inspect(ctx, protocol, raw)
			}))
			aud, buf := policyAudit(t)
			gov := governance.NewGovernor(nil, limiter.NewMemory(), budget.NewMemory(), nil)
			rec := policyDo(policyMux(f, aud, gov, nil, nil), ing.path, routingtest.Body(ing.protocol, "clean", ing.stream), true)
			aud.Close()
			if rec.Code != 200 || len(f.Private.Calls) != 1 || reloads != 1 || old == f.Holder.Load() {
				t.Fatalf("context/provider snapshot lost: %d %s", rec.Code, rec.Body)
			}
			if !strings.Contains(buf.String(), `"pricing_version":"captured"`) || !strings.Contains(buf.String(), `"amount_usd_micros":10`) {
				t.Fatalf("settled on wrong snapshot: %s", buf)
			}
		})
	}
}

func TestPolicyResolvedPreTierAndPostTier(t *testing.T) {
	for _, ing := range policyIngresses {
		for _, withPolicy := range []bool{false, true} {
			t.Run(ing.name()+map[bool]string{false: "/legacy", true: "/context"}[withPolicy], func(t *testing.T) {
				f := routingtest.New(t, nil)
				f.Router.SetTierGate(func(keystore.Principal) map[string]string { return map[string]string{"premium": "private"} })
				if withPolicy {
					p := routingtest.Context(v1alpha1.Enforce)
					p.Rules[0].Routing.Context.FromModels = []string{"private"}
					f.Policies = []*policy.Policy{p}
				}
				aud, buf := policyAudit(t)
				// The unrouted client model is NOT allowed. ResolveModel selects
				// premium before both tier and request-policy RBAC checks.
				store := stubStore{key: "dev-key", p: keystore.Principal{Team: "team", AllowedModels: []string{"premium", "private", "economy"}, KeyOptions: keystore.KeyOptions{Owner: "user"}}}
				h := DataMux(f.Router, f.Holder, store, aud, nil, nil, nil, nil, nil, nil, adminauth.MappingConfig{}, nil, 0)
				raw := strings.Replace(routingtest.Body(ing.protocol, "clean", ing.stream), `"premium"`, `"unrouted"`, 1)
				path := strings.Replace(ing.path, "premium", "unrouted", 1)
				rec := policyDo(h, path, raw, true)
				aud.Close()
				want := "private"
				if withPolicy {
					want = "economy"
				}
				if rec.Code != 200 || len(f.Private.Calls) != 1 || f.Private.Calls[0].Model != want {
					t.Fatalf("pre/post tier mismatch: %d %s calls=%+v", rec.Code, rec.Body, f.Private.Calls)
				}
				if rec.Header().Get("x-inferplane-substituted-model") != "private" || !strings.Contains(buf.String(), `"model_substituted_from":"premium"`) {
					t.Fatalf("budget evidence lost: %s", buf)
				}
				if withPolicy {
					if !strings.Contains(buf.String(), `"requested_model":"premium"`) || !strings.Contains(buf.String(), `"selected_model":"economy"`) {
						t.Fatalf("wrong routing model attribution: %s", buf)
					}
				} else if strings.Contains(buf.String(), `"routing":`) || rec.Header().Get("x-inferplane-routing-reason") != "" {
					t.Fatal("legacy traffic acquired new evidence")
				}
			})
		}
	}
}

type policyMasker struct{ calls int }

func (*policyMasker) Name() string { return "mask-test" }
func (m *policyMasker) Mask(s string) (string, int) {
	m.calls++
	return strings.ReplaceAll(s, "person@example.test", "redacted"), strings.Count(s, "person@example.test")
}

func TestPolicyDecisionBeforeLegacyMask(t *testing.T) {
	for _, ing := range policyIngresses {
		t.Run(ing.name(), func(t *testing.T) {
			f := routingtest.New(t, nil)
			f.Policies = []*policy.Policy{routingtest.Privacy(v1alpha1.Block)}
			mask := &policyMasker{}
			store := stubStore{key: "dev-key", p: keystore.Principal{Team: "team", AllowedModels: []string{"*"}, KeyOptions: keystore.KeyOptions{Owner: "user"}}}
			h := DataMux(f.Router, f.Holder, store, nil, nil, nil, &filter.Masking{Filter: mask, Global: true}, nil, nil, nil, adminauth.MappingConfig{}, nil, 0)
			raw := strings.Replace(routingtest.Body(ing.protocol, "clean", ing.stream), "hello", "person@example.test", 1)
			rec := policyDo(h, ing.path, raw, true)
			if rec.Code != 403 || mask.calls != 0 || f.Lookups != 1 || len(f.Public.Calls) != 0 {
				t.Fatalf("mask ran before Block: %d masks=%d lookups=%d", rec.Code, mask.calls, f.Lookups)
			}
		})
	}
}

func TestPolicyEmptyRegionChainRecoversSafely(t *testing.T) {
	paths := append([]policyIngress{}, policyIngresses...)
	paths = append(paths, policyIngress{"anthropic", "/v1/messages/count_tokens", false}, policyIngress{"bedrock", "/model/premium/count-tokens", false})
	for _, ing := range paths {
		t.Run(ing.path+ing.name(), func(t *testing.T) {
			f := routingtest.New(t, nil)
			f.Policies = []*policy.Policy{routingtest.Privacy(v1alpha1.InternalOnly)}
			store := stubStore{key: "dev-key", p: keystore.Principal{Team: "team", AllowedModels: []string{"*"}, KeyOptions: keystore.KeyOptions{Owner: "user"}}}
			h := DataMux(f.Router, f.Holder, store, nil, nil, nil, nil, func(string) (keystore.TeamRecord, bool) {
				return keystore.TeamRecord{AllowedRegions: []string{"eu"}}, true
			}, nil, nil, adminauth.MappingConfig{}, nil, 0)
			raw := routingtest.Body(ing.protocol, "system", ing.stream)
			if strings.Contains(ing.path, "count") {
				raw = countBody(ing.protocol, raw)
			}
			rec := policyDo(h, ing.path, raw, true)
			if rec.Code != 200 || len(f.Public.Calls) != 0 || len(f.Private.Calls) != 1 {
				t.Fatalf("empty region chain denied before recovery: %d %s", rec.Code, rec.Body)
			}
		})
	}
}

func TestPolicyFailuresNeverRestoreOriginalChain(t *testing.T) {
	paths := append([]policyIngress{}, policyIngresses...)
	paths = append(paths, policyIngress{"anthropic", "/v1/messages/count_tokens", false}, policyIngress{"bedrock", "/model/premium/count-tokens", false})
	for _, ing := range paths {
		for _, failure := range []string{"lookup", "inspection", "unknown-boundary", "capability", "context", "region", "rbac", "unpriced"} {
			t.Run(ing.path+ing.name()+"/"+failure, func(t *testing.T) {
				f := routingtest.New(t, func(cfg *config.Config) {
					if failure == "unknown-boundary" {
						for _, name := range []string{"private", "retry"} {
							pc := cfg.Providers[name]
							pc.DataBoundary = "unknown"
							cfg.Providers[name] = pc
						}
					}
					m := cfg.Models["private"]
					if failure == "capability" {
						m.Capabilities = nil
					}
					if failure == "context" {
						m.ContextWindow = 1
					}
					cfg.Models["private"] = m
					if failure == "unpriced" {
						delete(cfg.Pricing.Overrides, "private")
						delete(cfg.Pricing.Overrides, "retry")
					}
				})
				f.Policies = []*policy.Policy{routingtest.Privacy(v1alpha1.InternalOnly)}
				if failure == "lookup" {
					f.LookupError = errors.New("person@example.test")
				}
				if failure == "inspection" {
					f.Router.SetRequestInspector(policyInspectorFunc(func(context.Context, string, []byte) (sensitivity.Result, error) {
						return sensitivity.Result{}, errors.New("person@example.test")
					}))
				}
				models := []string{"*"}
				if failure == "rbac" {
					models = []string{"premium"}
				}
				store := stubStore{key: "dev-key", p: keystore.Principal{Team: "team", AllowedModels: models, KeyOptions: keystore.KeyOptions{Owner: "user"}}}
				var teamPolicy func(string) (keystore.TeamRecord, bool)
				if failure == "region" {
					teamPolicy = func(string) (keystore.TeamRecord, bool) {
						return keystore.TeamRecord{AllowedRegions: []string{"ap"}}, true
					}
				}
				h := DataMux(f.Router, f.Holder, store, nil, nil, nil, nil, teamPolicy, nil, nil, adminauth.MappingConfig{}, nil, 0)
				raw := routingtest.Body(ing.protocol, "tool", ing.stream)
				want := 403
				if strings.Contains(ing.path, "count") {
					raw = countBody(ing.protocol, raw)
					want = 200
				}
				rec := policyDo(h, ing.path, raw, true)
				if rec.Code != want || len(f.Public.Calls)+len(f.Private.Calls)+len(f.Retry.Calls) != 0 {
					t.Fatalf("unsafe refusal: %d calls=%d/%d/%d", rec.Code, len(f.Public.Calls), len(f.Private.Calls), len(f.Retry.Calls))
				}
				if strings.Contains(rec.Body.String(), "person@example.test") || strings.Contains(rec.Header().Get("x-inferplane-routing-reason"), "@") {
					t.Fatal("sensitive failure text exposed")
				}
			})
		}
	}
}

func TestPolicyCountAuditAndContextExemption(t *testing.T) {
	for _, protocol := range []string{"anthropic", "bedrock"} {
		for _, mode := range []string{"block", "internal", "context", "legacy"} {
			t.Run(protocol+"/"+mode, func(t *testing.T) {
				f := routingtest.New(t, nil)
				switch mode {
				case "block":
					f.Policies = []*policy.Policy{routingtest.Privacy(v1alpha1.Block)}
				case "internal":
					f.Policies = []*policy.Policy{routingtest.Privacy(v1alpha1.InternalOnly)}
				case "context":
					f.Policies = []*policy.Policy{routingtest.Context(v1alpha1.Enforce)}
				}
				path := "/v1/messages/count_tokens"
				if protocol == "bedrock" {
					path = "/model/premium/count-tokens"
				}
				aud, buf := policyAudit(t)
				rec := policyDo(policyMux(f, aud, nil, nil, nil), path, countBody(protocol, routingtest.Body(protocol, "system", false)), true)
				aud.Close()
				if rec.Code != 200 {
					t.Fatalf("count status %d", rec.Code)
				}
				if mode == "legacy" {
					if buf.Len() != 0 {
						t.Fatal("legacy count acquired audit records")
					}
					return
				}
				lines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))
				if len(lines) != 2 {
					t.Fatalf("missing count decision records: %s", buf)
				}
				var started, completed audit.Record
				if err := json.Unmarshal(lines[0], &started); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(lines[1], &completed); err != nil {
					t.Fatal(err)
				}
				if started.Event != "request_started" || completed.Event != "request_completed" || started.Request.Routing == nil || completed.Request.Routing == nil || started.Request.Routing.ActualProvider != "" {
					t.Fatalf("count lifecycle metadata: %s", buf)
				}
				d := completed.Request.Routing
				switch mode {
				case "block":
					if d.Reason != "sensitive_blocked" || d.ActualProvider != "" || len(f.Public.Calls)+len(f.Private.Calls) != 0 {
						t.Fatal("local refusal claims/calls an actual provider")
					}
				case "internal":
					if d.PlannedProvider != "private" || d.ActualProvider != "private" || d.ActualBoundary != "internal" || len(f.Public.Calls) != 0 {
						t.Fatal("count destination evidence lost")
					}
				case "context":
					if d.ProposedModel != "" || d.ActualProvider != "public" || len(f.Private.Calls) != 0 {
						t.Fatal("count applied a context recommendation")
					}
				}
				if strings.Contains(buf.String(), "person@example.test") {
					t.Fatal("count audit retained PII")
				}
			})
		}
	}
}
