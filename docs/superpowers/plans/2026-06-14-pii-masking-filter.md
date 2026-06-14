# Plan: Opt-in PII masking filter (roadmap #2)

**Date:** 2026-06-14
**Related:** ADR-009 (this plan implements it), spec ⑥/§4.4/§308–310/§7
**Base:** main @ 540584e · **Produces:** the `plugins/` extension surface + pii-mask

## Goal

An **opt-in, per-team** PII masking filter that replaces detected PII in request
**message text** with typed placeholders before forwarding upstream — making the
**cache-destruction cost explicit** (warning + metric + audit), **storing no
PII** (one-way mask, no vault), and leaving **unmasked traffic byte-for-byte
unchanged** (verbatim fast path, full prompt cache).

## Core architecture (from ADR-009)

- **`plugins/` top-level dir** (sibling to `providers/`, spec file tree).
  `plugins/piimask/` is self-contained; a `internal/filter` package holds the
  tiny `RequestFilter` interface + registry. New filter = one package + one
  blank import, zero core diff (provider-isolation extended to plugins).
- **Masking targets text only**: `messages[].content` (string form) and `text`
  blocks within content arrays. **Never** `system` (spec §302 forbids modifying
  the system prompt), **never** `cache_control` / `tool_use` / `tool_result` /
  `thinking` structure. The masker mutates the parsed request JSON in place and
  re-serializes — so it abandons verbatim `RawBody` for that request only.
- **Explicit cost**: enabling pii-mask for a team logs a one-time warning;
  `inferplane_pii_mask_redactions_total{team}` counts redactions; the audit
  record carries `pii_masked` + count (never values). Disabled teams keep the
  verbatim path (no parse, no overhead).
- **One-way, no vault**: typed placeholders (`‹EMAIL›` …); no original↔placeholder
  store; no response un-masking.
- **count_tokens never 500**: masking there is best-effort; on any masker error,
  fall back to the unmasked body.

## Hard safety invariants (the gate's checklist)

- **Masking updates BOTH `RawBody` AND `pr.Parsed`** (gate round-1 CRITICAL):
  bedrock re-parses `RawBody` but openai_compatible (non-OpenAI ingress) converts
  from `pr.Parsed` — masking only one path leaks PII through the other. Pinned by
  a test that routes a masked request through an openai_compatible target and
  asserts the converted upstream body is masked.
- **Fail CLOSED on `/v1/messages`** (gate round-1 CRITICAL): a masker error
  rejects the request (`400`); the unmasked body is NEVER forwarded upstream.
  Pinned by a test (forced masker error → 400, no upstream call).
- **OpenAI ingress is fail-closed for masked teams** (gate round-2 CRITICAL):
  `/v1/chat/completions` for a masked team is rejected (`400`) — v1 masks only
  the Anthropic ingress, and a masked team must not bypass the control by
  switching protocol. Pinned by a test (masked team → /v1/chat/completions → 400,
  no upstream; unmasked team → unchanged).
- **No PII at rest**: no code path persists the original values or a mapping — a
  test asserts the audit record + metric labels carry only a boolean + count, no
  original value. Audit carries `pii_masked` + count only.
- **System prompt untouched** (spec §302): masking never alters `system` — pinned
  by a test (PII in system survives verbatim; PII in messages is masked).
- **Structure preserved**: `cache_control`, `tool_use`/`tool_result`,
  `thinking`/`redacted_thinking` blocks pass through unchanged (only `text` field
  strings in text blocks are masked) — pinned by a round-trip test.
- **Opt-in, zero-overhead-when-off**: with no `plugins`/team-disabled, the
  handler takes the existing verbatim `RawBody` path unchanged (no parse) — pinned
  by a test asserting the body is forwarded byte-for-byte.
- **count_tokens never non-200 AND never leaks** (gate round-1): a masker error
  returns 200 with a LOCAL token estimate (no unmasked upstream forward) — pinned
  by a forced-error test asserting 200 + no upstream call.
- **Plugin isolation**: `internal/server`/`router` gain no import of
  `plugins/piimask`; the masker is injected via the `filter.RequestFilter`
  interface (assembly wires it) — import-guard style test.
- **Luhn-gated cards**: credit-card detection requires a Luhn check (cut false
  positives) — pinned by a test (a 16-digit non-Luhn string is NOT masked).

## Tasks

Each task: failing test → minimal code → refactor; one `git commit -s`; all four
gates green (build, test -race, vet+gofmt, tests/run-all.sh).

- [ ] **T1 — filter interface + registry (`internal/filter`).**
  `RequestFilter` interface (`Name()`, `Mask(text) (string,int)`), a registry
  (`Register`/`Get`), nil-safe. Tests: register/get, unknown name.
  *Files:* `internal/filter/filter.go`, `internal/filter/filter_test.go`.

