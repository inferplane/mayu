package bedrock

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"strings"

	"github.com/inferplane/inferplane/pkg/schema"
	"github.com/inferplane/inferplane/providers"
)

func init() { providers.Register("bedrock", factory) }

// factory builds a bedrock provider from registry config. region/auth_mode/
// profile come from the per-provider config (main.go fills Settings); model_api
// is an optional JSON map {upstreamModelID: api} gathered from the model targets
// pointing at this provider, used to override the default invoke/converse
// routing. The real AWS client is constructed here — newAWSClient loads the
// default config offline (it does not validate credentials), so registration is
// always exercised in tests.
func factory(cfg providers.Config) (providers.Provider, error) {
	region := cfg.Settings["region"]
	authMode := cfg.Settings["auth_mode"]
	profile := cfg.Settings["profile"]
	ac, err := newAWSClient(context.Background(), region, authMode, profile)
	if err != nil {
		return nil, fmt.Errorf("bedrock: aws config: %w", err)
	}
	modelAPI := map[string]string{}
	if raw := cfg.Settings["model_api"]; raw != "" {
		_ = json.Unmarshal([]byte(raw), &modelAPI)
	}
	// awsClient implements both invoker and converser.
	return &provider{inv: ac, conv: ac, modelAPI: modelAPI}, nil
}

type provider struct {
	inv      invoker
	conv     converser
	modelAPI map[string]string // upstream modelId → "invoke_model"|"converse"|"mantle"
}

func (p *provider) Name() string               { return "bedrock" }
func (p *provider) Models() []schema.ModelInfo { return nil }

// apiFor decides invoke vs converse. Default: Claude models → invoke_model,
// others → converse. Explicit per-model config overrides. "mantle" falls back
// to invoke_model in M4 (§10 #2 spike deferred).
func (p *provider) apiFor(upstream string) string {
	if a, ok := p.modelAPI[upstream]; ok && a != "" {
		if a == "mantle" {
			return "invoke_model" // M4: fallback
		}
		return a
	}
	if strings.Contains(upstream, "anthropic.") || strings.Contains(upstream, "claude") {
		return "invoke_model"
	}
	return "converse"
}

func (p *provider) Complete(ctx context.Context, req *providers.ProxyRequest) (*providers.ProxyResponse, error) {
	if p.apiFor(req.Upstream) == "converse" {
		return p.completeConverse(ctx, req)
	}
	return p.completeInvoke(ctx, req)
}

func (p *provider) Stream(ctx context.Context, req *providers.ProxyRequest) (iter.Seq2[*providers.StreamEvent, error], error) {
	if p.apiFor(req.Upstream) == "converse" {
		return p.streamConverse(ctx, req)
	}
	return p.streamInvoke(ctx, req)
}

var _ providers.Provider = (*provider)(nil)
