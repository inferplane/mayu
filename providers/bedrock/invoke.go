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
// InvokeModel: drop top-level "model" (it goes in the URL) and inject
// "anthropic_version". Parsing only the TOP LEVEL into json.RawMessage keeps
// every system/messages/tools VALUE byte-identical, so the prompt-cache prefix
// is preserved (§4.4). Top-level key order is irrelevant to the cache.
func toInvokeBody(raw []byte) ([]byte, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, err
	}
	delete(top, "model")
	if _, has := top["anthropic_version"]; !has {
		top["anthropic_version"] = json.RawMessage(bedrockAnthropicVersion)
	}
	return json.Marshal(top)
}

func (p *provider) completeInvoke(ctx context.Context, req *providers.ProxyRequest) (*providers.ProxyResponse, error) {
	body, err := toInvokeBody(req.RawBody)
	if err != nil {
		return nil, fmt.Errorf("bedrock: invoke body: %w", err)
	}
	respBody, err := p.inv.Invoke(ctx, req.Upstream, body)
	if err != nil {
		return nil, fmt.Errorf("bedrock: invoke: %w", err)
	}
	out := &providers.ProxyResponse{StatusCode: 200, RawBody: respBody}
	var parsed schema.ChatResponse
	if json.Unmarshal(respBody, &parsed) == nil {
		out.Parsed = &parsed
	}
	return out, nil
}

func (p *provider) streamInvoke(ctx context.Context, req *providers.ProxyRequest) (iter.Seq2[*providers.StreamEvent, error], error) {
	body, err := toInvokeBody(req.RawBody)
	if err != nil {
		return nil, fmt.Errorf("bedrock: invoke body: %w", err)
	}
	payloads, err := p.inv.InvokeStream(ctx, req.Upstream, body)
	if err != nil {
		return nil, fmt.Errorf("bedrock: invoke stream: %w", err)
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
