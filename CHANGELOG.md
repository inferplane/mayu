# Changelog

All notable changes to this project will be documented in this file.
The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- **Budget-tier model substitution** (ADR-041): a `GovernancePolicy` `routing.budgetTiers` rule substitutes a cheaper, self-hosted target (e.g. Sonnet → GLM on vLLM) for designated requested-model names once a named team budget rule crosses an operator-set utilization threshold — judged GLOBALLY by the control plane from the ADR-034 lease ledger, latched per budget window so it never flaps request-to-request. Substitution is keyed by requested model name (never touches a long-conversation/main-loop model, which is simply left out of the map, so live prompt caches are never invalidated), can only narrow — never widen — access, and never turns into a denial: a target that fails RBAC or isn't routed leaves the original model served. Substituted requests advertise `x-inferplane-substituted-model`, the audit record's `model_substituted_from` carries the client's original request, and `inferplane_model_substitution_total` counts it. New `internal/tier` package (`Table` + window-latched `Latch`); `routing.onAffinityConflict` (cache-affinity, still unenforceable) and `routing.budgetTiers` are now mutually-exclusive halves of the same rule kind.
- **`inferplane login` / `token` / `logout`** (ADR-028): OIDC login for humans — trades an IdP session for an automatically-renewing, short-lived gateway virtual key instead of a hand-copied, never-expiring one. New opt-in data-plane endpoints `GET /v1/auth/config`, `POST /v1/auth/key`, `DELETE /v1/auth/key`; a second, distinct OIDC client from the admin console's; no IdP refresh token is ever cached on disk. CI/service-account provisioning (`inferplane keys create`, declarative `virtual_keys`) is unchanged.
- **Model-level fallback** (ADR-029): a hardcoded client requesting a not-yet-configured model (e.g. a new Claude release) is now served a configured model instead of 404ing, via config `model_fallbacks` or — with no config at all — a default same-family heuristic (`claude-opus-5` → the highest configured `claude-opus-*` version below it). A configured model whose upstream rejects it as unknown (404, or Bedrock `ValidationException`) also crosses to its fallback model within the existing priority-fallback chain. Substitution is fail-closed on RBAC (a key allowed only the requested name is denied, never silently downgraded) and advertised via `x-inferplane-model-fallback`.

### Fixed
- **An OpenAI-wire stream that ended without a `finish_reason` reached Anthropic-speaking clients with no `stop_reason` at all.** `message_delta` is the only Anthropic frame that carries one, and it was emitted only when the upstream sent a `finish_reason` (or a usage-only chunk) of its own — a vLLM/Mantle stream that simply ran to `[DONE]` jumped straight from `content_block_stop` to `message_stop`, leaving the client unable to distinguish a completed turn from a truncated one. `ReadChatSSE` now synthesizes a terminal `end_turn` delta in exactly that case; any upstream message-level frame still suppresses it, since a consumer may treat the first `message_delta` it sees as end-of-message.
- **`google.gemma-4-*` was unusable on Bedrock Mantle.** Route selection is per-vendor segment, which put every `google.*` model on the bare `/v1/chat/completions` route — but gemma-4 answers only on `/openai/v1/chat/completions` and 400s "isn't supported on this route" elsewhere (probed live 2026-09-02). It is now routed there as a per-model exception; `google.gemma-3-*` is unaffected.
- **Converse usage for the OpenAI gpt-5.6 family double-counted cached tokens.** Bedrock reports that family's `inputTokens` INCLUSIVE of the cache read/write counts (OpenAI `prompt_tokens` semantics — verified on live traffic 2026-08-29→31: input = cache_read + cache_write + Δ across 20+ consecutive requests), while the Anthropic wire requires the three counts disjoint. Passing the inclusive value through made clients (Claude Code) compute context usage at ~2x the real prompt — a 1M-context model looked nearly full after a few turns — and made settlement bill every cached token twice (full input rate on top of the cache-read/-write rates), the egress mirror of the OpenAI-ingress bug ADR-030 fixed in `usageFromOAI`. `usageWithCache` now subtracts the cache counts back out for the `converseInclusiveInputUsage` allow-list (clamped at 0, never over-billing); unlisted families pass through untouched — Claude on Converse reports disjoint counts (ADR-030) and other implicit-caching families are unverified.
- **Cost settlement was wrong in five independent ways, producing 0 µUSD on real traffic** (ADR-030). Streaming requests billed **output tokens only** — input and prompt-cache counts arrive on `message_start`, which the settlement path never read (measured: 5 µUSD where 52 was correct, a 10.4x under-bill, and Claude Code traffic is effectively all streaming). Cache writes always billed at the cheaper 5-minute tier, leaving the 1-hour tier (2x input) unreachable. A config declaring only `input_per_mtok`/`output_per_mtok` billed **all cache tokens at zero** — cache rates are now derived from the input rate (0.1x / 1.25x / 2x, verified against both Anthropic's and Bedrock's published tables). Bedrock cross-region prefixes (`global.`/`us.`/`apac.`) missed the rate table entirely, and Bedrock Converse dropped cache tokens while InvokeModel kept them. The OpenAI-compatible ingress billed cached prompt tokens at the full input rate (over-billing). A stream that broke mid-flight billed nothing for tokens already delivered.
- `pricing.on_missing: "block"` was dead config — it behaved identically to `allow`, so unpriced traffic an operator believed was refused was served free. It is now enforced at boot and at runtime, and an unrecognized value is a load error instead of silently meaning `allow`.
- A non-admin OIDC identity issuing a key via `POST /admin/keys` could set `owner` to any value, letting a teammate attribute a key to someone else; the server now always overrides `owner` to the caller's own verified subject.

