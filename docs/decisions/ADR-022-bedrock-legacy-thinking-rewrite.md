# ADR-022: Bedrock legacy extended-thinking rewrite for newer models

**Date:** 2026-07-14
**Status:** Accepted (implemented).
**Related:** `providers/bedrock/errors.go` (ADR-021's sibling fix — accurate upstream status
instead of a generic 502 — is what made this bug diagnosable in the first place: the
`400 ValidationException` this ADR fixes used to be flattened into an opaque 502).

## Context

Claude Code (and the standard, pre-adaptive-thinking Anthropic Messages API) sends
extended thinking as `thinking: {"type": "enabled", "budget_tokens": N}`. Newer Claude
models — confirmed live against `aws bedrock-runtime invoke-model` and cross-checked
against Anthropic's own model-support matrix — reject this shape outright with a 400
`ValidationException`: `claude-opus-4-7`, `claude-opus-4-8`, `claude-fable-5`,
`claude-sonnet-5`, and the Mythos line don't support manual `budget_tokens` at all: "Not
supported (400 error)" is Anthropic's own wording. They require
`thinking: {"type": "adaptive"}` plus a top-level `output_config: {"effort":
"low"|"medium"|"high"}` instead. `claude-sonnet-4-6`/`claude-opus-4-6` still accept the
legacy shape today (deprecated, scheduled for eventual removal); `claude-haiku-4-5`,
`claude-opus-4-5`, and the Claude 3.x line support it with no deprecation notice at all.

This is not a Bedrock quirk — it's the same restriction on Anthropic's direct API. It
only surfaces through inferplane's Bedrock provider because the ingress translates the
client's standard Anthropic-shaped request into a Bedrock InvokeModel call; a client that
talks to Bedrock's native API directly (`CLAUDE_CODE_USE_BEDROCK=1` with no
`ANTHROPIC_BEDROCK_BASE_URL` override) hits the same 400 outside inferplane entirely —
confirmed live, and also confirmed that routing that same env-var combination *through*
inferplane via `ANTHROPIC_BEDROCK_BASE_URL` still produces the exact 400 this ADR fixes,
so the gateway is the right place to close the gap regardless of which client-side mode is
in use.

**Considered and rejected: Mantle.** Claude Code has a purpose-built mode
(`CLAUDE_CODE_USE_MANTLE=1` + `CLAUDE_CODE_SKIP_MANTLE_AUTH=1` +
`ANTHROPIC_BEDROCK_MANTLE_BASE_URL=<gateway>`) for routing through a centralized gateway
that injects AWS credentials server-side, served through a *native Anthropic API shape*
Bedrock endpoint rather than the Invoke API — closer in spirit to what inferplane's
Bedrock provider already does. It maps directly onto the `"mantle"` value
`providers/bedrock/bedrock.go`'s `modelAPI` already recognizes (currently a stub that
falls back to `invoke_model`, deferred from M4/§10 #2). It was not pursued here: Mantle's
model catalog uses different, unversioned IDs (`anthropic.claude-sonnet-5`, no `global.`
prefix) requiring separate config/routing work, and — since Mantle also speaks the real
Anthropic API contract — it would very plausibly hit the identical 400 on these same
models, so it doesn't sidestep this bug anyway. Real Mantle provider support is a
separate, larger follow-up.

## Decision

### `providers/bedrock/thinking.go` — allow-list, not deny-list

`needsAdaptiveRewrite(upstream)` matches upstream model IDs against
`legacyThinkingBrokenModels`, a short list of substrings (`"opus-4-7"`, `"opus-4-8"`,
`"fable-5"`, `"sonnet-5"`, `"mythos"`) confirmed to actually 400 on the legacy shape.
This is an allow-list of confirmed-broken models, not a deny-list of confirmed-working
ones, on purpose: a false-negative (a broken model not yet on the list) just fails exactly
as it did before this ADR — a plain 400, no worse than the status quo. A false-positive
(a working model incorrectly matched) would actively regress currently-functioning
requests, e.g. `claude-haiku-4-5`, which is not listed as supporting `effort` at all in
Anthropic's docs. Given that asymmetry, erring toward under-matching is the safer default.
This list will need upkeep as Anthropic ships more models with this restriction — it is
not something that can be derived structurally from the model name.

