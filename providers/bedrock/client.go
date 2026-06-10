// Package bedrock proxies to Amazon Bedrock. SDK-specific code is confined to
// awsClient (a thin adapter over aws-sdk-go-v2/service/bedrockruntime); the
// provider logic depends only on the narrow invoker/converser interfaces, so
// tests inject fakes and need no AWS credentials. Registered as type "bedrock".
package bedrock

import (
	"context"
	"fmt"
	"iter"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	brtypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
)

// invoker is the narrow interface the provider logic depends on for the raw
// InvokeModel path. It carries the request/response body verbatim so the cache
// invariant is preserved end to end. Implemented by awsClient (real SDK) and by
// fakes in tests.
type invoker interface {
	Invoke(ctx context.Context, modelID string, body []byte) ([]byte, error)
	InvokeStream(ctx context.Context, modelID string, body []byte) (iter.Seq2[[]byte, error], error)
}

// converser is the narrow interface the provider logic depends on for the
// Converse path. Requests and responses are canonical (non-SDK) types so no
// caller ever touches aws-sdk-go-v2. Implemented by awsClient and by fakes.
type converser interface {
	Converse(ctx context.Context, modelID string, req ConverseRequest) (ConverseResponse, error)
	ConverseStream(ctx context.Context, modelID string, req ConverseRequest) (iter.Seq2[ConverseStreamEvent, error], error)
}

type ConverseRequest struct {
	System      string
	Messages    []ConverseMessage
	Inference   map[string]any
	ModelFields map[string]any
}
type ConverseMessage struct {
	Role string
	Text string
}
type ConverseResponse struct {
	Text         string
	StopReason   string
	InputTokens  int64
	OutputTokens int64
}
type ConverseStreamEvent struct {
	TextDelta    string
	Done         bool
	StopReason   string
	InputTokens  int64
	OutputTokens int64
}

// awsClient is the only type in this package that touches aws-sdk-go-v2. Every
// SDK type stays behind these methods; callers see only the narrow interfaces.
type awsClient struct {
	rt *bedrockruntime.Client
}

// newAWSClient loads the default AWS config (env, shared config, IMDS, ...)
// scoped to region, optionally pinned to a named profile when authMode is
// "profile", and returns an adapter over the bedrockruntime client. It is
// exercised only at the manual gate (it needs real credentials); unit tests use
// the fakes instead.
func newAWSClient(ctx context.Context, region, authMode, profile string) (*awsClient, error) {
	opts := []func(*config.LoadOptions) error{config.WithRegion(region)}
	if authMode == "profile" && profile != "" {
		opts = append(opts, config.WithSharedConfigProfile(profile))
	}
	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("bedrock: load aws config: %w", err)
	}
	return &awsClient{rt: bedrockruntime.NewFromConfig(cfg)}, nil
}

func (c *awsClient) Invoke(ctx context.Context, modelID string, body []byte) ([]byte, error) {
	out, err := c.rt.InvokeModel(ctx, &bedrockruntime.InvokeModelInput{
		ModelId:     aws.String(modelID),
		Body:        body,
		ContentType: aws.String("application/json"),
		Accept:      aws.String("application/json"),
	})
	if err != nil {
		return nil, fmt.Errorf("bedrock: invoke model %q: %w", modelID, err)
	}
	return out.Body, nil
}

func (c *awsClient) InvokeStream(ctx context.Context, modelID string, body []byte) (iter.Seq2[[]byte, error], error) {
	out, err := c.rt.InvokeModelWithResponseStream(ctx, &bedrockruntime.InvokeModelWithResponseStreamInput{
		ModelId:     aws.String(modelID),
		Body:        body,
		ContentType: aws.String("application/json"),
		Accept:      aws.String("application/json"),
	})
	if err != nil {
		return nil, fmt.Errorf("bedrock: invoke model stream %q: %w", modelID, err)
	}
	return func(yield func([]byte, error) bool) {
		stream := out.GetStream()
		defer stream.Close()
		for event := range stream.Events() {
			if chunk, ok := event.(*brtypes.ResponseStreamMemberChunk); ok {
				if !yield(chunk.Value.Bytes, nil) {
					return
				}
			}
		}
		if err := stream.Err(); err != nil {
			yield(nil, fmt.Errorf("bedrock: invoke model stream %q: %w", modelID, err))
		}
	}, nil
}

func (c *awsClient) Converse(ctx context.Context, modelID string, req ConverseRequest) (ConverseResponse, error) {
	out, err := c.rt.Converse(ctx, &bedrockruntime.ConverseInput{
		ModelId:                      aws.String(modelID),
		System:                       buildSystem(req.System),
		Messages:                     buildMessages(req.Messages),
		InferenceConfig:              buildInference(req.Inference),
		AdditionalModelRequestFields: buildModelFields(req.ModelFields),
	})
	if err != nil {
		return ConverseResponse{}, fmt.Errorf("bedrock: converse %q: %w", modelID, err)
	}
	resp := ConverseResponse{StopReason: string(out.StopReason)}
	if msg, ok := out.Output.(*brtypes.ConverseOutputMemberMessage); ok {
		for _, block := range msg.Value.Content {
			if text, ok := block.(*brtypes.ContentBlockMemberText); ok {
				resp.Text += text.Value
			}
		}
	}
	if out.Usage != nil {
		resp.InputTokens = int64(aws.ToInt32(out.Usage.InputTokens))
		resp.OutputTokens = int64(aws.ToInt32(out.Usage.OutputTokens))
	}
	return resp, nil
}

