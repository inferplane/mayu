package router

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/inferplane/inferplane/api/v1alpha1"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/policy"
	"github.com/inferplane/inferplane/internal/sensitivity"
	"github.com/inferplane/inferplane/providers/testing/mockprovider"
)

type adaptiveRedactor func(context.Context, string, []byte) ([]byte, error)

func (f adaptiveRedactor) Redact(ctx context.Context, protocol string, raw []byte) ([]byte, error) {
	return f(ctx, protocol, raw)
}

func setAdaptiveRedactor(t *testing.T, r *Router, f adaptiveRedactor) {
	t.Helper()
	r.SetRequestRedactor(f)
}

func setBudgetConstraint(t *testing.T, r *Router, m map[string]string) {
	t.Helper()
	r.SetBudgetConstraintGate(func(keystore.Principal) map[string]string { return m })
}

func maskPolicy() *policy.Policy {
	p := privatePolicy("mask")
	p.Rules[0].SensitiveData.OnDetected = v1alpha1.Mask
	p.Rules[0].SensitiveData.OnUninspectable = v1alpha1.Block
	return p
}

func TestAdaptiveNormalAndLegacySelection(t *testing.T) {
	for _, normal := range []bool{false, true} {
		r, in := routingSetup(t, routingConfig())
		p := contextPolicy("c", v1alpha1.Enforce, "economy")
		c := p.Rules[0].Routing.Context
		c.MaxSimpleInputTokens = 1
		if normal {
			c.NormalModel, c.MaxNormalInputTokens = "private", 1000
		}
		installRoutingPolicies(r, p)
		got, err := r.RouteRequest(context.Background(), in)
		want := "premium"
		if normal {
			want = "private"
		}
		if err != nil || got.Model != want {
			t.Fatalf("normal=%v: model=%s err=%v", normal, got.Model, err)
		}
	}
}

func TestAdaptiveMaskCompletesBeforeReturningChain(t *testing.T) {
	r, in := routingSetup(t, routingConfig())
	protectedInput(&in)
	original := bytes.Clone(in.RawBody)
	installRoutingPolicies(r, maskPolicy(), privatePolicy("internal", "private"))
	calls := 0
	setAdaptiveRedactor(t, r, func(_ context.Context, protocol string, raw []byte) ([]byte, error) {
		calls++
		if protocol != in.Protocol || !bytes.Equal(raw, original) {
			t.Fatal("redactor did not receive original ingress")
		}
		// Deliberately mutate the buffer to prove the router protects its caller.
		clean := bytes.ReplaceAll(raw, []byte("person@example.test"), []byte("REDACTED"))
		copy(raw, bytes.Repeat([]byte{'x'}, len(raw)))
		return clean, nil
	})
	got, err := r.RouteRequest(context.Background(), in)
	if err != nil || len(got.Chain) == 0 || got.Model != "private" || calls != 1 {
		t.Fatalf("mask + internal obligation not completed: %+v %v", got, err)
	}
	if !bytes.Equal(in.RawBody, original) {
		t.Fatal("masking mutated original request")
	}
	masked := reflect.ValueOf(got.Decision).FieldByName("Masked")
	body := reflect.ValueOf(got).FieldByName("SanitizedBody")
	if !masked.IsValid() || !masked.Bool() || !body.IsValid() || bytes.Contains(body.Bytes(), []byte("person@example.test")) {
		t.Fatal("result omitted completed sanitized body/evidence")
	}
	for _, ct := range got.Chain {
		if ct.DataBoundary != "internal" {
			t.Fatal("masking relaxed an independent InternalOnly rule")
		}
	}
}

