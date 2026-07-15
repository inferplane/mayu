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

const bedrockAnthropicVersion = `"bedrock-2023-05-31"`

// toInvokeBody rewrites an Anthropic Messages request body for Bedrock
// InvokeModel: drop top-level "model" (it goes in the URL) and "stream"
// (Bedrock rejects it as an extra input — streaming is selected by the
// InvokeModelWithResponseStream OPERATION), inject "anthropic_version", and —
// only for models on legacyThinkingBrokenModels (thinking.go) — rewrite a
// legacy `thinking: {"type":"enabled",...}` into the adaptive-thinking shape
// those models require instead of rejecting with a 400. Parsing only the TOP
// LEVEL into json.RawMessage keeps every system/messages/tools VALUE
// byte-identical, so the prompt-cache prefix is preserved (§4.4) even with
// the thinking rewrite added — it only touches other top-level keys.
// Top-level key order is irrelevant to the cache.
func toInvokeBody(raw []byte, upstream string) ([]byte, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, err
	}
	delete(top, "model")
	delete(top, "stream")
	if _, has := top["anthropic_version"]; !has {
		top["anthropic_version"] = json.RawMessage(bedrockAnthropicVersion)
	}
	if needsAdaptiveRewrite(upstream) {
		rewriteLegacyThinking(top)
	}
	injectRequiredBetas(top)
	return json.Marshal(top)
}

// featureBetas maps a top-level body feature to the beta Bedrock requires for
// it in the body's anthropic_beta array. The direct Anthropic API takes betas
// as an anthropic-beta HTTP header, which InvokeModel has no equivalent of —
// so a client that thinks it talks to the direct API (Claude Code behind the
// gateway) sends the feature field but no body beta, and Bedrock 400s with
// ValidationException. Only betas for features ACTUALLY present in the body
// are injected: forwarding a client's full beta header list wholesale also
// 400s, because Bedrock rejects beta names it doesn't recognize (both
// behaviors verified against the live service 2026-07-15).
var featureBetas = map[string]string{
	"context_management": "context-management-2025-06-27",
}

func injectRequiredBetas(top map[string]json.RawMessage) {
	var betas []string
	if raw, has := top["anthropic_beta"]; has {
		if json.Unmarshal(raw, &betas) != nil {
			return // client sent a non-list shape; leave it untouched
		}
	}
	added := false
	for feature, beta := range featureBetas {
		if _, has := top[feature]; !has {
			continue
		}
		present := false
		for _, b := range betas {
			if b == beta {
				present = true
				break
			}
		}
		if !present {
			betas = append(betas, beta)
			added = true
		}
	}
	if added {
		if b, err := json.Marshal(betas); err == nil {
			top["anthropic_beta"] = b
		}
	}
}

func (p *provider) completeInvoke(ctx context.Context, req *providers.ProxyRequest) (*providers.ProxyResponse, error) {
	body, err := toInvokeBody(req.RawBody, req.Upstream)
	if err != nil {
		return nil, fmt.Errorf("bedrock: invoke body: %w", err)
	}
	respBody, err := p.inv.Invoke(ctx, req.Upstream, body, p.guardrailFor(req))
	if err != nil {
		// Classify the SDK error into its real upstream status (429 for a
		// throttled model, 503 unavailable, ...) instead of letting it fall
		// through to the ingress's generic 502 — see errors.go.
		return nil, upstreamError(err)
	}
	out := &providers.ProxyResponse{StatusCode: 200, RawBody: respBody}
	var parsed schema.ChatResponse
	if json.Unmarshal(respBody, &parsed) == nil {
		out.Parsed = &parsed
	}
	return out, nil
}

func (p *provider) streamInvoke(ctx context.Context, req *providers.ProxyRequest) (iter.Seq2[*providers.StreamEvent, error], error) {
	body, err := toInvokeBody(req.RawBody, req.Upstream)
	if err != nil {
		return nil, fmt.Errorf("bedrock: invoke body: %w", err)
	}
	payloads, err := p.inv.InvokeStream(ctx, req.Upstream, body, p.guardrailFor(req))
	if err != nil {
		// Pre-TTFT: the stream never opened (e.g. ThrottlingException before
		// any bytes), so its real status can still be teed to the client.
		return nil, upstreamError(err)
	}
	return func(yield func(*providers.StreamEvent, error) bool) {
		for payload, perr := range payloads {
			if perr != nil {
				yield(nil, perr)
				return
			}
			ev := &providers.StreamEvent{}
			var c schema.ChatChunk
			if json.Unmarshal(payload, &c) == nil {
				ev.Chunk = &c
				// re-serialize the parsed chunk as canonical Anthropic SSE
				var b strings.Builder
				if werr := schema.WriteAnthropicSSE(&b, &c); werr == nil {
					ev.Raw = []byte(b.String())
				}
			}
			if ev.Raw == nil {
				// unparseable payload: emit verbatim as a data line (defensive)
				ev.Raw = append(append([]byte("event: unknown\ndata: "), payload...), '\n', '\n')
			}
			if !yield(ev, nil) {
				return
			}
		}
	}, nil
}
