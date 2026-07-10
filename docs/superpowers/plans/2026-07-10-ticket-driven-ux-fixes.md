# Ticket-driven UX fixes: discoverable 404, model aliases, self-service usage

**Date:** 2026-07-10
**Source:** `docs/customer-issue-analysis.md` (59 real Claude Code on Bedrock support
tickets in a LiteLLM gateway environment). Three gaps confirmed by code exploration —
each maps to a concrete ticket pain point:

- **Discoverable 404** — tickets row 25/41/52: `Invalid model name passed in`. Today the
  gateway returns `unknown model: X` but does not tell the caller what IS available.
- **Model aliases** — same tickets: `apac.anthropic.*` vs `global.anthropic.*` name
  confusion. Today one route = one exact name; no alias/normalization.
- **Self-service usage** — tickets row 2/37/42: usage-blind → "used up my quota and I
  didn't even do anything" anxiety. Today spend/quota is internal-only (Prometheus +
  admin analytics), never exposed to the key holder.

**Invariants (CLAUDE.md, non-negotiable):** cost is integer microUSD (never float);
`/metrics` and any new response must not leak `key_id`; cache invariant (verbatim
`RawBody` when protocols match) must hold — the ONLY body change allowed is a cache-safe
top-level `model` rewrite on the alias path (F1 below), never the content/`cache_control`
blocks; `count_tokens` must never return non-200.

**Plan-gate resolutions (hybrid gate round 1 — codex/gpt-5.5 + kiro/glm-5 CONFIRMED all;
digest: `.claude/co-agent-consensus/plan-gate/ticket-ux-round1-digest.md`).** These are
binding on the tasks below:

- **F1 (CRITICAL) — alias must reach the forwarded body on the anthropic verbatim path.**
  `providers/anthropic/anthropic.go:46` forwards `RawBody` byte-for-byte and never rewrites
  the top-level `model`. An alias would reach the Anthropic API verbatim and be rejected.
  Bedrock (`providers/bedrock/invoke.go` drops top-level `model`, uses resolved `Upstream`)
  and openaicompat (`providers/openaicompat/openaicompat.go:52` `rewriteModel`) are already
  safe. **Fix in Task 2:** when (and only when) an alias was used, rewrite the single
  top-level `"model"` JSON field of `RawBody` alias→canonical. Cache-safe (cache_control is
  in content/system blocks). When `model == canonical`, do NOT touch `RawBody` — the common
  path stays byte-identical verbatim.
- **F2 (MAJOR) — zero/negative limit = unlimited.** `budget.go:41` returns `Allow` when
  `limitMicros <= 0`; `limiter.go:81` likewise. `UsageOf` must emit `null` (omit) for an
  unlimited dimension, never `remaining: 0`.
- **F3 (MAJOR) — report the caller's per-KEY limits, not just the team's.** Governance
  debits `budget:key:<id>` and enforces per-key rpm/tpm (`governance.go`). A team-only
  `/v1/usage` hides the limit that actually blocks the developer (the ticket anxiety).
  Report the principal's per-key budget/quota AND team budget/quota; key id is a store
  lookup key only, never echoed.
- **F4 (MAJOR) — nil governor must not panic.** `DataMux` accepts `gov == nil`
  (`server.go`). The `/v1/usage` handler returns a well-formed ungoverned response when the
  governor is nil.
- **F5 (MINOR) — the 403 "model not allowed" branch also gets the available list** (same
  helper as the 404 branch; a disallowed typo hits 403 first, `messages.go:116`).
- **F6 (MINOR) — canonicalize in `count_tokens` too** (`count_tokens.go`, keep always-200);
  add a test that an allow-list holding only the ALIAS still denies (403 — no RBAC bypass).
- **F7 (NIT) — ASCII `...` not the Unicode ellipsis; name the cap `const maxModelsInError = 20`.**

**Reused assets:**
- `Router.AllModels()` (`internal/router/router.go:168`) + the RBAC-filter pattern in
  `internal/server/anthropicapi/models.go:24-32` (filter by `p.Allows`).
- `internal/live.State.Route` (`internal/live/live.go:78`) — exact-match map lookup.
- `Governor.policyOf` (`internal/governance/governance.go:97`), `budget.BudgetStore.Spent`
  (`internal/budget/budget.go:24`), `limiter.LimiterStore.QuotaUsed`.
- `principal.From(ctx)` + KeyAuth middleware for the new data-plane endpoint.

