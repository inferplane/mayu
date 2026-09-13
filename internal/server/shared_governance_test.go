package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/inferplane/inferplane/internal/adminauth"
	"github.com/inferplane/inferplane/internal/budget"
	"github.com/inferplane/inferplane/internal/governance"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/limiter"
	"github.com/inferplane/inferplane/internal/policy"
	"github.com/inferplane/inferplane/internal/server/routingtest"
	"github.com/inferplane/inferplane/pkg/schema"
)

type sharedProbe struct {
	requests []governance.SharedRequest
	finishes []governance.SharedSettlement
	deny     error
}

func (s *sharedProbe) ReserveShared(_ context.Context, r governance.SharedRequest) (*governance.SharedPermit, error) {
	s.requests = append(s.requests, r)
	if s.deny != nil {
		return nil, s.deny
	}
	return &governance.SharedPermit{ID: "permit", TokenBound: r.TokenBound, CostBoundMicroUSD: r.CostBoundMicroUSD}, nil
}
func (s *sharedProbe) FinishShared(_ context.Context, _ *governance.SharedPermit, result governance.SharedSettlement) error {
	s.finishes = append(s.finishes, result)
	return nil
}
func (*sharedProbe) CancelShared(context.Context, *governance.SharedPermit) error { return nil }
func (*sharedProbe) SharedUsage(context.Context, governance.Subject) ([]governance.SharedLimit, error) {
	return nil, nil
}
func (*sharedProbe) Ready(context.Context) error { return nil }

func TestEveryIngressUsesSharedAdmissionAndOriginalPolicyGeneration(t *testing.T) {
	for _, ingress := range authorityIngresses() {
		for _, refused := range []bool{false, true} {
			t.Run(ingress.name()+map[bool]string{false: "/allow", true: "/deny"}[refused], func(t *testing.T) {
				f := routingtest.New(t, nil)
				f.Router.SetRoutingPolicySnapshot(func(string, string) ([]*policy.Policy, string, error) {
					return nil, "captured-generation", nil
				})
				in, out := int64(2), int64(3)
				f.Public.Usage = &schema.Usage{InputTokens: &in, OutputTokens: &out}
				probe := &sharedProbe{}
				if refused {
					probe.deny = &governance.SharedDenial{Status: 429, Reason: "global token quota exceeded", RetryAfter: 12}
				}
				gov := governance.NewGovernor(map[string]governance.TeamPolicy{"team": {TokensPerDay: 1}}, limiter.NewMemory(), budget.NewMemory(), nil)
				gov.SetSharedAuthority(probe)
				rec := policyDo(policyMux(f, nil, gov, nil, nil), ingress.path, authorityBody(ingress), true)
				if len(probe.requests) != 1 {
					t.Fatalf("shared reservation bypassed: status=%d calls=%d", rec.Code, len(probe.requests))
				}
				request := probe.requests[0]
				if request.PolicyGeneration != "captured-generation" || request.Subject.User != "user" || request.TokenBound != 500000 {
					t.Fatalf("lost authority scope/bound: %+v", request)
				}
				if refused {
					if rec.Code != 429 || len(f.Public.Calls) != 0 || len(probe.finishes) != 0 {
						t.Fatal("refused shared request reached provider")
					}
					if got := rec.Header().Get("Retry-After"); got != "12" {
						t.Fatalf("shared backoff lost: got %q want 12", got)
					}
				} else if rec.Code != 200 || len(probe.finishes) != 1 || !probe.finishes[0].Complete ||
					probe.finishes[0].Tokens == nil || *probe.finishes[0].Tokens != 5 {
					t.Fatalf("bad shared settlement: status=%d %+v", rec.Code, probe.finishes)
				}
			})
		}
	}
}

type unavailableKeys struct{ stubStore }

func (unavailableKeys) Resolve(context.Context, string) (keystore.Principal, error) {
	return keystore.Principal{}, keystore.ErrStoreUnavailable
}

func TestSharedKeyOutageRefusesGenerationAndCountsLocally(t *testing.T) {
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("unavailable identity reached handler") })
	h := KeyAuth(unavailableKeys{}, next)
	for _, path := range []string{"/v1/messages", "/v1/chat/completions", "/v1/responses", "/model/m/invoke",
		"/v1/messages/count_tokens", "/model/m/count-tokens"} {
		req := httptest.NewRequest("POST", path, nil)
		req.Header.Set("x-api-key", "test-unavailable")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		want := 503
		if path == "/v1/messages/count_tokens" || path == "/model/m/count-tokens" {
			want = 200
		}
		if rec.Code != want || !json.Valid(rec.Body.Bytes()) {
			t.Fatalf("%s: status=%d body=%s", path, rec.Code, rec.Body.String())
		}
	}
}

func TestAllIngressesUseAuthenticatedTeamSnapshot(t *testing.T) {
	for _, ingress := range authorityIngresses() {
		t.Run(ingress.name(), func(t *testing.T) {
			f := routingtest.New(t, nil)
			in, out := int64(2), int64(1)
			f.Private.Usage = &schema.Usage{InputTokens: &in, OutputTokens: &out}
			f.Router.SetRoutingPolicySnapshot(func(string, string) ([]*policy.Policy, string, error) {
				return nil, "generation", nil
			})
			probe := &sharedProbe{}
			gov := governance.NewGovernor(nil, limiter.NewMemory(), budget.NewMemory(), nil)
			gov.SetSharedAuthority(probe)
			store := stubStore{key: "dev-key", p: keystore.Principal{KeyID: "key", Team: "team", AllowedModels: []string{"*"},
				KeyOptions: keystore.KeyOptions{Owner: "user"}, SharedRevision: "captured-revision",
				TeamSnapshotLoaded: true, TeamSnapshot: &keystore.TeamRecord{Name: "team", AllowedRegions: []string{"eu"}}}}
			lookup := func(string) (keystore.TeamRecord, bool) {
				t.Fatal("shared request re-read mutable team metadata")
				return keystore.TeamRecord{}, false
			}
			mux := DataMux(f.Router, f.Holder, store, nil, gov, nil, nil, lookup, nil, nil, adminauth.MappingConfig{}, nil, 0)
			path := strings.ReplaceAll(ingress.path, "premium", "private")
			body := strings.ReplaceAll(authorityBody(ingress), "premium", "private")
			rec := policyDo(mux, path, body, true)
			if rec.Code != 200 || len(f.Public.Calls) != 0 || len(f.Private.Calls) != 1 {
				t.Fatalf("captured region restriction lost: status=%d public=%d private=%d", rec.Code, len(f.Public.Calls), len(f.Private.Calls))
			}
			if len(probe.requests) != 1 || probe.requests[0].AuthRevision != "captured-revision" {
				t.Fatal("shared admission lost authenticated revision")
			}
		})
	}
}

func TestSharedCountsNeverCallProvider(t *testing.T) {
	f := routingtest.New(t, nil)
	gov := governance.NewGovernor(nil, limiter.NewMemory(), budget.NewMemory(), nil)
	probe := &sharedProbe{}
	gov.SetSharedAuthority(probe)
	for _, protocol := range []string{"anthropic", "bedrock"} {
		path := "/v1/messages/count_tokens"
		if protocol == "bedrock" {
			path = "/model/premium/count-tokens"
		}
		rec := policyDo(policyMux(f, nil, gov, nil, nil), path, countBody(protocol, `{"model":"premium","messages":[{"role":"user","content":"hi"}]}`), true)
		if rec.Code != 200 || len(f.Public.Calls) != 0 || len(probe.requests) != 0 {
			t.Fatal("shared count contacted an authority/provider")
		}
	}
}
