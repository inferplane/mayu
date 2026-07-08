# ADR-020: Per-team region locking (D7)

**Date:** 2026-07-08
**Status:** Accepted (implemented).
**Related:** §6.7/§8 D7 of `docs/superpowers/specs/2026-06-26-admin-console-litellm-ux-redesign-design.md`;
ADR-016 (teams as keystore records — this ADR extends the `teams` table and
reuses its fresh-per-request lookup posture); ADR-019 (D6 Bedrock Guardrails —
this ADR reuses the exact same `SetTeamPolicy` setter rather than adding a
second one).

## Context

A team may be legally or contractually required to keep its traffic inside a
declared geographic region (data residency). Nothing in the router or ingress
layer today is aware of "region" as a routing constraint — `ChainTarget` has
no region, and a provider's `Region` config field (originally added for
Bedrock's AWS SDK region parameter) carries no enforcement meaning; it is a
label the provider factory reads, not something governance ever looks at.

## Decision

### 1. Semantics

| Team state | Target state | Outcome |
|---|---|---|
| No `AllowedRegions` policy | any | Unrestricted — current (pre-D7) behavior, byte-for-byte. |
| `AllowedRegions` set | target's `Region` ∈ set | Target kept in the fallback chain. |
| `AllowedRegions` set | target's `Region` ∉ set | Target dropped. |
| `AllowedRegions` set | target's `Region` == "" (unlabeled) | Target dropped — **fail-closed**: an unlabeled provider cannot prove residency, so a restricted team must never reach it even though the same target is perfectly reachable for an unrestricted team. |
| Every target in the chain dropped | — | **403** `permission_error`, `region_blocked` — same status/type family as the existing per-key model-allow-list deny, audited the same way. |

### 2. Region is a generic provider label, not a Bedrock-only concept

`config.ProviderConfig.Region` already existed (added for the Bedrock SDK's
own region parameter) but was read only by `providers/bedrock`'s factory.
`internal/live.State.Region(name string) string` is a new accessor that
exposes it generically — **any** provider type (anthropic, openaicompat,
bedrock) can carry a region label now, because residency is a topology
property of "where does this base_url physically live," not something
specific to one provider implementation. Repurposing the existing field
avoids a second, parallel "residency region" config key that would inevitably
drift from the SDK one for actual Bedrock providers.

### 3. Enforcement point: the ingress handler's resolved chain, not the router

`internal/router.Router.ResolveChain` has no `Principal` in scope — it only
knows the model, not the calling team — so it cannot itself apply a per-team
filter. `internal/router.FilterRegions(chain []ChainTarget, allowed []string)
[]ChainTarget` is a pure function (no team/store dependency) that the ingress
handler calls immediately after `ResolveChain`, mirroring where the existing
per-key model-allow-list check (`p.Allows`) already lives — both are
"this specific caller cannot use this specific resolution" decisions, decided
at the same layer, for the same reason (the router is caller-agnostic by
design; ADR-006's hot-reload snapshot model depends on it staying that way).

### 4. `SetTeamPolicy` reused verbatim — no second setter

ADR-019 introduced `MessagesHandler.SetTeamPolicy`/`ChatHandler.SetTeamPolicy`
specifically anticipating this: "D7/ADR-020's region-lock reuses this exact
same setter." `TeamRecord.AllowedRegions` is read off the *same* fresh
per-request lookup already used for the D6 guardrail override — one keystore
read serves both overrides. Both ingress handlers now do exactly one
`teamPolicy(p.Team)` call per request (down from the two separate calls D6
alone would have needed if this ADR had added a second setter), immediately
after `ResolveChain`, before any billing/masking work — a region-blocked
request is a pre-resolution-shaped deny, not something that should reach
governance pre-check or PII masking first.

### 5. Config-team fallback: the *only* place `TeamRecord` is synthesized from config

D3/ADR-016 established that a DB `teams` record wins wholesale over a config
`teams:` entry of the same name, and that governance reads config-declared
teams through its own `ConfigTeam`/`PoliciesFromConfig` path — never through
`keystore.TeamRecord`. `AllowedRegions` has no such parallel path (it isn't a
governance policy), so `cmd/inferplane/gateway.go`'s `teamPolicy` closure gains
one more branch: when no DB record exists, it synthesizes a
`TeamRecord{AllowedRegions: cfg.Teams[team].AllowedRegions}` so a purely
config-declared team's region policy still enforces. The moment a DB record
for that team is created (even one that sets no `AllowedRegions` at all), it
wins *wholesale* — same ADR-016 precedence — so `PUT`-ing an unrelated field on
a previously config-only team silently drops its config region policy unless
the PUT also carries `allowed_regions`. This is called out here as a sharp
edge, not hidden.

### 6. `teams` table migration: third column onto the same migration list

`allowed_regions TEXT NOT NULL DEFAULT ''` is added to the `teams` schema and
to the `existingColumns`/`applyMigrations` migration list ADR-019 introduced
— no new migration mechanism, just one more entry, stored comma-joined via
the existing `joinModels`/`splitModels` helpers (renamed in spirit, not in
code — they store any string slice, not just model names).

### 7. `count_tokens` guard: never call an out-of-region `TokenCounter`

`CountTokensHandler` gained its own `SetTeamPolicy` (D6 deliberately did NOT
wire one here — a guardrail override has no bearing on token counting). For
D7, wiring it is necessary: skipping it would let a region-restricted team's
`count_tokens` call reach a real upstream `TokenCounter` (e.g. Bedrock's
`CountTokens`) outside its allowed region, sending message content there. The
handler now resolves the full chain, applies `FilterRegions`, and only calls
the real counter on what survives; an empty result falls back to the local
byte-based estimator — the **existing** never-non-200 contract already
required this fallback path to exist for the no-`TokenCounter` case, so this
reuses it rather than adding new fallback logic. A known, documented gap: the
estimate is coarser than the real count for a region-restricted team on a
`TokenCounter`-capable provider; correctness (never leaking region-restricted
content, never a non-200) is prioritized over estimate precision here.

### 8. Capability flag: `region_policy` unconditionally `true`

Same rationale as `teams_records` (ADR-016): the enforcement code path is
always present regardless of whether any team actually has a policy set —
there is no "region locking subsystem" to turn on or off, just a per-team
field that defaults to empty (unrestricted). The field already existed in
`configapi.Capabilities` (stubbed `false` since Phase 2 scaffolding); this ADR
is what makes it real.

## Deferred

- Audit deny-reason taxonomy: `region_blocked` is set as a plain string on
  `OutcomeRef.Error`; a broader enum/taxonomy across all deny reasons
  (allow-list, quota, budget, region) is a larger, separate cleanup.
- Config-file validation of `allowed_regions` entries (shape/charset) — the
  admin-API path (`validateAllowedRegions`) is the only validated write path
  today, matching D3's existing precedent that config-declared teams get no
  validation beyond JSON shape.
- A `FilterRegions` cost metric / audit detail listing which targets were
  dropped and why — today a 403 says "blocked," not "these N targets were
  unlabeled, these M were the wrong region."