func (c *awsClient) ConverseStream(ctx context.Context, modelID string, req ConverseRequest) (iter.Seq2[ConverseStreamEvent, error], error) {
	out, err := c.rt.ConverseStream(ctx, &bedrockruntime.ConverseStreamInput{
		ModelId:                      aws.String(modelID),
		System:                       buildSystem(req.System),
		Messages:                     buildMessages(req.Messages),
		InferenceConfig:              buildInference(req.Inference),
		AdditionalModelRequestFields: buildModelFields(req.ModelFields),
	})
	if err != nil {
		return nil, fmt.Errorf("bedrock: converse stream %q: %w", modelID, err)
	}
	return func(yield func(ConverseStreamEvent, error) bool) {
		stream := out.GetStream()
		defer stream.Close()
		for event := range stream.Events() {
			switch e := event.(type) {
			case *brtypes.ConverseStreamOutputMemberContentBlockDelta:
				if text, ok := e.Value.Delta.(*brtypes.ContentBlockDeltaMemberText); ok {
					if !yield(ConverseStreamEvent{TextDelta: text.Value}, nil) {
						return
					}
				}
			case *brtypes.ConverseStreamOutputMemberMessageStop:
				if !yield(ConverseStreamEvent{Done: true, StopReason: string(e.Value.StopReason)}, nil) {
					return
				}
			case *brtypes.ConverseStreamOutputMemberMetadata:
				if u := e.Value.Usage; u != nil {
					ev := ConverseStreamEvent{
						InputTokens:  int64(aws.ToInt32(u.InputTokens)),
						OutputTokens: int64(aws.ToInt32(u.OutputTokens)),
					}
					if !yield(ev, nil) {
						return
					}
				}
			}
		}
		if err := stream.Err(); err != nil {
			yield(ConverseStreamEvent{}, fmt.Errorf("bedrock: converse stream %q: %w", modelID, err))
		}
	}, nil
}

func buildSystem(system string) []brtypes.SystemContentBlock {
	if system == "" {
		return nil
	}
	return []brtypes.SystemContentBlock{
		&brtypes.SystemContentBlockMemberText{Value: system},
	}
}

func buildMessages(msgs []ConverseMessage) []brtypes.Message {
	if len(msgs) == 0 {
		return nil
	}
	out := make([]brtypes.Message, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, brtypes.Message{
			Role:    brtypes.ConversationRole(m.Role),
			Content: []brtypes.ContentBlock{&brtypes.ContentBlockMemberText{Value: m.Text}},
		})
	}
	return out
}

func buildInference(inf map[string]any) *brtypes.InferenceConfiguration {
	if len(inf) == 0 {
		return nil
	}
	cfg := &brtypes.InferenceConfiguration{}
	if v, ok := asInt32(inf["maxTokens"]); ok {
		cfg.MaxTokens = aws.Int32(v)
	}
	if v, ok := asFloat32(inf["temperature"]); ok {
		cfg.Temperature = aws.Float32(v)
	}
	if v, ok := asFloat32(inf["topP"]); ok {
		cfg.TopP = aws.Float32(v)
	}
	if seqs, ok := inf["stopSequences"].([]string); ok {
		cfg.StopSequences = seqs
	} else if raw, ok := inf["stopSequences"].([]any); ok {
		for _, s := range raw {
			if str, ok := s.(string); ok {
				cfg.StopSequences = append(cfg.StopSequences, str)
			}
		}
	}
	return cfg
}

func buildModelFields(fields map[string]any) document.Interface {
	if len(fields) == 0 {
		return nil
	}
	return document.NewLazyDocument(fields)
}

// asInt32 coerces JSON-decoded numerics (float64) and common integer types to
// int32. Callers populate Inference from decoded JSON, so float64 is the usual
// dynamic type.
func asInt32(v any) (int32, bool) {
	switch n := v.(type) {
	case int32:
		return n, true
	case int:
		return int32(n), true
	case int64:
		return int32(n), true
	case float64:
		return int32(n), true
	case float32:
		return int32(n), true
	default:
		return 0, false
	}
}

func asFloat32(v any) (float32, bool) {
	switch n := v.(type) {
	case float32:
		return n, true
	case float64:
		return float32(n), true
	case int:
		return float32(n), true
	case int32:
		return float32(n), true
	case int64:
		return float32(n), true
	default:
		return 0, false
	}
}

var (
	_ invoker   = (*awsClient)(nil)
	_ converser = (*awsClient)(nil)
)