func TestAdaptiveMaskFailuresNeverExposeChain(t *testing.T) {
	for _, kind := range []string{"nil", "error", "residual", "opaque", "malformed"} {
		t.Run(kind, func(t *testing.T) {
			r, in := routingSetup(t, routingConfig())
			protectedInput(&in)
			installRoutingPolicies(r, maskPolicy())
			if kind != "nil" {
				setAdaptiveRedactor(t, r, func(_ context.Context, _ string, raw []byte) ([]byte, error) {
					switch kind {
					case "error":
						return nil, errors.New("private detail")
					case "residual":
						return raw, nil
					case "opaque":
						return []byte(`{"messages":[{"role":"user","content":[{"type":"image","source":{"data":"AA=="}}]}]}`), nil
					default:
						return []byte(`{`), nil
					}
				})
			}
			got, err := r.RouteRequest(context.Background(), in)
			requireDenied(t, got, err, "mask_failed")
			if strings.Contains(err.Error(), "private detail") {
				t.Fatal("redactor error leaked")
			}
		})
	}
}

func TestAdaptiveBudgetConstrainsOriginalSourceEveryAttempt(t *testing.T) {
	r, in := routingSetup(t, routingConfig())
	// Simulate legacy substitution before RouteRequest. The strict map must
	// still match premium, the original authorized source.
	in.Model = "economy"
	in.Chain, in.State, _ = r.ResolveChain(in.Model)
	setBudgetConstraint(t, r, map[string]string{"premium": "private-alias"})
	p := contextPolicy("context", v1alpha1.Enforce, "premium")
	p.Rules[0].Routing.Context.FromModels = []string{"economy"}
	installRoutingPolicies(r, p)
	got, err := r.RouteRequest(context.Background(), in)
	if err != nil || got.Model != "private" || len(got.Chain) != 3 {
		t.Fatalf("strict source/target lost: %+v %v", got, err)
	}
	for _, ct := range got.Chain {
		if ct.Model != "private" {
			t.Fatal("expensive fallback escaped budget target")
		}
	}
}

func TestAdaptiveBudgetNoSafeIntersectionDenies(t *testing.T) {
	for _, target := range []string{"", "missing", "premium"} {
		r, in := routingSetup(t, routingConfig())
		protectedInput(&in)
		setBudgetConstraint(t, r, map[string]string{"premium": target})
		installRoutingPolicies(r, privatePolicy("internal", "private"))
		got, err := r.RouteRequest(context.Background(), in)
		if err == nil || len(got.Chain) != 0 {
			t.Fatalf("unusable strict target %q escaped privacy: %+v", target, got)
		}
	}
}

func TestAdaptiveExplicitStabilityEnablesCompatibleToolsHistory(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		cfg := routingConfig()
		m := cfg.Models["economy"]
		m.Capabilities = []string{"tools"}
		cfg.Models["economy"] = m
		r, in := routingSetup(t, cfg)
		in.RawBody = []byte(`{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"ok"},{"role":"user","content":"next"}],"tools":[{"name":"f","input_schema":{"type":"object"}}],"max_tokens":32}`)
		p := contextPolicy("context", v1alpha1.Enforce, "economy")
		if enabled {
			p.Rules[0].Routing.Context.Stability = &policy.ContextStability{MinHold: time.Minute, MinRequests: 3, SessionTTL: time.Hour}
		}
		installRoutingPolicies(r, p)
		got, err := r.RouteRequest(context.Background(), in)
		want := "premium"
		if enabled {
			want = "economy"
		}
		if err != nil || got.Model != want {
			t.Fatalf("stability=%v: %+v %v", enabled, got, err)
		}
	}
}

