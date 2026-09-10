// Package routingtest contains socket-free fixtures shared by ingress tests.
// Only provider I/O is replaced; routing, inspection, state and policies are real.
package routingtest

import (
	"context"
	"errors"
	"iter"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/inferplane/inferplane/api/v1alpha1"
	"github.com/inferplane/inferplane/internal/config"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/live"
	"github.com/inferplane/inferplane/internal/policy"
	"github.com/inferplane/inferplane/internal/principal"
	"github.com/inferplane/inferplane/internal/router"
	"github.com/inferplane/inferplane/pkg/schema"
	"github.com/inferplane/inferplane/providers"
)

type Call struct {
	Context               context.Context
	Body, Model, Protocol string
}

type Spy struct {
	Calls         []Call
	Fail, Partial bool
	Usage         *schema.Usage
}

func (*Spy) Name() string                { return "mock" }
func (*Spy) SupportsIngress(string) bool { return true }
func (*Spy) Models() []schema.ModelInfo  { return nil }
func (s *Spy) record(ctx context.Context, r *providers.ProxyRequest) {
	s.Calls = append(s.Calls, Call{ctx, string(r.RawBody), r.Model, r.IngressProtocol})
}
func (s *Spy) Complete(ctx context.Context, r *providers.ProxyRequest) (*providers.ProxyResponse, error) {
	s.record(ctx, r)
	if s.Fail {
		return nil, errors.New("transport failed")
	}
	return &providers.ProxyResponse{StatusCode: 200, RawBody: []byte(`{"id":"ok","type":"message","role":"assistant","content":[]}`), Parsed: &schema.ChatResponse{ID: "ok", Role: "assistant", Usage: s.Usage}}, nil
}
func (s *Spy) Stream(ctx context.Context, r *providers.ProxyRequest) (iter.Seq2[*providers.StreamEvent, error], error) {
	s.record(ctx, r)
	if s.Fail {
		return nil, errors.New("transport failed")
	}
	return func(yield func(*providers.StreamEvent, error) bool) {
		if !yield(&providers.StreamEvent{Raw: []byte("event: message_start\ndata: {\"type\":\"message_start\"}\n\n"), Chunk: &schema.ChatChunk{Type: "message_start", Message: &schema.ChatResponse{ID: "ok", Role: "assistant", Usage: s.Usage}}}, nil) {
			return
		}
		if s.Partial {
			yield(nil, errors.New("interrupted"))
			return
		}
		yield(&providers.StreamEvent{Raw: []byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"), Chunk: &schema.ChatChunk{Type: "message_stop"}}, nil)
	}, nil
}
func (s *Spy) CountTokens(ctx context.Context, r *providers.ProxyRequest) (int64, error) {
	s.record(ctx, r)
	return 777, nil
}

func init() {
	providers.Register("task4-spy", func(providers.Config) (providers.Provider, error) { return &Spy{}, nil })
}

type Fixture struct {
	Router                 *router.Router
	Holder                 *live.Holder
	Config                 *config.Config
	Public, Private, Retry *Spy
	Policies               []*policy.Policy
	LookupError            error
	Lookups                int
}

func New(t *testing.T, edit func(*config.Config)) *Fixture {
	t.Helper()
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"public":  {Type: "task4-spy", DataBoundary: "external", Region: "us"},
			"private": {Type: "task4-spy", DataBoundary: "internal", Region: "eu"},
			"retry":   {Type: "task4-spy", DataBoundary: "internal", Region: "eu"},
		},
		Models: map[string]config.ModelConfig{
			"premium": {ContextWindow: 100000, Capabilities: []string{"tools"}, Targets: []config.Target{{Provider: "public", Model: "up"}}},
			"private": {ContextWindow: 100000, Capabilities: []string{"tools"}, Targets: []config.Target{{Provider: "private", Model: "up"}, {Provider: "public", Model: "up"}, {Provider: "retry", Model: "up"}}},
			"economy": {ContextWindow: 100000, Capabilities: []string{"tools"}, Targets: []config.Target{{Provider: "private", Model: "cheap"}}},
		},
		ModelFallbacks: map[string]string{"private": "premium", "unrouted": "premium"},
		Pricing: config.PricingConfig{Overrides: map[string]map[string]config.RateConfig{
			"public": {"up": {Free: true}}, "private": {"up": {Free: true}, "cheap": {Free: true}}, "retry": {"up": {Free: true}},
		}},
	}
	if edit != nil {
		edit(cfg)
	}
	st, _, err := live.BuildState(cfg)
	if err != nil {
		t.Fatal(err)
	}
	holder := &live.Holder{}
	holder.Swap(st)
	f := &Fixture{Router: router.New(holder), Holder: holder, Config: cfg}
	a, _ := st.Provider("public")
	f.Public = a.(*Spy)
	b, _ := st.Provider("private")
	f.Private = b.(*Spy)
	c, _ := st.Provider("retry")
	f.Retry = c.(*Spy)
	f.Router.SetRoutingPolicyLookup(func(team, user string) ([]*policy.Policy, error) {
		if team != "team" || user != "user" {
			t.Errorf("lookup subject = %q/%q", team, user)
		}
		f.Lookups++
		return f.Policies, f.LookupError
	})
	return f
}

