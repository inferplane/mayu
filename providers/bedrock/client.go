// Package bedrock proxies to Amazon Bedrock. SDK-specific code is confined to
// awsClient (a thin adapter over aws-sdk-go-v2/service/bedrockruntime); the
// provider logic depends only on the narrow invoker/converser interfaces, so
// tests inject fakes and need no AWS credentials. Registered as type "bedrock".
package bedrock

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	brtypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"github.com/inferplane/inferplane/pkg/schema"
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
	Tools       []ConverseTool
	ToolChoice  ConverseToolChoice
	Inference   map[string]any
	ModelFields map[string]any
}

// ConverseMessage carries the canonical Anthropic content-block vocabulary
// (text | tool_use | tool_result) straight through from converse.go; the SDK
// mapping (including tool_result flattening) happens in buildMessages below.
type ConverseMessage struct {
	Role    string
	Content []schema.ContentBlock
}

// ConverseTool mirrors one entry of Anthropic's "tools" array.
type ConverseTool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

// ConverseToolChoice mirrors Anthropic's tool_choice. Type is one of
// ""|"auto"|"any"|"tool"|"none"; Name is set only for "tool". The zero value
// omits ToolChoice from the Bedrock call entirely (SDK default is auto, and
// some models reject an explicit choice — see buildToolConfig).
type ConverseToolChoice struct {
	Type string
	Name string
}

type ConverseResponse struct {
	Content      []schema.ContentBlock
	StopReason   string
	InputTokens  int64
	OutputTokens int64
}

// Discriminators for ConverseStreamEvent.Kind.
const (
	eventTextDelta      = "text_delta"
	eventToolUseStart   = "tool_use_start"
	eventToolInputDelta = "tool_input_delta"
	eventBlockStop      = "block_stop"
	eventMessageStop    = "message_stop"
	eventUsage          = "usage"
)

