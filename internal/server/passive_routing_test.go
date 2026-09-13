package server

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/inferplane/inferplane/api/v1alpha1"
	"github.com/inferplane/inferplane/internal/adminauth"
	"github.com/inferplane/inferplane/internal/audit"
	"github.com/inferplane/inferplane/internal/config"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/policy"
	"github.com/inferplane/inferplane/internal/server/routingtest"
)

// All three ingresses must recover through existing cross-model fallbacks even
// when passive policy inspection discovers missing model metadata or pricing.
func TestPolicyPassiveLegacyFallback(t *testing.T) {
	for _, ing := range policyIngresses {
		for _, mode := range []v1alpha1.ContextMode{"", v1alpha1.Shadow, v1alpha1.Enforce} {
			t.Run(ing.name()+"/"+string(mode), func(t *testing.T) {
				f := routingtest.New(t, func(cfg *config.Config) {
					cfg.ModelFallbacks["premium"] = "private"
					m := cfg.Models["private"]
					m.ContextWindow = 0
					m.Capabilities = nil
					cfg.Models["private"] = m
					delete(cfg.Pricing.Overrides, "private")
					delete(cfg.Pricing.Overrides, "retry")
				})
				f.Public.Fail = true
				if mode != "" {
					p := routingtest.Context(mode)
					p.Rules[0].Routing.Context.SimpleModel = "missing"
					f.Policies = []*policy.Policy{p}
				}
				raw := routingtest.Body(ing.protocol, "clean", ing.stream)
				rec := policyDo(policyMux(f, nil, nil, nil, nil), ing.path, raw, true)
				if rec.Code != 200 || len(f.Public.Calls) != 1 || len(f.Private.Calls) != 1 || len(f.Retry.Calls) != 0 {
					t.Fatalf("passive policy changed fallback: status=%d calls=%d/%d/%d", rec.Code, len(f.Public.Calls), len(f.Private.Calls), len(f.Retry.Calls))
				}
				if f.Private.Calls[0].Body != raw || f.Private.Calls[0].Model != "private" {
					t.Fatal("fallback lost original bytes or per-attempt model")
				}
			})
		}
	}
}

// The 12 no-rule cases have independently verified pre-integration ingress
// controls. Shadow and unavailable Enforce must preserve those same outcomes.
func TestPolicyPassiveOriginalPreflight(t *testing.T) {
	for _, ing := range policyIngresses {
		for _, mode := range []v1alpha1.ContextMode{"", v1alpha1.Shadow, v1alpha1.Enforce} {
			for _, small := range []string{"premium", "private"} {
				t.Run(ing.name()+"/"+string(mode)+"/small-"+small, func(t *testing.T) {
					f := routingtest.New(t, func(cfg *config.Config) {
						cfg.ModelFallbacks["premium"] = "private"
						m := cfg.Models[small]
						m.ContextWindow = 1
						cfg.Models[small] = m
					})
					if mode != "" {
						p := routingtest.Context(mode)
						p.Rules[0].Routing.Context.SimpleModel = "missing"
						f.Policies = []*policy.Policy{p}
					}
					aud, buf := policyAudit(t)
					store := stubStore{key: "dev-key", p: keystore.Principal{Team: "team", AllowedModels: []string{"*"}, KeyOptions: keystore.KeyOptions{Owner: "user"}}}
					h := DataMux(f.Router, f.Holder, store, aud, nil, nil, nil, func(string) (keystore.TeamRecord, bool) {
						return keystore.TeamRecord{AllowedRegions: []string{"eu"}}, true
					}, nil, nil, adminauth.MappingConfig{}, nil, 0)
					raw := routingtest.Body(ing.protocol, "clean", ing.stream)
					rec := policyDo(h, ing.path, raw, true)
					aud.Close()
					wantStatus, wantCalls := 200, 1
					if small == "premium" {
						wantStatus, wantCalls = 400, 0
					}
					if rec.Code != wantStatus || len(f.Private.Calls) != wantCalls || len(f.Public.Calls) != 0 {
						t.Fatalf("passive context changed preflight: status=%d calls=%d want=%d/%d", rec.Code, len(f.Private.Calls), wantStatus, wantCalls)
					}
					lines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))
					var started audit.Record
					if err := json.Unmarshal(lines[0], &started); err != nil {
						t.Fatal(err)
					}
					if started.Request.ModelRequested != "premium" {
						t.Fatalf("legacy request attribution promoted fallback: %+v", started.Request)
					}
					if wantCalls == 1 && (f.Private.Calls[0].Model != "private" || f.Private.Calls[0].Body != raw) {
						t.Fatal("actual attempt did not retain ct.Model/original bytes")
					}
					if mode != "" {
						d := started.Request.Routing
						if d == nil || d.RequestedModel != "premium" || d.SelectedModel != "private" || d.PlannedProvider != "private" {
							t.Fatalf("planned evidence lost: %+v", d)
						}
						if wantCalls == 1 {
							var completed audit.Record
							if err := json.Unmarshal(lines[len(lines)-1], &completed); err != nil {
								t.Fatal(err)
							}
							if completed.Request.Routing.ActualModel != "private" || completed.Request.Routing.ActualProvider != "private" {
								t.Fatal("actual attempt evidence lost")
							}
						}
					}
				})
			}
		}
	}
}
