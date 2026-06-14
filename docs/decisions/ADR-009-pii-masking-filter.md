# ADR-009: Opt-in PII masking filter — the honest cache/cost trade-off

**Date:** 2026-06-14
**Status:** Accepted — 2-round design gate (gemini 3.1-pro + kiro; codex
empty-streamed every round on this machine, skipped). Round 1: 2 CRITICAL
(cross-protocol `Parsed`-leak, fail-open masker error) + clarifs folded in.
Round 2: both CLOSED (gemini + kiro), 1 NEW CRITICAL (OpenAI-ingress bypass)
folded in (reject masked-team OpenAI ingress). Implementation = Stage B.
**Related:** spec ⑥ (filter chain), §4.4 (cache invariant — verbatim body
forwarding), §308–310 (mask = explicit opt-in + cost warning), §7 (no secret/PII
at rest), ADR-003 (differentiation vs LiteLLM), `plugins/` top-level dir (spec
file tree)

## Context

The design reserves a v0.2+ **filter chain** (spec ⑥) whose first plugin is PII
masking. The defining constraint is the **cache invariant (§4.4)**: when the
provider protocol equals the ingress protocol, the gateway forwards the request
body **verbatim** (`RawBody`) so `cache_control` / prompt-cache prefixes are
never corrupted — Claude Code traffic is ~96% cache-hit, and a broken cache costs
the user up to **10×** (cache read = 10% of base input).

PII masking **mutates the request body** (replacing detected PII in message text),
which forces a re-serialize and **destroys verbatim forwarding → destroys the
prompt cache** for masked traffic. Rival gateways mask silently; the spec
(§308–310) mandates the opposite: masking is allowed **only as an explicit
opt-in, with the cost impact warned in docs and surfaced at runtime**. Honesty
about the trade-off is the differentiator (ADR-003), not a bug to hide.

## Decision

**A request filter chain with an opt-in, regex-based PII masking plugin that
(a) makes cache destruction explicit and (b) never stores PII.**

### 1. Filter chain + plugin isolation

A new top-level `plugins/` dir (sibling to `providers/`, per the spec file tree).
`plugins/piimask/` is a self-contained package; the core exposes a tiny
request-filter interface (`internal/filter`):

```go
type RequestFilter interface {
    Name() string
    // Mask returns the masked message text and the number of redactions; it
    // operates on extracted text only, never on cache_control / tool blocks /
    // structural fields.
    Mask(text string) (string, int)
}
```

Plugins register by name (registry like `providers`); a provider/plugin PR adds
**one package under `plugins/<name>/`** plus a blank import — zero core diff
(provider-isolation mandate extended to plugins).

### 2. Opt-in, per-team or global; cache destruction is explicit

Masking is enabled via config (`plugins: [{ name: "pii-mask", teams: [...] }]`,
or global). When enabled for a request's team:

- The handler masks **text content blocks only** and updates **BOTH the
  forwarded `RawBody` AND the parsed `pr.Parsed`** (gate round-1 CRITICAL): the
  bedrock provider re-parses `RawBody`, but the openai_compatible provider on a
  non-OpenAI ingress converts from `pr.Parsed` (`openai.CanonicalToRequest(req.
  Parsed)`) — masking only `RawBody` would leak unmasked PII through that path.
  The masker produces masked `RawBody`, then re-unmarshals it into `pr.Parsed`
  so every forward path (verbatim tee, bedrock convert, openai convert) sees the
  masked content. Verbatim forwarding is deliberately abandoned for that traffic.
- **Masker errors FAIL CLOSED on `/v1/messages`** (gate round-1 CRITICAL):
  masking is a security control, and the gateway's posture is fail-closed —
  a masker error (e.g. an unexpected body shape) must NOT forward the unmasked
  body. The request is rejected (`400`); it is never sent upstream unmasked.