`rewriteLegacyThinking` only fires inside `toInvokeBody` (invoke.go) when
`needsAdaptiveRewrite` is true. It parses `thinking` looking for exactly
`{"type": "enabled", ...}`; anything else (absent, unparsable, `"adaptive"`, `"disabled"`,
or any other shape) passes through byte-identical — this is a best-effort compatibility
shim, not a validator, so an unrecognized shape is never turned into a client-visible
error it didn't already have. When it does match, `thinking` becomes
`{"type": "adaptive"}` and a top-level `output_config` is added — but only if the caller
didn't already send one, so an explicit client choice is never clobbered.

### `budget_tokens` → `effort` bucketing is a judgment call, not a spec

Anthropic doesn't publish a token-to-effort equivalence, so the thresholds
(`<=2048 → low`, `<=8192 → medium`, `>8192 → high`, no `budget_tokens` at all → `medium`)
are a documented guess intended to land in the right ballpark, not a precise mapping.
They're isolated in one function (`effortForBudget`) specifically so they can be revisited
without touching the rest of the rewrite logic.

### Why this lives in `toInvokeBody`, not a new file per concern

The rewrite is one more top-level-key operation in the same function that already strips
`model`/`stream` and injects `anthropic_version` — all three exist for the identical
reason (Bedrock's InvokeModel contract differs from the standard Anthropic body the client
sent) and share the identical cache-safety property: parsing only the top level into
`json.RawMessage` never touches the `system`/`messages` VALUES, so the prompt-cache prefix
is preserved exactly as it already was for the pre-existing rewrites (§4.4). The
allow-list/bucket/parse logic itself lives in a separate `thinking.go` file only because
it's independently testable and large enough to clutter `invoke.go` inline.

## Consequences

- **Positive:** `claude-opus-4-7/4-8`, `claude-fable-5`, and `claude-sonnet-5` requests
  with extended thinking enabled now succeed through inferplane instead of 400ing on every
  attempt (observed live, repeatedly, via `ctrust`'s `opusplan` fallback chain retrying and
  re-failing against Opus after Fable failed the same way).
- **This is explicitly a compatibility shim, not a permanent fix.** Once Claude Code (or
  whatever client) sends `thinking: {"type": "adaptive"}` natively, `rewriteLegacyThinking`
  becomes a no-op for that request — nothing needs to be un-done later. If Anthropic's
  model-support matrix changes (a currently-broken model starts accepting the legacy shape,
  or a currently-fine model stops), `legacyThinkingBrokenModels` needs a manual update; there
  is no structural signal in the model ID string itself that would let this be derived
  automatically.
- **Known limitation (accepted): Converse path.** `converse.go`'s `toConverseRequest`
  drops the `thinking` field entirely today (out of scope, unrelated to this fix) and this
  ADR does not change that. In practice this doesn't matter for the models on the
  allow-list: `apiFor`'s default routing sends every Claude-family model (which is all five
  entries on `legacyThinkingBrokenModels`) through `invoke_model`, not `converse`, unless an
  operator explicitly overrides a Claude model's `api` to `"converse"` in config — a narrow,
  self-inflicted edge case, not something this ADR needs to chase.
- **Known limitation (accepted): `thinking: {"type": "disabled"}`.** Anthropic's docs note
  this shape is also unsupported on `fable-5`/Mythos, but that's a different bug than the
  one actually reproduced here (`"enabled"`), and there's no evidence Claude Code sends
  `"disabled"` today. Left untouched; add a case to `rewriteLegacyThinking` if it turns out
  to matter.
- **Out of scope (follow-up, considered and rejected for this ADR):** real Mantle provider
  support (`modelAPI: "mantle"`) — see Context above.

## Verification

`go test ./providers/bedrock/... -race`, `go vet ./...`, `gofmt -l .` clean;
`bash tests/run-all.sh` 67/67. New tests: `thinking_test.go` (allow-list matching including
a `sonnet-4-5` vs `sonnet-5` substring-collision guard, effort bucketing boundaries,
idempotency, no-clobber of an existing `output_config`); `invoke_test.go` additions pin
both directions — a currently-working model's `thinking` field must come back
byte-identical (regression guard) and a broken model's must come back rewritten with the
cache-relevant `system`/`messages` bytes still untouched (bug-fix + cache-invariant
confirmation together, mirroring the existing
`TestToInvokeBodyStripsModelAddsVersionPreservesCachePrefix` pattern).

Manual reproduction used throughout this investigation (and re-run after the fix, against
the redeployed demo, to confirm 400 → 200):

```
curl .../v1/messages -d '{"model":"claude-fable-5","max_tokens":4096,
  "thinking":{"type":"enabled","budget_tokens":1024},
  "messages":[{"role":"user","content":"hi"}]}'
```
