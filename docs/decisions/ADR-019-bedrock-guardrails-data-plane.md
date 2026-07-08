# ADR-019: Bedrock Guardrails data-plane application + per-team override (D6)

**Date:** 2026-07-08
**Status:** Accepted (implemented).
**Related:** §6.7/§8 D6 of `docs/superpowers/specs/2026-06-26-admin-console-litellm-ux-redesign-design.md`;
ADR-016 (teams as keystore records — this ADR extends the `teams` table and
reuses its fresh-per-request lookup posture); ADR-009 (the PII-masking filter
seam — the only other content-policy mechanism in the repo, and the thing
this ADR is explicitly NOT reusing, since Guardrails are provider-side, not a
request-transform).

## Context

The spec names Bedrock Guardrails bypass as the substantive problem: a client
configures a Guardrail in the AWS console, but a gateway sitting between the
client and Bedrock can silently not apply it — because applying a guardrail is
a parameter on the `InvokeModel`/`Converse` API call itself, not something a
proxy gets "for free" by forwarding bytes. Before this change, inferplane's
Bedrock provider (`providers/bedrock/`) had no guardrail parameter anywhere —
every invocation went out guardrail-free regardless of what was configured in
AWS. The spec is explicit that the console UI is not the fix; the **data
plane** (the actual SDK call) is.

User-confirmed scope (2026-07-08): both provider-level default AND per-team
override — full spec compliance, not just the anti-bypass minimum.

## Decision

### 1. Provider-level default (the actual anti-bypass fix) + per-team override, no opt-out

`providers/bedrock/bedrock.go`'s factory reads `guardrail_id`/`guardrail_version`
from the provider's config `Settings` (populated by `internal/live.BuildState`
from `config.ProviderConfig.GuardrailID/GuardrailVersion`) into a
`defaultGuardrail` field on `provider`. Every one of the four call paths
(`Invoke`, `InvokeStream`, `Converse`, `ConverseStream`) resolves an
*effective* guardrail via `provider.guardrailFor(req)`: a per-team override
(threaded through `providers.ProxyRequest.GuardrailID/GuardrailVersion`) wins
over the default; with neither, no guardrail fields are set on the SDK call
at all (today's pre-D6 behavior, unchanged when nothing is configured).

**Deliberately no per-team opt-out.** A team record can select a *different*
guardrail (a stricter one, or one scoped to that team's use case) but cannot
clear the provider's default to get an unguarded call — `guardrailFor` has no
"none" branch. This matches the anti-bypass framing: the mechanism this ADR
exists to fix is a *client* (or, here, an intermediate layer) making the
guardrail not apply; letting the team itself disable it would reopen the
identical hole one layer up.

### 2. `providers.ProxyRequest.GuardrailID/GuardrailVersion` — a narrow, explicit exception to provider isolation

CLAUDE.md §8 says a provider PR touches only `providers/<name>/`. A per-team
override, by construction, has to travel from the team record (keystore) →
the ingress handler (which knows the calling team) → the specific provider
call. Two new string fields on the shared `ProxyRequest` transport struct are
the minimal core-diff needed to carry that value across the ingress/provider
boundary; every other provider ignores them (they're plain data, not a
behavior change to any code path outside `providers/bedrock`). This is called
out explicitly as an intentional, narrow exception — not a precedent for
routing arbitrary per-provider config through core structs.

### 3. `Governor`-lookup pattern reused for a non-governance purpose: `SetTeamPolicy`

ADR-016 established `Governor.SetTeamLookup`: a fresh-per-request keystore
read, no caching, so a console edit takes effect on the very next request.
That pattern is exactly what a guardrail override needs too — but a guardrail
override is not a governance decision, so it doesn't belong on `Governor`.
Both `anthropicapi.MessagesHandler` and `openaiapi.ChatHandler` gain a
**separate** setter, `SetTeamPolicy(func(team string) (keystore.TeamRecord, bool))`,
resolved once per request (before the fallback loop, reused across every
fallback attempt) and threaded into each `ProxyRequest`. D7/ADR-020's
region-lock reuses this exact same setter — one lookup, two independent
per-team overrides read off the same `TeamRecord` — rather than adding a
second near-identical setter.

`cmd/inferplane/gateway.go` wires `teamPolicy` as its own closure over
`store.GetTeam`, sitting right next to (but independent from) the governance
`SetTeamLookup` closure. This means a request now does two keystore point
reads instead of one when a team record exists — accepted as negligible next
to the SQLite read `KeyAuth` already does to resolve the principal itself; a
shared per-request cache was considered and rejected as unwarranted
complexity for a sub-millisecond local read.

### 4. `teams` table migration: the first ALTER-TABLE path it has ever needed

ADR-016 shipped `teams` as a brand-new table (`CREATE TABLE IF NOT EXISTS`
only — no pre-existing table to migrate). D6 adds `guardrail_id`/
`guardrail_version` columns to that now-real, possibly-populated table, so
`internal/keystore/sqlite.go`'s `ensureSchema` gains an ALTER-if-missing block
for `teams`, mirroring the one `keys` already had for its D2 governance
columns. The PRAGMA-scan-then-ALTER logic was factored into two small shared
helpers (`existingColumns`, `applyMigrations`) so `keys` and `teams` share the
same migration mechanics instead of duplicating the scan loop a second time.

### 5. Version defaulting: empty → `"DRAFT"`

The Bedrock SDK rejects a `GuardrailIdentifier` with no `GuardrailVersion` set
(a `ValidationException`). `Guardrail.versionOrDraft()` defaults an empty
version to `"DRAFT"` — Bedrock's own default working version — so an operator
who only sets `guardrail_id` (the common case while iterating on a guardrail)
doesn't have to also always specify `"DRAFT"` explicitly.

### 6. Capability flag: `guardrails` keeps its current (PII-masking) meaning

`Capabilities.Guardrails` (spec §4.4) is currently wired as `masking != nil`
— it answers "is the PII-masking filter on," not "is a Bedrock Guardrail
configured." Reshaping it into the richer object the spec sketches
(`guardrails: {...}`) is deferred; per-team guardrail state is instead fully
discoverable through the existing `/admin/teams` surface (gated on
`teams_records`, which is unconditionally `true`). Documented here rather than
silently left inconsistent with the spec's eventual shape.

## Deferred

- `providerstore` (UI-registered providers) guardrail columns / provider-form
  fields — a DB-registered Bedrock provider gets no *provider-level* default
  guardrail yet; a team override still applies to it. Needs a
  `providerstore` column migration + `configapi/write.go` + `overlay.go`
  changes, scoped out to keep this PR reviewable.
- `Trace` / `StreamProcessingMode` guardrail knobs (SDK fields left at zero
  value) — add as `Settings`/team-record fields when a real need for guardrail
  trace output appears.
- Config-file-declared per-team guardrail (`config.TeamConfig` has no
  guardrail field) — the console (`/admin/teams`) is the write path; a
  config-declared team has no override slot, matching D3's own precedent
  (config teams have no DB-only fields either).
- Spec §4.4's `guardrails: {...}` capability object shape.