---

### Task 1: Discoverable unknown-model 404

**Files:**
- Modify: `internal/server/anthropicapi/messages.go`
- Modify: `internal/server/openaiapi/chat.go`
- Test: `internal/server/anthropicapi/messages_test.go`
- Test: `internal/server/openaiapi/chat_test.go`

Steps:
- [ ] Write a test per ingress: POST with a model that is not routable but whose name
      passes the allow-list (a principal whose allow-list matches the unknown name) →
      assert status stays 404 and error type stays `not_found_error` (Anthropic) / the
      OpenAI equivalent, AND the message now contains the available model names the key
      may use.
- [ ] Write a test asserting the list is filtered by `p.Allows` (a key not allowed model
      `b` must not see `b` listed), sorted by name, and capped at `maxModelsInError` (20)
      with an ASCII `...` marker appended when truncated (assert the marker appears past
      the cap). Assert the 404 body contains no `key_id`/`ik_` material (F7/security lock).
- [ ] Write a test for the empty case: a key whose allow-list excludes every configured
      model → message is `. No models available for this key.` (no dangling list).
- [ ] **F5:** Write a test that the 403 "model not allowed" branch ALSO appends the
      allow-filtered available list (a disallowed typo hits 403 before 404).
- [ ] Add `const maxModelsInError = 20`. Add a small helper (e.g. `availableModels(r, p)`)
      returning the sorted, allow-filtered, capped list string; use it in BOTH the 404 and
      403 branches of `messages.go` and `chat.go`. Append `. Available models: a, b, c`
      / `. No models available for this key.`. Do NOT change status, error type, audit
      call, or metrics.
- [ ] `gofmt`, `go vet`, `go test ./internal/server/...` green.

---

### Task 2: Config-file model aliases

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/live/live.go`
- Modify: `internal/router/router.go`
- Modify: `internal/server/anthropicapi/messages.go`
- Modify: `internal/server/anthropicapi/count_tokens.go`
- Modify: `internal/server/openaiapi/chat.go`
- Modify: `providers/anthropic/anthropic.go`
- Modify: `examples/config.json`
- Test: `internal/config/config_test.go`
- Test: `internal/live/live_test.go`
- Test: `internal/server/anthropicapi/messages_test.go`
- Test: `providers/anthropic/anthropic_test.go`

Steps:
- [ ] Write a config test: a `ModelConfig` with `"aliases": ["apac.anthropic.claude-sonnet-4-6"]`
      loads, and load REJECTS a config where an alias collides with an existing model name
      OR with another model's alias (duplicate-name error, same style as existing config
      validation errors). Aliases are one-hop: an alias may not name another alias
      (enforced by the collides-with-canonical check).
- [ ] Write a `live` test: `BuildState` builds an alias→canonical map;
      `State.Canonical("apac.anthropic.claude-sonnet-4-6")` returns the canonical model
      name, and `Canonical` of an unknown/canonical name returns it unchanged (identity).
      `State.Route` still only accepts canonical names.
- [ ] Write an ingress test: POST `/v1/messages` with an alias model name → routes to the
      canonical target, and the audit record + the resolved model are the CANONICAL name
      (alias must not double-count as a separate model in RBAC/metrics/pricing/audit).
- [ ] **F6 RBAC lock:** Write a test that a key whose allow-list holds ONLY the alias (not
      the canonical name) is DENIED 403 — normalization happens before `p.Allows`, so
      allow-lists must use canonical names; no bypass.
- [ ] **F1 cache-safe body rewrite:** Write a `providers/anthropic` test: a request whose
      body top-level `model` is an alias but `req.Upstream`/resolved model is canonical →
      the forwarded body's top-level `model` is the canonical name AND a `cache_control`
      marker inside a content block is preserved byte-for-byte. Also assert that when body
      model already == canonical, `RawBody` is forwarded byte-identical (no rewrite).
- [ ] Add `Aliases []string \`json:"aliases,omitempty"\`` to `config.ModelConfig`
      (`config.go`). Config-file path only — do NOT touch the providerstore/UI-write DB
      DTO (out of scope; recorded as follow-up in the ADR).
- [ ] In `live.BuildState`, after building `models`, build an `aliases map[string]string`
      (alias→canonical); validate no alias equals any canonical name and no alias is
      declared twice; store on `State`. Add `func (s *State) Canonical(name string) string`
      returning the canonical name (alias hit) or the input unchanged. Ensure the new map
      is carried through any `State` copy/immutability paths.
