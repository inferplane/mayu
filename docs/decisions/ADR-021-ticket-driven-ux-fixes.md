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

`config.ModelConfig` gains `aliases []string` (config-file path; the providerstore/UI-write
DB path is extended the same way — see "Providerstore alias support" below, added
2026-07-12 as the follow-up this ADR originally deferred). `live.BuildState` (via
`NewState`) builds an alias→canonical map and rejects a config where an alias collides
with a model name or another alias (one hop only). `State.Canonical(name)` resolves an
alias (identity otherwise); `Route` still accepts canonical names only. Both ingresses
and `count_tokens` call `Router.Canonical(...)` **before** the RBAC check, so allow-list,
audit, metrics, and pricing all key off the canonical name — an alias never double-counts.
`Router.Allows(p, model)` (added during code-gate, HIGH finding on PR #25) canonicalizes
`p.AllowedModels` entries too, not just the request: an allow-list holding only an alias
now grants its canonical target instead of a permanent lockout — canonicalizing both sides
is still an exact match, so this closes an operator footgun without widening what a key
can reach (an unrelated allow-list entry still denies).

**Cache-invariant carve-out (the one subtle decision):** the anthropic passthrough
provider forwards `RawBody` verbatim and never rewrote the body `model`, so an alias would
reach the Anthropic API and be rejected (bedrock drops the top-level model; openaicompat
rewrites it — both already safe). On the alias path only (`body model != resolved
Upstream`), the provider now rewrites **only the top-level `model` field**, re-emitting
with `SetEscapeHTML(false)` so nested content bytes (a prompt with `&`/`<`/`>`) and
`cache_control` survive; when `model == Upstream` the body is forwarded byte-identical.
This mirrors the bedrock/openaicompat top-level-rewrite pattern and keeps the cache
invariant intact for the common path.

### 2a. Providerstore alias support (2026-07-12 follow-up)

Guardrails (D6, ADR-019) were a per-PROVIDER attribute, so adding them to the DB path was
one TEXT column + an ALTER-TABLE migration on `providers`. Aliases are a per-MODEL (group)
attribute, and `providerstore.model_targets` is a per-TARGET (fallback-chain position) row —
a column there would store the same alias once per position and orphan it if the chain
shrinks. Instead: a new `model_aliases(model, alias PRIMARY KEY)` table (a brand-new table
needs only `CREATE TABLE IF NOT EXISTS`, no ALTER-TABLE) and a `providerstore.ModelRoute
{Aliases []string; Targets []Target}` DTO replacing the bare `[]Target` the `Store`
interface's `SetModel`/`ListModels`/`Seed` used — the model analog of `ProviderRow`.
`overlay.go` gets `modelConfigFromRoute`/`routeFromModelConfig` (the model-side
`providerConfigFromRow`/`rowFromProviderConfig`), so `Overlay`/`SeedIfEmpty` thread aliases
exactly as they thread guardrails. `configapi.ModelWrite`/`ModelView` gain `aliases`
(mirroring `ProviderWrite`/`ProviderView`'s guardrail fields — non-secret, echoed for
console prefill). A within-write duplicate alias is rejected in `ParseModelWrite`; a
cross-model collision (alias == another model's name, or a duplicate across two writes) can
only be checked against the full topology, so `cmd/inferplane`'s `writeMutation` runs
`config.ValidateModelAliases` (newly exported — it was already `config.LoadRaw`'s internal
guard) on the candidate effective config, the same build-once-swap-once gate
`config.ResolveProviders`/`live.BuildState` already run there. Once overlaid into
`config.ModelConfig`, a DB-registered alias flows through the exact same `Router.Canonical`/
RBAC-before-canonicalization/cache-safe-rewrite path a config-file alias does — zero diff in
`live/router/anthropicapi/openaiapi/anthropic provider`.

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
- **Out of scope (follow-up):** per-key RPM (request-count, as opposed to TPM/token) usage
  reporting in `/v1/usage` — RPM limits return immediate 429 feedback on the blocking
  request itself, so the self-service value of polling it ahead of time is much lower than
  for budget/TPM (which build up silently); add if a real need appears.

## Verification

`go test ./... -race`, `go vet ./...`, `gofmt -l .` clean; `bash tests/run-all.sh` 67/67.
Handler-level wire tests drive the real Router + `live.State` + Governor + providers for
all three features (discoverable 404/403, alias routing + RBAC deny + cache-safe body
rewrite + special-char preservation, `UsageOf` + `/v1/usage` including the nil-governor and
unlimited-dimension paths). The providerstore alias follow-up adds store-level round-trip/
migration tests (`internal/providerstore`), a cross-model-collision write-rejection test at
the `writeMutation` layer (`cmd/inferplane`), and `configapi` parse/view/export alias tests.