- [ ] **T2 — piimask detectors + Luhn (`plugins/piimask`).**
  Regex detectors (email, phone E.164/NA, credit-card+Luhn, US SSN, IPv4) →
  typed placeholders; per-detector toggle; `Mask(text)` returns masked text +
  count. Registers as "pii-mask" in `init()` (blank-imported by cmd, like a
  provider). Tests: each detector masks; **non-Luhn card NOT masked**; toggles;
  placeholder format; count correct; no-PII text unchanged; **false-positive
  cases pinned** (dotted-quad in prose masks as ‹IP› — over-masks by design,
  asserted so it's known).
  *Files:* `plugins/piimask/piimask.go`, `plugins/piimask/piimask_test.go`,
  `plugins/CLAUDE.md`.

- [ ] **T3 — request body masker (text-only, updates RawBody + Parsed).**
  `maskBody(raw []byte, f RequestFilter) (masked []byte, n int, err error)` for
  Anthropic ingress JSON: walk **user/assistant** `content` (string + `text`
  blocks only), mask, re-marshal; leave `system` (spec §302), `cache_control`,
  `tool_use`/`tool_result`, `thinking`/`redacted_thinking` untouched. Tests:
  messages text masked, **system untouched**, **tool_use/tool_result NOT
  descended into**, structure preserved (round-trip), invalid JSON → error
  (caller fails closed).
  *Files:* `internal/server/anthropicapi/mask.go`,
  `internal/server/anthropicapi/mask_test.go`.

- [ ] **T4 — config: `plugins` block + per-team enable.**
  `PluginConfig{Name string, Teams []string}` + `Config.Plugins []PluginConfig`;
  a resolved `MaskedTeams` set / "global" flag. Validation: unknown plugin name
  rejected at load. Tests: parse, per-team vs global, unknown plugin rejected.
  *Files:* `internal/config/config.go`, `internal/config/config_test.go`.

- [ ] **T5 — wire masking into /v1/messages (fail-closed) + metric + audit + inject.**
  The handler takes an injected `filter.RequestFilter` (nil = off). If the
  principal's team is masked: `maskBody` → set `pr.RawBody = masked` AND
  `json.Unmarshal(masked, pr.Parsed)` (both paths masked, gate CRITICAL); on a
  masker error **reject `400`, never forward unmasked** (fail closed, gate
  CRITICAL); bump `inferplane_pii_mask_redactions_total{team}`; set audit
  `pii_masked` + count (no values). Disabled/nil → the existing verbatim path,
  unchanged (no parse, no overhead). Assembly resolves the filter from config +
  registry and logs the one-time enable warning. Tests: masked team upstream
  body masked (incl. via an openai_compatible target → Parsed masked) + metric/
  audit set; unmasked team byte-for-byte verbatim; masker error → 400, no upstream.
  *Files:* `internal/server/anthropicapi/messages.go`, `internal/metrics/*`,
  `internal/audit/record.go` (add `pii_masked`), `cmd/inferplane/gateway.go` +
  `cmd/inferplane/main.go` (blank-import + wire filter + warning), tests.

- [ ] **T6 — count_tokens masking (never-500 AND never-leak).**
  Mask the count_tokens body too; on a masker error return `200` with a LOCAL
  token estimate (no unmasked upstream forward) — satisfies both mandates (gate
  CRITICAL). Tests: masked count path 200 + body masked; forced masker error →
  200 + NO upstream call.
  *Files:* `internal/server/anthropicapi/count_tokens.go`, test.

- [ ] **T6b — OpenAI ingress fail-closed for masked teams (gate round-2 CRIT).**
  In the `/v1/chat/completions` handler (`openaiapi`), if the principal's team is
  masked, reject `400` ("PII masking enabled for your team but not supported on
  the OpenAI endpoint yet; use /v1/messages") — never forward unmasked. The
  handler takes the same injected masked-team predicate. Tests: masked team →
  400 + no upstream; unmasked team → unchanged.
  *Files:* `internal/server/openaiapi/chat.go`,
  `internal/server/openaiapi/chat_test.go`.

- [ ] **T7 — docs.** `plugins/CLAUDE.md` (extension surface), `docs/reference/
  api.md` + `agent-llm.md` (filter chain), `docs/architecture.md` (filter +
  cache trade-off), example config with the cost warning; mark ADR-009 Accepted.
  *Files:* docs + `examples/config.json`.

## File scope (allow-list)

```
docs/decisions/ADR-009-pii-masking-filter.md
docs/superpowers/plans/2026-06-14-pii-masking-filter.md
internal/filter/filter.go
internal/filter/filter_test.go
plugins/piimask/piimask.go
plugins/piimask/piimask_test.go
plugins/CLAUDE.md
internal/server/anthropicapi/mask.go
internal/server/anthropicapi/mask_test.go
internal/server/anthropicapi/messages.go
internal/server/anthropicapi/messages_test.go
internal/server/anthropicapi/count_tokens.go
internal/server/anthropicapi/count_tokens_test.go
internal/server/openaiapi/chat.go
internal/server/openaiapi/chat_test.go
internal/server/server.go
internal/config/config.go
internal/config/config_test.go
internal/metrics/metrics.go
internal/metrics/metrics_test.go
internal/audit/record.go
cmd/inferplane/gateway.go
cmd/inferplane/main.go
docs/reference/api.md
docs/reference/agent-llm.md
docs/architecture.md
examples/config.json
```

## Out of scope (explicit)

- PII vault / response un-masking (ADR-009 alt. 2 — would store PII).
- Masking the `system` prompt (spec §302) or OpenAI-ingress bodies (v1: Anthropic
  ingress; OpenAI-ingress masking is a follow-up if demanded).
- ML/NER-based detection (regex + Luhn only for v1 — deterministic, testable).
- Masking streamed RESPONSE content (one-way request masking only).
