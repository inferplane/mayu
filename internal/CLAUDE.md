# internal Module

## Role
Private gateway internals — everything that is not a provider or a public package.
Packages are leaf-oriented to avoid import cycles (`principal`, `metrics`, `governance`
are leaves that others depend on).

## Key Packages
- `server/` — HTTP data plane + admin plane; `anthropicapi/`, `openaiapi/`, `adminapi/` ingress handlers; `adminui/` embedded key console (`/admin/ui/`, data-free static assets, ADR-001); `configapi/` read-only secret-free topology view (`GET /admin/config`, ADR-005); `auth.go`, `adminauth.go`, `tls.go`, `metricsapi.go`.
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
- `config/` — config loading + secret-ref resolution (inline secrets rejected); OIDC block validation (https issuer, mandatory client_id, JWT-shaped static tokens rejected).
- `principal/` — request-scoped principal context (leaf, breaks import cycles).

## Rules
- Pre-check BEFORE billing, settle AFTER. `on_exceeded` block wins on tie.
- Cost is integer microUSD — never float. Use `math/big` round-half-even.
- `count_tokens` must never return non-200.
- Keep `principal`, `metrics`, `governance` import-cycle-free (they are depended on widely).
- Metric labels are config-bounded; never label with raw client input (use the `_rejected` sentinel on pre-resolution rejects).
