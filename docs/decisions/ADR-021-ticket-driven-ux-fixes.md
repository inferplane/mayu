# ADR-021: Ticket-driven UX fixes — discoverable errors, model aliases, self-service usage

**Date:** 2026-07-10
**Status:** Accepted (implemented).
**Related:** `docs/customer-issue-analysis.md` (59 real Claude Code on Bedrock support
tickets in a LiteLLM gateway environment — the evidence base); ADR-006 (hot-reload —
`live.State` is where the alias map lives); ADR-016 (teams as keystore records — the
team-policy source `UsageOf` reads); ADR-017 (budget alerts — the write-side of the same
budget counters `UsageOf` reads); CLAUDE.md §2.2 canonical-schema invariant, §4.4 cache
invariant, §7 secret-ref mandate, and the `count_tokens`-always-200 / no-`key_id`-leak
mandates.

## Context

The customer-issue analysis surfaced three gaps between what inferplane claimed to solve
and what it actually implemented, each mapping to concrete tickets:

1. **Cryptic model errors** (tickets row 25/41/52 — `Invalid model name passed in`,
   `apac.*` vs `global.*` confusion). The gateway returned `unknown model: X` but never
   told the caller what they *could* use.
2. **No model aliases.** One route = one exact name; a client sending a regional/vendor
   variant of a name got a hard 404 even when the model existed under another name.
3. **Usage-blind clients** (tickets row 2/37/42 — "used up my quota and I didn't even do
   anything"). Spend/quota lived only in Prometheus and the full-admin analytics API; a
   key holder had no way to see their own remaining budget.

## Decision

Three changes, delivered together (host-designed, `co-agent:harness`
codex-implemented, hybrid-gate reviewed).

### 1. Discoverable unknown/disallowed-model errors

Both ingresses append the caller's **allow-list-filtered, sorted, capped** available-model
list to the 404 (unknown model) AND the 403 (model not allowed for this key) messages.
Cap is `maxModelsInError = 20` with an ASCII `...` marker; an empty filtered list yields
`. No models available for this key.`. Status codes, error types, audit, and metrics are
unchanged; the list is filtered by `p.Allows` so it discloses nothing beyond the RBAC-
filtered `/v1/models` already exposes, and never contains `key_id`.

### 2. Config-file model aliases with canonical normalization

`config.ModelConfig` gains `aliases []string` (config-file path only; the providerstore/
UI-write DB DTO is deliberately **out of scope** — a follow-up). `live.BuildState` (via
`NewState`) builds an alias→canonical map and rejects a config where an alias collides
with a model name or another alias (one hop only). `State.Canonical(name)` resolves an
alias (identity otherwise); `Route` still accepts canonical names only. Both ingresses
and `count_tokens` call `Router.Canonical(...)` **before** the RBAC check, so allow-list,
audit, metrics, and pricing all key off the canonical name — an alias never double-counts,
and an alias-only allow-list still denies (no bypass).

**Cache-invariant carve-out (the one subtle decision):** the anthropic passthrough
provider forwards `RawBody` verbatim and never rewrote the body `model`, so an alias would
reach the Anthropic API and be rejected (bedrock drops the top-level model; openaicompat
rewrites it — both already safe). On the alias path only (`body model != resolved
Upstream`), the provider now rewrites **only the top-level `model` field**, re-emitting
with `SetEscapeHTML(false)` so nested content bytes (a prompt with `&`/`<`/`>`) and
`cache_control` survive; when `model == Upstream` the body is forwarded byte-identical.
This mirrors the bedrock/openaicompat top-level-rewrite pattern and keeps the cache
invariant intact for the common path.

### 3. `GET /v1/usage` self-service endpoint

A data-plane endpoint behind the same `KeyAuth` as `/v1/messages`. `Governor.UsageOf(team,
keyID, kp)` (read-only — no debit, no state mutation) reports the caller's **per-key AND
team** budget/quota, including the key's own TPM bucket (`LimiterStore.RateUsed` — a
read-only peek at the same refill math `AllowRate` already does, never writing the bucket
back; added during code-gate after 3 of 4 panel reviewers flagged that a key's TPM limit —
the thing that actually blocks a developer mid-burst — was invisible even though its
*budget* was reported). Each dimension is pointer-typed: an unlimited dimension
(`limit <= 0`, the same "unlimited" convention `budget.Check`/`limiter` already use)
serializes as `null`, never `remaining: 0` (which would falsely read as "used up"). Integer
µUSD/tokens throughout; the response never contains `key_id` (the id is only a store-lookup
key). A nil governor (governance disabled) returns a well-formed ungoverned payload, never
a panic.

## Consequences

- **Positive:** the top three ticket categories (auth aside) are directly addressed;
  clients can self-diagnose model errors and self-check budget/rate limits without an admin.
- **Known limitation (accepted):** `Router.Canonical` and `ResolveChain` each load their
  own `live.State` snapshot, so a hot-reload landing between them could split alias
  resolution from routing. Impact is bounded to a stray 404 (canonical names are stable
  across an alias-only reload) and reloads are rare + validated; not worth threading a
  shared snapshot through the RBAC boundary. Revisit if reloads become frequent.
- **Out of scope (follow-up):** aliases in the providerstore/UI-write DB path; per-key
  RPM (request-count, as opposed to TPM/token) usage reporting in `/v1/usage` — RPM limits
  return immediate 429 feedback on the blocking request itself, so the self-service value of
  polling it ahead of time is much lower than for budget/TPM (which build up silently); add
  if a real need appears.

## Verification

`go test ./... -race`, `go vet ./...`, `gofmt -l .` clean; `bash tests/run-all.sh` 67/67.
Handler-level wire tests drive the real Router + `live.State` + Governor + providers for
all three features (discoverable 404/403, alias routing + RBAC deny + cache-safe body
rewrite + special-char preservation, `UsageOf` + `/v1/usage` including the nil-governor and
unlimited-dimension paths).