func TestAdaptiveBudgetValidatesEvenAlreadySubstitutedTarget(t *testing.T) {
	for _, missing := range []string{"window", "price", "capability"} {
		t.Run(missing, func(t *testing.T) {
			cfg := routingConfig()
			switch missing {
			case "window":
				m := cfg.Models["economy"]
				m.ContextWindow = 0
				cfg.Models["economy"] = m
			case "price":
				delete(cfg.Pricing.Overrides["private"], "economy-upstream")
			}
			r, in := routingSetup(t, cfg)
			in.Model = "economy"
			in.Chain, in.State, _ = r.ResolveChain(in.Model)
			if missing == "capability" {
				in.RawBody = []byte(`{"messages":[{"role":"user","content":"hi"}],"tools":[{"name":"f","input_schema":{"type":"object"}}]}`)
			}
			setBudgetConstraint(t, r, map[string]string{"premium": "economy"})
			got, err := r.RouteRequest(context.Background(), in)
			if err == nil || len(got.Chain) != 0 {
				t.Fatalf("strict target inherited legacy %s exemption: %+v", missing, got)
			}
		})
	}
}

func TestAdaptiveMaskBlockWinsWithoutCallingRedactor(t *testing.T) {
	r, in := routingSetup(t, routingConfig())
	protectedInput(&in)
	block := privatePolicy("block")
	block.Rules[0].SensitiveData.OnDetected = v1alpha1.Block
	installRoutingPolicies(r, maskPolicy(), block)
	calls := 0
	r.SetRequestRedactor(adaptiveRedactor(func(context.Context, string, []byte) ([]byte, error) {
		calls++
		return nil, nil
	}))
	got, err := r.RouteRequest(context.Background(), in)
	requireDenied(t, got, err, "sensitive_blocked")
	if calls != 0 {
		t.Fatal("redactor ran despite unconditional block")
	}
}

func TestAdaptiveBudgetOriginalAuthorizationPrecedesConstraint(t *testing.T) {
	r, in := routingSetup(t, routingConfig())
	in.Principal.AllowedModels = []string{"economy"}
	setBudgetConstraint(t, r, map[string]string{"premium": ""})
	got, err := r.RouteRequest(context.Background(), in)
	requireDenied(t, got, err, "model_forbidden")
}

func TestAdaptiveThreeClassBoundaries(t *testing.T) {
	for _, tt := range []struct {
		input   int64
		keyword bool
		want    string
	}{
		{100, false, "economy"}, {101, false, "private"}, {1000, false, "private"},
		{1001, false, "premium"}, {1, true, "premium"}, {500, true, "premium"},
	} {
		r, in := routingSetup(t, routingConfig())
		p := contextPolicy("context", v1alpha1.Enforce, "economy")
		c := p.Rules[0].Routing.Context
		c.MaxSimpleInputTokens, c.MaxNormalInputTokens, c.NormalModel = 100, 1000, "private"
		installRoutingPolicies(r, p)
		r.SetRequestInspector(inspectFunc(func(context.Context, string, []byte) (sensitivity.Result, error) {
			return sensitivity.Result{Complete: true, UserTurns: 1, InputTokens: tt.input}, nil
		}))
		if tt.keyword {
			in.RawBody = []byte(`{"messages":[{"role":"user","content":"security"}]}`)
		}
		got, err := r.RouteRequest(context.Background(), in)
		if err != nil || got.Model != tt.want {
			t.Fatalf("input=%d keyword=%v: %+v %v", tt.input, tt.keyword, got, err)
		}
	}
}

func TestAdaptiveResponsesRequiresNativeAttestation(t *testing.T) {
	p := ingressCapableProvider{routingProvider{mockprovider.New("upstream"), "openai_responses"}, true}
	if !requestCompatible("responses", p, routingConfig().Models["premium"].Targets[0], &sensitivity.Result{Complete: true}, false) {
		t.Fatal("native Responses attestation was refused")
	}
	p.supported = false
	if requestCompatible("responses", p, routingConfig().Models["premium"].Targets[0], nil, false) {
		t.Fatal("Responses provider refusal was ignored")
	}
	p.supported, p.name = true, "anthropic"
	if requestCompatible("responses", p, routingConfig().Models["premium"].Targets[0], nil, false) {
		t.Fatal("unimplemented Responses cross-protocol adapter was inferred")
	}
}