- [ ] Add a thin `Router.Canonical(model) string` (loads one `live.State` snapshot,
      delegates to `State.Canonical`) so the handler normalizes without holding `State`
      directly. The brief window between `Router.Canonical` and `ResolveChain` is benign:
      canonical names are stable across an alias-only reload (documented in the ADR).
- [ ] In both ingress handlers, normalize the parsed model via `Router.Canonical(...)`
      BEFORE the `p.Allows` check and before `ResolveChain`, so allow-list, audit,
      metrics, and pricing all key off the canonical name. Do the same in
      `count_tokens.go` (F6) — keep its always-200 guarantee.
- [ ] **F1:** In `providers/anthropic/anthropic.go`, when the request's body top-level
      `model` differs from the resolved `req.Upstream` (i.e. an alias was used), rewrite
      ONLY that top-level scalar field in the body before forwarding (a minimal
      top-level-only rewrite, mirroring the bedrock/openaicompat pattern — never re-encode
      content/`cache_control`). When they match, forward `RawBody` unchanged.
- [ ] `/v1/models` continues to list canonical names only — add/confirm a test locking
      that aliases are not advertised.
- [ ] Add an `aliases` example to one model in `examples/config.json`.
- [ ] `gofmt`, `go vet`, `go test ./...` green.

---

### Task 3: GET /v1/usage self-service endpoint

**Files:**
- Create: `internal/server/usageapi/usage.go`
- Create: `internal/server/usageapi/usage_test.go`
- Modify: `internal/server/server.go`
- Modify: `internal/governance/governance.go`
- Test: `internal/governance/governance_test.go`

Steps:
- [ ] Write a `governance` test: `UsageOf(p)` for the caller (team + key id) returns, for
      each dimension that is LIMITED: limit + spent + remaining (int64 microUSD for budget,
      tokens for quota) with the window; and for each UNLIMITED dimension (`limit <= 0`)
      returns null/omitted, NOT `remaining: 0` (**F2**). It reports BOTH per-key and team
      budget/quota (**F3**). Assert spent reflects a prior `Settle` (debit visible on next
      read). Assert the returned struct contains no key-id field.
- [ ] Write a `usageapi` handler test: an authenticated principal (KeyAuth) GETting
      `/v1/usage` receives JSON with integer microUSD fields, per-key + team sections, and
      NO `key_id`/`ik_` anywhere; a fully ungoverned caller yields null budget/quota (not
      an error); no principal → 401; **governor == nil → well-formed ungoverned response,
      not a panic (F4)**.
- [ ] Add `Governor.UsageOf(p principal.Principal) UsageStatus` in `governance.go`:
      for the team (`policyOf(p.Team)`) and the key (`p.KeyID` + `KeyPolicy`), combine
      limits with `bud.Spent("budget:key:"+id)` / team budget key and
      `lim.QuotaUsed(...)` over each policy's window. Read-only (no Debit/Check). Exported
      `UsageStatus` with per-key + team sub-structs; each dimension pointer-typed so an
      unlimited dimension serializes as `null` (**F2**). Remaining = `limit - spent`
      clamped ≥ 0, only for limited dimensions. No key-id field on the struct (**F3**).
- [ ] Create `internal/server/usageapi/usage.go`: handler resolving `principal.From`
      (401 if absent), calling `Governor.UsageOf(p)` (or returning an ungoverned payload
      when the injected governor is nil — **F4**), marshaling the response. Never include
      `key_id`; never a float.
- [ ] Mount `GET /v1/usage` on the data plane in `server.go` behind the same KeyAuth used
      by `/v1/messages` (data-plane mux, not admin). Add a wiring test that KeyAuth guards
      the route and it is absent from the admin plane.
- [ ] `gofmt`, `go vet`, `go test ./...` green.

---

## Verification (end-to-end)

- `go test ./... -race`, `go vet ./...`, `gofmt -l .` all clean.
- `go run ./cmd/inferplane serve --config examples/config.json`, then:
  - POST `/v1/messages` with an unregistered model → 404 body lists available models.
  - POST with an alias name → routes correctly; `audit verify` / report shows the
    canonical model, not the alias.
  - `GET /v1/usage` with a virtual key → budget/quota JSON; issue one request, re-GET,
    confirm `spent_usd_micros` increased.
- `bash tests/run-all.sh` passes.
