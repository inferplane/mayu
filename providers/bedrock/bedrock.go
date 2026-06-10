package bedrock

import (
	"context"
	"iter"
	"strings"

	"github.com/inferplane/inferplane/pkg/schema"
	"github.com/inferplane/inferplane/providers"
)

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