// ConverseStreamEvent is a discriminated union over the Bedrock stream events
// the provider cares about; Kind selects which of the other fields are set.
type ConverseStreamEvent struct {
	Kind         string
	TextDelta    string
	ToolUseID    string
	ToolName     string
	ToolDelta    string
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
		ToolConfig:                   buildToolConfig(req.Tools, req.ToolChoice),
		InferenceConfig:              buildInference(req.Inference),
		AdditionalModelRequestFields: buildModelFields(req.ModelFields),
	})
	if err != nil {
		return ConverseResponse{}, fmt.Errorf("bedrock: converse %q: %w", modelID, err)
	}
	resp := ConverseResponse{StopReason: string(out.StopReason)}
	if msg, ok := out.Output.(*brtypes.ConverseOutputMemberMessage); ok {
		resp.Content = contentBlocksFromSDK(msg.Value.Content)
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
		ToolConfig:                   buildToolConfig(req.Tools, req.ToolChoice),
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
			case *brtypes.ConverseStreamOutputMemberContentBlockStart:
				if tu, ok := e.Value.Start.(*brtypes.ContentBlockStartMemberToolUse); ok {
					ev := ConverseStreamEvent{Kind: eventToolUseStart, ToolUseID: aws.ToString(tu.Value.ToolUseId), ToolName: aws.ToString(tu.Value.Name)}
					if !yield(ev, nil) {
						return
					}
				}
			case *brtypes.ConverseStreamOutputMemberContentBlockDelta:
				switch d := e.Value.Delta.(type) {
				case *brtypes.ContentBlockDeltaMemberText:
					if !yield(ConverseStreamEvent{Kind: eventTextDelta, TextDelta: d.Value}, nil) {
						return
					}
				case *brtypes.ContentBlockDeltaMemberToolUse:
					if !yield(ConverseStreamEvent{Kind: eventToolInputDelta, ToolDelta: aws.ToString(d.Value.Input)}, nil) {
						return
					}
				}
			case *brtypes.ConverseStreamOutputMemberContentBlockStop:
				if !yield(ConverseStreamEvent{Kind: eventBlockStop}, nil) {
					return
				}
			case *brtypes.ConverseStreamOutputMemberMessageStop:
				if !yield(ConverseStreamEvent{Kind: eventMessageStop, StopReason: string(e.Value.StopReason)}, nil) {
					return
				}
			case *brtypes.ConverseStreamOutputMemberMetadata:
				if u := e.Value.Usage; u != nil {
					ev := ConverseStreamEvent{
						Kind:         eventUsage,
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

// buildMessages maps the canonical content-block vocabulary to Bedrock SDK
// content blocks. tool_result content is flattened to a single text block
// (Bedrock's ToolResultContentBlock supports text/image/json/document, but
// Claude Code's tool_result payloads are text/JSON strings in practice — richer
// content is out of scope, see providers/bedrock/CLAUDE.md-equivalent notes in
// the design doc). Empty blocks and messages left with no content are dropped:
// Bedrock rejects an empty text block or a message with zero content blocks
// with a ValidationException.
func buildMessages(msgs []ConverseMessage) []brtypes.Message {
	if len(msgs) == 0 {
		return nil
	}
	out := make([]brtypes.Message, 0, len(msgs))
	for _, m := range msgs {
		var content []brtypes.ContentBlock
		for _, b := range m.Content {
			switch b.Type {
			case "text":
				if b.Text != nil && *b.Text != "" {
					content = append(content, &brtypes.ContentBlockMemberText{Value: *b.Text})
				}
			case "tool_use":
				content = append(content, &brtypes.ContentBlockMemberToolUse{Value: brtypes.ToolUseBlock{
					ToolUseId: aws.String(b.ID),
					Name:      aws.String(b.Name),
					Input:     document.NewLazyDocument(rawJSONToAny(b.Input)),
				}})
			case "tool_result":
				status := brtypes.ToolResultStatusSuccess
				if b.IsError != nil && *b.IsError {
					status = brtypes.ToolResultStatusError
				}
				content = append(content, &brtypes.ContentBlockMemberToolResult{Value: brtypes.ToolResultBlock{
					ToolUseId: aws.String(b.ToolUseID),
					Status:    status,
					Content:   []brtypes.ToolResultContentBlock{&brtypes.ToolResultContentBlockMemberText{Value: toolResultText(b.Content)}},
				}})
			}
		}
		if len(content) == 0 {
			continue
		}
		out = append(out, brtypes.Message{Role: brtypes.ConversationRole(m.Role), Content: content})
	}
	return out
}

// toolResultText flattens a tool_result.content value (string OR block array)
// into plain text, the same shape internal/openai's OpenAI conversion uses for
// tool messages — duplicated here rather than imported, since providers/ may
// not depend on internal/ (design §8 provider isolation).
func toolResultText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
		return ""
	}
	if raw[0] == '[' {
		var blocks []schema.ContentBlock
		if json.Unmarshal(raw, &blocks) == nil {
			var out string
			for _, blk := range blocks {
				if blk.Text != nil {
					out += *blk.Text
				}
			}
			return out
		}
	}
	// Any other JSON shape (e.g. an object like {"output":"x"}) falls through
	// here and the raw bytes — braces included — become the block's text
	// verbatim. Intentional: Anthropic's tool_result.content is documented as
	// string or block array only, so this is an edge case, but it's a silent
	// fallthrough — keep it, don't "fix" it into an implicit re-encode that
	// could diverge from the string/array paths above.
	return string(raw)
}

// contentBlocksFromSDK maps a Converse response's content blocks back to the
// canonical vocabulary. Only text and tool_use appear in model output.
func contentBlocksFromSDK(blocks []brtypes.ContentBlock) []schema.ContentBlock {
	var out []schema.ContentBlock
	for _, block := range blocks {
		switch b := block.(type) {
		case *brtypes.ContentBlockMemberText:
			text := b.Value
			out = append(out, schema.ContentBlock{Type: "text", Text: &text})
		case *brtypes.ContentBlockMemberToolUse:
			out = append(out, schema.ContentBlock{
				Type:  "tool_use",
				ID:    aws.ToString(b.Value.ToolUseId),
				Name:  aws.ToString(b.Value.Name),
				Input: documentToRawJSON(b.Value.Input),
			})
		}
	}
	return out
}

// buildToolConfig translates Anthropic tools/tool_choice into a Bedrock
// ToolConfiguration, or nil when there are no tools to send. Only "any" and
// "tool" are forwarded as an explicit ToolChoice — "auto"/unset are left unset
// (the Bedrock default is auto, and some models reject an explicit choice).
// "none" (never call a tool) has no equivalent in Bedrock's ToolChoice union
// (only Auto/Any/Tool — there is no "forbid" member), so the closest faithful
// behavior is to send no ToolConfig at all: a model that isn't offered any
// tools can't call one, which is the outcome "none" asks for.
func buildToolConfig(tools []ConverseTool, choice ConverseToolChoice) *brtypes.ToolConfiguration {
	if len(tools) == 0 || choice.Type == "none" {
		return nil
	}
	cfg := &brtypes.ToolConfiguration{}
	for _, t := range tools {
		cfg.Tools = append(cfg.Tools, &brtypes.ToolMemberToolSpec{Value: brtypes.ToolSpecification{
			Name:        aws.String(t.Name),
			Description: aws.String(t.Description),
			InputSchema: &brtypes.ToolInputSchemaMemberJson{Value: document.NewLazyDocument(rawJSONToAny(t.InputSchema))},
		}})
	}
	switch choice.Type {
	case "any":
		cfg.ToolChoice = &brtypes.ToolChoiceMemberAny{}
	case "tool":
		if choice.Name != "" {
			cfg.ToolChoice = &brtypes.ToolChoiceMemberTool{Value: brtypes.SpecificToolChoice{Name: aws.String(choice.Name)}}
		}
	}
	return cfg
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

// rawJSONToAny decodes a JSON value into a Go value suitable for
// document.NewLazyDocument. Absent/invalid input becomes an empty object,
// which is what an empty tool_use.input ("{}") or a schema-less tool decodes
// to anyway.
func rawJSONToAny(raw json.RawMessage) any {
	if len(raw) == 0 {
		return map[string]any{}
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return map[string]any{}
	}
	return v
}

// documentToRawJSON reads back a Bedrock response document (e.g. a
// tool_use.input the model produced) as JSON for the canonical ContentBlock.
func documentToRawJSON(doc document.Interface) json.RawMessage {
	if doc == nil {
		return json.RawMessage("{}")
	}
	var v any
	if err := doc.UnmarshalSmithyDocument(&v); err != nil {
		return json.RawMessage("{}")
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("{}")
	}
	return raw
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
