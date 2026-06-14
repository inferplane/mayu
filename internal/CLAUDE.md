# internal Module

## Role
Private gateway internals — everything that is not a provider or a public package.
Packages are leaf-oriented to avoid import cycles (`principal`, `metrics`, `governance`
are leaves that others depend on).

## Key Packages
- `server/` — HTTP data plane + admin plane; `anthropicapi/`, `openaiapi/`, `adminapi/` ingress handlers; `adminui/` embedded key console (`/admin/ui/`, data-free static assets, ADR-001); `configapi/` read-only secret-free topology view (`GET /admin/config`, ADR-005); `auditapi/` `GET /admin/audit/verify` per-sink chain check (ADR-003 #2); `auth.go`, `adminauth.go`, `tls.go`, `metricsapi.go`.
- `router/` — model→provider resolution (reads topology from `live.Holder`, one snapshot per `ResolveChain`), priority fallback, per-provider circuit breaker keyed by identity (`breaker.go`, pruned on reload).
- `governance/` — `Governor` (PreCheck/Settle); `fromconfig.go` maps config → policy (USD→µUSD).
- `keystore/` — virtual-key `Store` (SQLite), `Principal`, RBAC `Allows()`.
- `audit/` — single-writer hash-chain writer, WAL, `verify.go`, metrics hooks.
- `pricing/` — integer microUSD table, round-half-even (`math/big`), bundled defaults.
- `limiter/`, `budget/` — in-memory two-phase governance stores with injectable clocks.
- `metrics/` — Prometheus registry + GenAI collectors + nil-safe hooks.
- `openai/` — OpenAI ⇄ canonical conversion.
- `adminauth/` — admin-plane identity leaf (ADR-004): shared `IsOIDCBearerShape` predicate (config guard == middleware routing), groups→team `Resolve`, go-oidc ID-token `Verifier` (lazy discovery, negative cache, alg pin, aud/azp, ±60s skew).
- `live/` — reloadable topology generation (providers + routes + pricing) as one immutable `State` behind an atomic `Holder` (ADR-006); `BuildState` is the topology-only builder (imports only config/providers/pricing — import-guard tested). Hot-reload swaps the generation; governance counters/keystore/audit/breaker persist.
- `providerstore/` — opt-in DB-authoritative provider/model topology store (ADR-008, Stage 2 of ADR-005). SQLite, Postgres-portable TEXT-only DDL: `providers` (refs only — **no secret column**), `model_targets` (ordered routes), `meta` (durable `seeded` marker — seed-once is marker-gated, never row-count, so deleting all providers never resurrects). `Overlay`/`OverlayFrom` build the effective config (file + DB topology, refs unresolved); `SeedIfEmpty` one-time file→DB import (validates ref shape first). UI writes go build-once-swap-once through the assembly's `reloadMu` (see `cmd/inferplane` gateway `writeMutation`); secrets never enter the store (refs only).
- `config/` — config loading + secret-ref resolution (inline secrets rejected); OIDC block validation (https issuer, mandatory client_id, JWT-shaped static tokens rejected).
- `principal/` — request-scoped principal context (leaf, breaks import cycles).
- `filter/` — request-transform filter seam (ADR-009): `RequestFilter` interface + name registry + `Masking` (resolved per-team decision). Core imports this; concrete filters live under `plugins/<name>/` and register via blank import (like providers). Leaf (imports only `sort`).

## Rules
- Pre-check BEFORE billing, settle AFTER. `on_exceeded` block wins on tie.
- Cost is integer microUSD — never float. Use `math/big` round-half-even.
- `count_tokens` must never return non-200.
- Keep `principal`, `metrics`, `governance` import-cycle-free (they are depended on widely).
- Metric labels are config-bounded; never label with raw client input (use the `_rejected` sentinel on pre-resolution rejects).
