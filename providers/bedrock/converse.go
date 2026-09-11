package bedrock

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"log"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/inferplane/inferplane/pkg/schema"
	"github.com/inferplane/inferplane/providers"
)

func toConverseRequest(raw []byte) (ConverseRequest, error) {
	var body struct {
		MaxTokens     *int64           `json:"max_tokens"`
		Temperature   *float64         `json:"temperature"`
		TopP          *float64         `json:"top_p"`
		StopSequences []string         `json:"stop_sequences"`
		System        json.RawMessage  `json:"system"`
		Messages      []schema.Message `json:"messages"`
		Tools         json.RawMessage  `json:"tools"`
		ToolChoice    json.RawMessage  `json:"tool_choice"`
		ModelFields   map[string]any   `json:"model_fields"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return ConverseRequest{}, err
	}
	cr := ConverseRequest{Inference: map[string]any{}, ModelFields: body.ModelFields}
	if body.MaxTokens != nil {
		cr.Inference["maxTokens"] = *body.MaxTokens
	}
	// Sampling params pass through to InferenceConfig. Keys match the names
	// buildInference (client.go) reads: temperature, topP, stopSequences.
	if body.Temperature != nil {
		cr.Inference["temperature"] = *body.Temperature
	}
	if body.TopP != nil {
		cr.Inference["topP"] = *body.TopP
	}
	if len(body.StopSequences) > 0 {
		cr.Inference["stopSequences"] = body.StopSequences
	}
	cr.System = systemText(body.System)
	// Bedrock's ConversationRole only has user/assistant. Real Claude Code
	// traffic interleaves other roles (observed: "system", for hook output)
	// as ordinary messages — Anthropic's API tolerates this, but Bedrock
	// rejects it outright. The content must stay IN PLACE, merged into the
	// next user message (or a trailing user message — Bedrock wants the last
	// turn to be user anyway). Folding it into the system prompt instead
	// (the previous behavior) mutated the HEAD of the prompt on every turn a
	// hook fired, invalidating the whole prompt-cache prefix: observed live
	// as cache_creation ≈ full input (475k tokens) on every request, with
	// prefill slow enough that Claude Code timed out and fell back to
	// another model.
	var pending []schema.ContentBlock
	for _, m := range body.Messages {
		if m.Role != "user" && m.Role != "assistant" {
			if t := flattenText(m.Content); t != "" {
				txt := t
				pending = append(pending, schema.ContentBlock{Type: "text", Text: &txt})
			}
			continue
		}
		blocks := messageBlocks(m)
		if len(blocks) == 0 {
			continue // Bedrock rejects a message with zero content blocks
		}
		if m.Role == "user" && len(pending) > 0 {
			// tool_result blocks must stay FIRST in a user message — a hook
			// firing between an assistant tool_use and the user tool_result
			// would otherwise put text ahead of them, a shape upstreams
			// reject. Merge the pending text after any leading tool_results.
			lead := 0
			for lead < len(blocks) && blocks[lead].Type == "tool_result" {
				lead++
			}
			merged := make([]schema.ContentBlock, 0, len(blocks)+len(pending))
			merged = append(merged, blocks[:lead]...)
			merged = append(merged, pending...)
			merged = append(merged, blocks[lead:]...)
			blocks = merged
			pending = nil
		}
		cr.Messages = append(cr.Messages, ConverseMessage{Role: m.Role, Content: blocks})
	}
	if len(pending) > 0 {
		// Trailing hook text with no user turn left to attach to. If the last
		// message is already a user turn, extend it — a second consecutive
		// user message is a shape Converse can reject.
		if n := len(cr.Messages); n > 0 && cr.Messages[n-1].Role == "user" {
			cr.Messages[n-1].Content = append(cr.Messages[n-1].Content, pending...)
		} else {
			cr.Messages = append(cr.Messages, ConverseMessage{Role: "user", Content: pending})
		}
	}
	cr.Tools = parseTools(body.Tools)
	cr.ToolChoice = resolveToolChoice(parseToolChoice(body.ToolChoice), cr.Tools)
	return cr, nil
}

// resolveToolChoice drops a "tool" choice that points at a tool parseTools
// already dropped (schema-less, empty-named, over bedrockToolNameMax, or an
// invalid-charset name) —
// otherwise buildToolConfig would send Bedrock a SpecificToolChoice for a
// tool absent from the tool list, which Bedrock rejects with a
// ValidationException. Falling back to the zero value leaves ToolChoice
// unset on the Bedrock call (buildToolConfig, client.go), i.e. auto.
func resolveToolChoice(choice ConverseToolChoice, tools []ConverseTool) ConverseToolChoice {
	if choice.Type != "tool" {
		return choice
	}
	for _, t := range tools {
		if t.Name == choice.Name {
			return choice
		}
	}
	return ConverseToolChoice{}
}

// systemText extracts plain text from an Anthropic system field (string or
// array of text blocks). M4: text only.
func systemText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if raw[0] == '"' {
		var s string
		json.Unmarshal(raw, &s)
		return s
	}
	var blocks []schema.ContentBlock
	json.Unmarshal(raw, &blocks)
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != nil {
			parts = append(parts, *b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// flattenText concatenates a message's text blocks, ignoring tool_use/
// tool_result/thinking/image — used only for folding a non-user/assistant
// role's content into the system prompt (see toConverseRequest).
func flattenText(blocks []schema.ContentBlock) string {
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != nil {
			parts = append(parts, *b.Text)
		}
	}
	return strings.Join(parts, "")
}

// messageBlocks keeps only the block types Bedrock Converse can represent
// (text, tool_use, tool_result); thinking/image/document blocks are dropped
// (out of scope — see the design doc). The SDK-specific mapping, including
// tool_result flattening, happens in buildMessages (client.go).
func messageBlocks(m schema.Message) []schema.ContentBlock {
	var out []schema.ContentBlock
	for _, b := range m.Content {
		switch b.Type {
		case "text", "tool_use", "tool_result":
			out = append(out, b)
		}
	}
	return out
}

// bedrockToolNameMax is Bedrock's ToolSpecification.Name limit (a
// ValidationException, not a soft truncation). Anthropic allows tool names up
// to 128 chars — long MCP-qualified names (e.g.
// "mcp__plugin_foo_bar__some_long_action") are common there and routinely
// exceed 64. Truncating would still break correctly: the client maps a
// tool_use response back to its local tool registry by exact name, so a
// truncated name it doesn't recognize can never be executed. Dropping the
// tool is the same trade-off already made for schema-less tools below — the
// model loses that one capability instead of the whole request failing.
const bedrockToolNameMax = 64

// bedrockToolNameRE is Bedrock's ToolSpecification.Name charset: it must start
// with a letter and contain only letters, digits, and underscores — no
// hyphens, dots, colons, or spaces. Claude Code / MCP tool names routinely
// contain hyphens (e.g. "mcp__aws-sdk-v3__getObject"), which pass every other
// check here but make Bedrock reject the WHOLE request with a
// ValidationException. Same drop-and-continue trade-off as the oversized-name
// case below.
var bedrockToolNameRE = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]*$`)

// parseTools decodes Anthropic's "tools" array into Bedrock-shaped tool specs,
// skipping any tool Bedrock would reject outright: no input_schema (server-tool
// shorthands like computer use can't be expressed as a ToolSpec), an empty,
// over-length, or invalid-charset name, or a JSON "null" input_schema. A
// dropped tool's tool_choice reference is cleaned up separately by
// resolveToolChoice.
func parseTools(raw json.RawMessage) []ConverseTool {
	if len(raw) == 0 {
		return nil
	}
	var tools []struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"input_schema"`
	}
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil
	}
	var out []ConverseTool
	for _, t := range tools {
		// Bedrock's ToolSpecification.Name/InputSchema are both required and
		// non-null: an empty name, a name over the length or charset limit, a
		// missing input_schema, or a JSON "null" input_schema (still 4 bytes,
		// so a bare len==0 check misses it) all produce a ValidationException
		// — skip the tool rather than send one Bedrock will reject.
		if t.Name == "" || len(t.Name) > bedrockToolNameMax || !bedrockToolNameRE.MatchString(t.Name) ||
			len(t.InputSchema) == 0 || string(t.InputSchema) == "null" {
			continue
		}
		out = append(out, ConverseTool{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema})
	}
	return out
}

// parseToolChoice decodes Anthropic's tool_choice object. An absent or
// unrecognized value yields the zero ConverseToolChoice, which
// buildToolConfig (client.go) leaves unset on the Bedrock call.
func parseToolChoice(raw json.RawMessage) ConverseToolChoice {
	if len(raw) == 0 {
		return ConverseToolChoice{}
	}
	var tc struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &tc); err != nil {
		return ConverseToolChoice{}
	}
	return ConverseToolChoice{Type: tc.Type, Name: tc.Name}
}

func (p *provider) completeConverse(ctx context.Context, req *providers.ProxyRequest) (*providers.ProxyResponse, error) {
	cr, err := toConverseRequest(req.RawBody)
	if err != nil {
		return nil, fmt.Errorf("bedrock: converse req: %w", err)
	}
	stripUnsupportedInference(req.Upstream, cr.Inference)
	floorMaxTokens(req.Upstream, cr.Inference)
	cr.Guardrail = p.guardrailFor(req)
	cresp, err := p.conv.Converse(ctx, req.Upstream, cr)
	if err != nil {
		// Classify the SDK error into its real upstream status — see errors.go.
		return nil, upstreamError(err)
	}
	stop := cresp.StopReason
	in, out := cresp.InputTokens, cresp.OutputTokens
	content := cresp.Content
	if content == nil {
		content = []schema.ContentBlock{} // never emit "content":null (real API always sends an array)
	}
	resp := &schema.ChatResponse{
		Type: "message", Role: "assistant", Model: req.Model,
		Content:    content,
		StopReason: &stop,
		Usage: usageWithCache(req.Upstream, in, out,
			cresp.CacheReadTokens, cresp.CacheWrite5m, cresp.CacheWrite1h, cresp.CacheWriteTotal),
	}
	rawBody, _ := json.Marshal(resp)
	return &providers.ProxyResponse{StatusCode: 200, RawBody: rawBody, Parsed: resp}, nil
}

// streamConverse replays Bedrock's Converse event stream as Anthropic SSE. It
// tracks one open content block at a time (lazily opened on the first delta
// that needs it) and re-indexes blocks sequentially, ignoring Bedrock's own
// ContentBlockIndex. The terminal frame (message_delta + message_stop) is only
// emitted once BOTH the stop reason (MessageStop) and usage (a later Metadata
// event) have arrived — Bedrock delivers Metadata strictly after MessageStop,
// so waiting for whichever comes last (instead of returning on MessageStop)
// is what makes streamed usage land in the audit/billing record at all.
func (p *provider) streamConverse(ctx context.Context, req *providers.ProxyRequest) (iter.Seq2[*providers.StreamEvent, error], error) {
	cr, err := toConverseRequest(req.RawBody)
	if err != nil {
		return nil, fmt.Errorf("bedrock: converse req: %w", err)
	}
	stripUnsupportedInference(req.Upstream, cr.Inference)
	floorMaxTokens(req.Upstream, cr.Inference)
	cr.Guardrail = p.guardrailFor(req)
	evs, err := p.conv.ConverseStream(ctx, req.Upstream, cr)
	if err != nil {
		// Pre-TTFT: the stream never opened, so its real status can still be
		// teed to the client (see errors.go).
		return nil, upstreamError(err)
	}
	return func(yield func(*providers.StreamEvent, error) bool) {
		emit := func(c *schema.ChatChunk) bool {
			var b strings.Builder
			schema.WriteAnthropicSSE(&b, c)
			return yield(&providers.StreamEvent{Raw: []byte(b.String()), Chunk: c}, nil)
		}
		if !emit(&schema.ChatChunk{Type: "message_start", Message: &schema.ChatResponse{Type: "message", Role: "assistant", Model: req.Model}}) {
			return
		}

		idx := -1
		blockOpen := false
		var stopReason string
		stopReasonSet := false
		var usageIn, usageOut int64
		var usageCacheRead, usageWrite5m, usageWrite1h, usageWriteTotal int64
		usageSet := false

		finish := func() {
			if blockOpen {
				// Mark closed before emitting: finish is only ever called
				// once (every call site returns right after), but leaving
				// blockOpen stale on an early return here — if the consumer
				// stops iterating mid-emit — is a needless hidden invariant.
				blockOpen = false
				if !emit(&schema.ChatChunk{Type: "content_block_stop", Index: &idx}) {
					// Cannot fall through to message_delta/message_stop here:
					// emit's false means yield already returned false once,
					// and Go's range-over-func contract panics if the loop
					// body calls yield again afterward ("range function
					// continued iteration after function for loop body
					// returned false"). A consumer that stopped mid-stream
					// gets NO further frames from this or any other
					// provider — this is the iterator protocol, not a gap
					// specific to usage settlement. Nothing to do but return.
					return
				}
			}
			delta, _ := json.Marshal(map[string]any{"stop_reason": stopReason, "stop_sequence": nil})
			u := usageWithCache(req.Upstream, usageIn, usageOut, usageCacheRead, usageWrite5m, usageWrite1h, usageWriteTotal)
			if !emit(&schema.ChatChunk{Type: "message_delta", Delta: delta, Usage: u}) {
				return
			}
			emit(&schema.ChatChunk{Type: "message_stop"})
		}

		for e, eerr := range evs {
			if eerr != nil {
				yield(nil, eerr)
				return
			}
			switch e.Kind {
			case eventTextDelta:
				if !blockOpen {
					idx++
					empty := ""
					if !emit(&schema.ChatChunk{Type: "content_block_start", Index: &idx, ContentBlock: &schema.ContentBlock{Type: "text", Text: &empty}}) {
						return
					}
					blockOpen = true
				}
				delta, _ := json.Marshal(map[string]any{"type": "text_delta", "text": e.TextDelta})
				if !emit(&schema.ChatChunk{Type: "content_block_delta", Index: &idx, Delta: delta}) {
					return
				}
			case eventToolUseStart:
				if blockOpen {
					if !emit(&schema.ChatChunk{Type: "content_block_stop", Index: &idx}) {
						return
					}
				}
				idx++
				blockOpen = true
				if !emit(&schema.ChatChunk{Type: "content_block_start", Index: &idx, ContentBlock: &schema.ContentBlock{
					Type: "tool_use", ID: e.ToolUseID, Name: e.ToolName, Input: json.RawMessage("{}"),
				}}) {
					return
				}
			case eventToolInputDelta:
				if idx < 0 {
					// A tool-input delta before any ToolUseStart/text block has
					// opened one — malformed/truncated upstream stream. Emitting
					// content_block_delta with index:-1 would hand the client an
					// invalid SSE frame; discard the orphaned delta instead.
					continue
				}
				delta, _ := json.Marshal(map[string]any{"type": "input_json_delta", "partial_json": e.ToolDelta})
				if !emit(&schema.ChatChunk{Type: "content_block_delta", Index: &idx, Delta: delta}) {
					return
				}
			case eventBlockStop:
				if blockOpen {
					if !emit(&schema.ChatChunk{Type: "content_block_stop", Index: &idx}) {
						return
					}
					blockOpen = false
				}
			case eventMessageStop:
				stopReason = e.StopReason
				stopReasonSet = true
				if usageSet {
					finish()
					return
				}
			case eventUsage:
				usageIn, usageOut = e.InputTokens, e.OutputTokens
				usageCacheRead, usageWriteTotal = e.CacheReadTokens, e.CacheWriteTotal
				usageWrite5m, usageWrite1h = e.CacheWrite5m, e.CacheWrite1h
				usageSet = true
				if stopReasonSet {
					finish()
					return
				}
			}
		}
		// Reaching here means the event channel closed before both
		// MessageStop and Metadata arrived (e.g. no Metadata event at all, or
		// — in principle — a clean close with neither). Flush a terminal
		// frame unconditionally so the client is never left hanging with no
		// message_delta/message_stop at all; whichever of stop
		// reason/usage we didn't receive defaults to its zero value, the
		// same safe default already used for the no-Metadata case.
		finish()
	}, nil
}

// converseInclusiveInputUsage lists upstream-id substrings whose Converse
// TokenUsage.InputTokens INCLUDES the cache read/write counts (OpenAI
// prompt_tokens semantics), verified against live gateway traffic
// (ap-northeast-2, 2026-08-29→31): input = cacheRead + cacheWrite + Δ held
// across 20+ consecutive gpt-5.6-sol requests. The Anthropic wire requires
// the three counts DISJOINT — passing the inclusive value through makes a
// client's context math run at ~2x the real prompt and settle bill every
// cached token twice (full input rate + cache rate), the egress mirror of
// the OpenAI-ingress bug ADR-030 fixed in usageFromOAI. Same evidence-based
// allow-list posture as converseUnsupportedInference: an unlisted family
// passes through untouched (Claude on Converse reports disjoint counts —
// ADR-030 — and other implicit-caching families are unverified).
var converseInclusiveInputUsage = []string{"openai.gpt-5.6"}

// usageWithCache builds a schema.Usage carrying Bedrock's prompt-cache counts
// alongside input/output for the upstream model id. Zero counts are left nil
// so the emitted JSON stays byte-identical to the pre-cache shape for
// responses without caching.
//
// Bedrock reports a per-TTL breakdown (CacheDetails) and an untiered total; the
// breakdown wins when present, matching how schema.Usage.CacheWriteTiers
// resolves the same ambiguity on the Anthropic wire. Never both, or every cache
// write would be double-counted.
//
// For converseInclusiveInputUsage families the cache counts are subtracted
// back out of InputTokens so the emitted counts are disjoint; a subtraction
// that would go negative clamps to 0 (never over-bill — the unknown-TTL
// posture of cacheWriteTiers).
func usageWithCache(upstream string, in, out, cacheRead, write5m, write1h, writeTotal int64) *schema.Usage {
	writes := writeTotal
	if writes == 0 {
		writes = write5m + write1h
	}
	if cacheRead != 0 || writes != 0 {
		for _, m := range converseInclusiveInputUsage {
			if strings.Contains(upstream, m) {
				in -= cacheRead + writes
				if in < 0 {
					in = 0
				}
				break
			}
		}
	}
	u := &schema.Usage{InputTokens: &in, OutputTokens: &out}
	if cacheRead != 0 {
		u.CacheReadInputTokens = &cacheRead
	}
	if write5m != 0 || write1h != 0 {
		cc := &schema.CacheCreation{}
		if write5m != 0 {
			cc.Ephemeral5mInputTokens = &write5m
		}
		if write1h != 0 {
			cc.Ephemeral1hInputTokens = &write1h
		}
		u.CacheCreation = cc
		return u
	}
	if writeTotal != 0 {
		u.CacheCreationInputTokens = &writeTotal
	}
	return u
}

// converseUnsupportedInference is the upstream-model-ID substring list of
// InferenceConfig keys a model REJECTS with a 400 ValidationException ("This
// model doesn't support the temperature field. Remove temperature and try
// again."), probed per-param against live Bedrock (ap-northeast-2 and
// us-east-1) 2026-08-28. Clients like Claude Code send
// temperature/stop_sequences on most requests, so without stripping these
// every such request 400s and the client silently falls back to another
// model. Same style as legacyThinkingBrokenModels (thinking.go): an
// evidence-based allow-list, NOT a guess — models not listed keep every
// param untouched, so an unrecognized future model fails the same way it did
// before. Extend as models are confirmed broken. Anthropic models are absent
// deliberately: apiFor (bedrock.go) routes them via invoke_model, and their
// param rules are the client's contract, not ours to rewrite.
var converseUnsupportedInference = []struct {
	match  string
	params []string
}{
	// OpenAI gpt-5.6 reasoning models (gpt-5.6-luna/-sol/-terra) reject all
	// sampling params. Neither "openai." nor "openai.gpt-5" is narrow
	// enough: openai.gpt-oss-* ACCEPTS temperature/topP (only stopSequences
	// rejected, below), and openai.gpt-5.4/-5.5 (served only through
	// Bedrock Mantle's OpenAI-schema endpoint, not InvokeModel/Converse)
	// accept every sampling param — probed via
	// bedrock-mantle.us-east-1.api.aws /openai/v1/chat/completions.
	{"openai.gpt-5.6", []string{"temperature", "topP", "stopSequences"}},
	// xai (grok-4.6) rejects all sampling params.
	{"xai.", []string{"temperature", "topP", "stopSequences"}},
	// These accept temperature/topP but reject stopSequences:
	{"openai.gpt-oss", []string{"stopSequences"}},
	// deepseek v3.x only — deepseek r1 accepts all three, and
	// "deepseek.v" deliberately does not match "deepseek.r1-v1:0".
	{"deepseek.v", []string{"stopSequences"}},
	{"google.gemma-", []string{"stopSequences"}},
	{"minimax.", []string{"stopSequences"}},
	{"moonshot", []string{"stopSequences"}}, // moonshot. and moonshotai.
	{"qwen.", []string{"stopSequences"}},
	{"zai.", []string{"stopSequences"}},
}

// converseMinMaxTokens is the upstream-model-ID substring list of models
// that reject a small maxTokens with a 400 ValidationException ("Invalid
// 'max_output_tokens': integer below minimum value. Expected a value >= 16,
// but got 1 instead." — probed live against Bedrock 2026-09-04, ap-northeast-2
// global.openai.gpt-5.6-sol and global.xai.grok-4.6; Mantle-served
// openai.gpt-5.4/-5.5 and zai.glm-5 ACCEPT max_tokens 1). Claude Code sends a
// max_tokens:1 probe request when the user switches models with /model, so
// without this floor every such switch to an affected model fails at the
// switch itself. Same evidence-based allow-list posture as
// converseUnsupportedInference: unlisted models are never touched.
var converseMinMaxTokens = []struct {
	match string
	min   int64
}{
	{"openai.gpt-5.6", 16},
	{"xai.", 16},
}

// floorMaxTokens raises an under-minimum maxTokens to the model's documented
// floor. Raising a client's output cap is a smaller mutation than the
// alternative (a hard 400): the client asked for at most N tokens and may
// receive up to the floor instead — only ever more output, never less, and
// only on the rare requests that asked for less than the floor. Logged once
// per (upstream) like a param strip.
func floorMaxTokens(upstream string, inf map[string]any) {
	for _, e := range converseMinMaxTokens {
		if !strings.Contains(upstream, e.match) {
			continue
		}
		if v, ok := inf["maxTokens"].(int64); ok && v < e.min {
			inf["maxTokens"] = e.min
			logStrippedParam("converse", upstream, "maxTokens<"+strconv.FormatInt(e.min, 10)+" (raised to floor)")
		}
	}
}

// stripUnsupportedInference removes InferenceConfig keys the upstream model
// is known to reject outright (value-independent — gpt-5.6-sol 400s even on
// temperature 1.0, its own default). Dropping a sampling param silently
// changes sampling semantics, but the alternative is a hard 400 on every
// request that carries it.
func stripUnsupportedInference(upstream string, inf map[string]any) {
	for _, e := range converseUnsupportedInference {
		if !strings.Contains(upstream, e.match) {
			continue
		}
		for _, p := range e.params {
			if _, has := inf[p]; has {
				delete(inf, p)
				logStrippedParam("converse", upstream, p)
			}
		}
	}
}

// strippedParamLogged dedupes logStrippedParam to once per (route, upstream,
// param) per process: stripping hits every request an affected model serves,
// so a per-request line is noise — but zero signal left an operator debugging
// a reproducibility issue with no telemetry that the request was mutated
// before egress. A per-request signal (audit/metric) is the fuller fix,
// tracked as P1 "undisclosed request mutation" in docs/enterprise-strategy.md.
var strippedParamLogged sync.Map

func logStrippedParam(route, upstream, param string) {
	key := route + "|" + upstream + "|" + param
	if _, seen := strippedParamLogged.LoadOrStore(key, struct{}{}); !seen {
		log.Printf("bedrock %s: stripping inference param %q for %s (model rejects it — docs/reference/agent-llm.md §5); logged once per model+param", route, param, upstream)
	}
}