- **The OpenAI ingress is fail-closed for masked teams** (gate round-2
  CRITICAL): v1 masks the Anthropic ingress (`/v1/messages`) only. A masked team
  must not be able to **bypass the control by switching protocol** — so
  `/v1/chat/completions` (OpenAI ingress) for a masked team is **rejected**
  (`400`, "PII masking is enabled for your team but not supported on the
  OpenAI-compatible endpoint yet; use /v1/messages") until OpenAI-ingress masking
  ships. A protocol the masker cannot redact is refused, never silently passed.
- **The cost/cache impact is surfaced, never silent**: config load emits a
  prominent one-time warning per masked team; a metric
  `inferplane_pii_mask_redactions_total{team}` and a request-scoped audit field
  (`pii_masked: true`, redaction count — never the values) record that masking
  occurred. Docs warn that masked traffic loses prompt-cache hits.
- Unmasked teams keep the verbatim fast path unchanged (zero overhead, full
  cache). Masking is strictly additive and opt-in.

### 3. One-way mask — no PII vault, no un-masking

Detected PII is replaced with **typed placeholders** (`‹EMAIL›`, `‹PHONE›`,
`‹CARD›`, `‹SSN›`, `‹IP›`). The model sees only masked text; the response
naturally carries the placeholders. The gateway **never stores the
original↔placeholder mapping** — storing PII would make the gateway a PII vault /
breach target, directly against the §7 no-secret-at-rest posture. Masking is
one-way by design; there is no restore step (and none is possible across
streaming without a vault).

Default detectors (each individually toggleable): email, E.164/NA phone,
credit-card (regex + **Luhn** to cut false positives), US SSN, IPv4. Detectors
operate on the `text` field of **user/assistant text blocks** and string-form
`content` only; the filter does NOT descend into `tool_use`/`tool_result` blocks
or `thinking`/`redacted_thinking`, and never touches `cache_control` or other
structural fields (gate round-1 clarification — tool block internals are
out of scope for v1; masking there is a documented follow-up).

Detectors are regex + Luhn, so they are deterministic but not perfect: a
dotted-quad in prose (`v1.2.3.4`) masks as `‹IP›`, etc. — they err toward
**over-masking** (safe side for a PII control), and the behavior is pinned by
false-positive/negative tests so it is known, not surprising.

### 4. Plugin injection (core imports no plugin)

`plugins/piimask` registers itself in its `init()` (blank-imported in
`cmd/inferplane/main.go`, exactly like a provider). At boot the assembly reads
the `plugins` config, resolves the named filter from the registry, and injects
it into the messages/count_tokens handlers as a `filter.RequestFilter`. The
handlers and router import only `internal/filter` (the interface), never
`plugins/piimask` — an import-guard test enforces it. Plugin name registration
happens at process init (before config load), so config validation can reject an
unknown plugin name.

#### 5. count_tokens stays a 200 — without leaking

The `count_tokens` path masks too (so the count reflects what is sent). It must
satisfy BOTH mandates at once: **never a non-200** (count_tokens must not crash
Claude Code) AND **never forward unmasked PII** (gate round-1). So on a masker
error it does NOT fall back to forwarding the unmasked body upstream; instead it
returns a `200` with a **locally-computed token estimate** (no upstream call),
keeping the client alive without leaking. The fail-open fallback is thus replaced
by a fail-safe-but-local one.

## Alternatives considered

1. **Silent masking (LiteLLM-style).** Rejected — mutating the body invisibly
   corrupts the prompt cache and inflicts a silent up-to-10× cost regression; the
   honest opt-in + warning is the stated differentiator (§308–310, ADR-003).
2. **Tokenize-and-restore via a PII vault.** Rejected — to un-mask the response
   the gateway would store the original↔token mapping, i.e. become a PII store
   and breach target (against §7); restoring across streamed frames is also
   unsafe (partial tokens). One-way masking needs no vault.
3. **Mask only when the protocol already differs (cache already broken).**
   Rejected — that leaves the primary same-protocol path unmasked; masking is a
   deliberate security control that must apply independently of protocol.
4. **Header-only / out-of-band redaction.** Rejected — PII lives in the body
   content; headers can't carry it (and the cache key is the body prefix anyway).
5. **Always-on masking.** Rejected — it would impose the cache/cost penalty on
   everyone; the spec mandates opt-in.

## Consequences

- Operators get a real PII control as an explicit, per-team opt-in — with the
  cache/cost cost made visible (warning + metric + audit), never hidden.
- Unmasked traffic is byte-for-byte unchanged (verbatim fast path, full cache).
- A new top-level `plugins/` extension surface mirrors `providers/` isolation: a
  new filter = one package + one blank import, zero core diff.
- No PII is ever stored — masking is one-way; the gateway remains a non-vault.
- Masked responses contain placeholders (the model never saw the real values);
  this is the intended, documented behavior, not a regression.
