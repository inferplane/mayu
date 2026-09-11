# Responses adapter profile

`RequestToCanonical` is an observation adapter. Native `openai_responses`
forwarding uses the original request bytes, with only the selected upstream
model rewritten. Native response/event bytes retain their original phases and
opaque fields. Successful terminal responses require observed usage.

`ResponseUsage(raw)` validates accounting independently of response status and
output decoding. Native nonstreaming `Complete` returns a safe HTTP-502
`ProxyResponse` with `Parsed.Usage` and a nil Go error when a failed, incomplete,
or incompatible HTTP-2xx response contains valid usage. The ingress must settle
that usage before fallback and recheck admission for another attempt. Malformed
JSON or invalid/missing usage never acquires invented accounting. Streaming
`response.incomplete` retains its existing native terminal semantics.

Call `ValidateConversion` before dispatching a Responses request to another
protocol. The supported bridge handles text, system/developer instructions,
client function calls/results, and custom tools such as `apply_patch`. Custom
tools use a canonical function with one required string argument, `input`;
the request-aware response renderer restores their freeform wire format.

The recorded Codex 0.154.0 stateless profile allows
`include: ["reasoning.encrypted_content"]` and `reasoning: {"summary":"auto"}` as
optional output preferences. The bridge omits them on foreign requests and
does not fabricate reasoning summaries or encrypted content. `client_metadata`
is local client bookkeeping and is also omitted from foreign requests.

Provider-owned response/conversation references, explicit stored/background
operations, opaque reasoning/media input, reasoning effort requirements,
built-in tools and unsupported execution semantics remain refused. Other
output-inclusion requests, `text.verbosity` and non-text output formats remain
outside the cross-wire profile.

## Tool namespaces

One namespace level containing client function/custom tools is supported.
Root function names stay unchanged. Namespaced tools become bounded ASCII
function aliases derived from an unambiguous namespace/name encoding and a
SHA-256 digest; declaration ordering does not change them. Alias collisions,
including collisions with root names, are rejected rather than merged.

Canonical input schemas carry bridge-owned annotations for original
namespace/name and custom-tool identity. Caller-provided copies of these
reserved annotations are removed before trusted metadata is generated.
Request-aware response/SSE rendering restores the original `namespace`,
`name`, call ID and custom freeform input. Replayed calls and named tool choices
use the same mapping. Namespaced history requires its corresponding declared
tool; nested namespaces, built-in children and asynchronous/program execution
semantics are refused.

Parallel function/custom call items share one canonical assistant turn until
their results. Interleaved assistant text remains in that turn with its phase
attached to the original blocks; a result or a new non-assistant turn ends the
batch. Replaying the batch through Chat produces one assistant `tool_calls`
array followed by the individual tool results.

## Strict function schemas

Function `strict` must be an explicit boolean when present. Canonical tools
carry it as a declaration field, and Chat rendering writes `function.strict`,
including an explicit false. An omitted Responses function setting is strict:
the bridge makes that default explicit and normalizes object schemas to require
all properties and disallow additional ones, walking schema locations only.
Explicit schemas are preserved, and const/default/enum application data is never
normalized. Native forwarding continues to use the original bytes.

`StrictTools(raw []byte) (bool, error)` reports effective strict requirements
across root and namespaced function declarations. A target without proven
strict support must refuse when it returns true or an error; callers must still
apply `ValidateConversion` for other unsupported features. Custom tools do not
claim function strictness, and an unknown/nonboolean strict setting refuses.

## Assistant phase

The adapter preserves explicit assistant phases in canonical message/block
metadata. Nonstreaming Responses rendering infers `commentary` for unphased
text when the same response contains a tool call, and `final_answer` otherwise.
Explicit phases always win.

A stream cannot know whether later blocks will call a tool. Unphased text starts
as provisional `commentary`, and text deltas stream immediately. Its phase-bearing
`response.output_item.done` is deferred until a tool call is seen or the response
ends. A tool turn completes that item as `commentary`; a text-only turn completes
it as `final_answer`. The terminal response uses the same resolved phase.

This is not end-to-end phase preservation across arbitrary protocols. The
existing Chat Completions converter does not serialize canonical
`Message.Extra.phase`; that bridge preserves tool-turn continuity through
assistant/tool message order and call IDs. Native Responses remains verbatim.
Tests exercise both the Responses replay and the actual Chat Completions
conversion, including custom-tool arguments and the final answer after a result.