func Privacy(action v1alpha1.SensitiveDataAction) *policy.Policy {
	return &policy.Policy{Name: "privacy", Generation: 7, Rules: []policy.Rule{{Name: "protect", SensitiveData: &policy.SensitiveData{OnDetected: action, OnUninspectable: action, InternalModels: []string{"private"}}}}}
}

func Context(mode v1alpha1.ContextMode) *policy.Policy {
	return &policy.Policy{Name: "selection", Generation: 8, Rules: []policy.Rule{{Name: "short", Routing: &policy.Routing{Context: &policy.Context{Mode: mode, FromModels: []string{"premium"}, SimpleModel: "economy", ComplexModel: "premium", MaxSimpleInputTokens: 1000}}}}}
}

func Request(path, raw string) *http.Request {
	r := httptest.NewRequest("POST", path, strings.NewReader(raw))
	r.SetPathValue("modelId", "premium")
	return r.WithContext(principal.With(r.Context(), keystore.Principal{KeyID: "opaque-key", Team: "team", AllowedModels: []string{"*"}, KeyOptions: keystore.KeyOptions{Owner: "user"}}))
}

// Bodies cover decoded system text, tool results and nested numeric tool input.
func Body(protocol, surface string, stream bool) string {
	content := `[{"role":"user","content":"hello"}]`
	extra := ""
	switch surface {
	case "system":
		if protocol == "openai" {
			content = `[{"role":"system","content":"person@example.test"},{"role":"user","content":"hello"}]`
		} else {
			extra = `,"system":"person@example.test"`
		}
	case "tool":
		if protocol == "openai" {
			content = `[{"role":"tool","tool_call_id":"x","content":"person@example.test"},{"role":"user","content":"hello"}]`
		} else {
			content = `[{"role":"user","content":[{"type":"tool_result","tool_use_id":"x","content":"person@example.test"}]}]`
		}
	case "numeric":
		if protocol == "openai" {
			content = `[{"role":"assistant","tool_calls":[{"id":"x","type":"function","function":{"name":"pay","arguments":"{\"card\":4111111111111111}"}}]},{"role":"user","content":"hello"}]`
		} else {
			content = `[{"role":"assistant","content":[{"type":"tool_use","id":"x","name":"pay","input":{"card":4111111111111111}}]},{"role":"user","content":"hello"}]`
		}
	}
	s := "false"
	if stream {
		s = "true"
	}
	return `{"model":"premium","max_tokens":16,"stream":` + s + `,"messages":` + content + extra + `}`
}