### Changed
- **BREAKING (only when `pricing.on_missing` is `"block"`):** the gateway now refuses to boot if any configured route has no pricing rate, naming the routes. With the default `allow` it logs them loudly and continues. Migration: declare the missing rates under `pricing.overrides` (two numbers per model — cache rates derive), or set `on_missing` to `"allow"`.
- **BREAKING:** a `pricing.overrides` entry declaring `0` for both `input_per_mtok` and `output_per_mtok` is now a load error instead of a silently accepted zero rate. `0/0` used to read as "this model is free", which is exactly how unpriced traffic ended up billing nothing. Migration: declare the real rates, or add `"free": true` to the entry — the only way to assert a genuinely zero-cost model.
- Bedrock's `mantle` egress now REFUSES a request whose model has a Guardrail configured (provider default or per-team override) instead of serving it unguarded. Mantle has no guardrail parameter, and the audit record attests the configured guardrail regardless — a silent bypass was being recorded as compliant. Route guarded models via `converse`/`invoke_model`, or drop the guardrail for that model.
- `pricing.version` labels the rate table and lands in every audit record's `cost.pricing_version`, which was previously the hardcoded string `"bundled"` even for fully-overridden tables — a disputed invoice can now be pinned to the rates that produced it.

## [0.2.0] - 2026-06-14

### Added
- **Free OIDC SSO for the admin plane** (ADR-004): the gateway validates IdP ID tokens (Dex/Keycloak/Okta) against the issuer's JWKS and maps the `groups` claim to teams; the static admin token remains as break-glass. Resource-server-only — no redirect/session/cookie, no CSP change.
- **Config hot-reload** (ADR-006): `SIGHUP` re-reads config and atomically swaps the provider/model/pricing topology with no restart; governance counters, keystore, and audit chain persist; a bad config rolls back.
- **Provider visibility** (ADR-005): read-only `GET /admin/config` and a console **Providers** tab show wired providers, endpoints, auth modes, and model routing — never a secret value.
- **Console operator dashboard** (ADR-002): token-gated SPA with Overview, Virtual keys, Providers, Governance, and Quickstart tabs; data-free static assets behind a strict CSP.
- **Governance views + one-click audit verify** (ADR-003 #2): per-team quota-utilization gauge and cumulative budget spend, plus `GET /admin/audit/verify` (per-sink hash-chain check, complete-prefix tolerant of a live writer).
- **Chargeback report** (ADR-007): `inferplane report` aggregates settled µUSD by team (or resolved model) from the audit log to CSV — exact integer-micros money, no float drift.
- **Per-team admin authorization + admin-action audit** (ADR-004): OIDC team-members issue/revoke keys only for their teams; every admin mutation and denial is an audit event.

### Changed
- Admin key management, config view, and audit verify are unified behind a single `AdminAuth` accepting static tokens or OIDC ID tokens on one bearer header.

## [0.1.0]

### Added
- Anthropic Messages ingress (`/v1/messages`, `/v1/messages/count_tokens`, `/v1/models`) with verbatim, cache-safe body forwarding.
- OpenAI Chat Completions ingress (`/v1/chat/completions`, `/v1/models`) with canonical-schema conversion.
- Virtual keys (`ik_...`) with team RBAC and per-key allowed-model lists; SHA-256 hashed at rest, shown once.
- Two-phase governance: per-team rate limits (TPM/RPM), daily token quotas, and monthly USD budgets with `block`/`warn` policies.
- Integer-microUSD pricing with round-half-even and TTL-tiered prompt-cache rates; `on_missing: allow` for self-hosted chargeback.
- Tamper-evident audit log: per-instance SHA-256 hash chain, disk WAL (`buffer_then_block`), and the `inferplane audit verify` command.
- Providers: Anthropic direct, Amazon Bedrock (Claude via InvokeModel, others via Converse), and any OpenAI-compatible endpoint, with priority fallback and per-provider circuit breakers.
- Prometheus `/metrics` on the admin plane using OpenTelemetry GenAI semantic conventions, plus a 9-panel Grafana dashboard.
- Optional self-terminated TLS on the data plane for non-Kubernetes deployments.
- Packaging: multi-stage `CGO_ENABLED=0` static Docker image (distroless/nonroot) and a Helm chart (ConfigMap config, IRSA ServiceAccount, `existingSecret` reference).

### Security
- Config rejects inline secrets; credentials are referenced only via `env:`/`file:`/`secret:`.
- The gateway never forwards the client key upstream and never exposes its upstream keys to clients.
- `/metrics` carries no secret or `key_id`, and bounds label cardinality with a `_rejected` sentinel on pre-resolution 403/404 paths.
- `count_tokens` always returns 200 to avoid crashing Claude Code.

[0.2.0]: https://github.com/inferplane/inferplane/releases/tag/v0.2.0
[0.1.0]: https://github.com/inferplane/inferplane/releases/tag/v0.1.0
