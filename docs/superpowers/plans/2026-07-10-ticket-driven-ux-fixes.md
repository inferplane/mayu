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
`RawBody` when protocols match) must hold — alias normalization affects routing only,
never the forwarded body; `count_tokens` must never return non-200 (untouched here).

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
      passes the allow-list (or, simpler, a principal whose allow-list matches the unknown
      name) → assert status stays 404 and error type stays `not_found_error`
      (Anthropic) / the OpenAI equivalent, AND the message now contains the available
      model names the key may use.
- [ ] Write a test asserting the available-model list in the message is filtered by
      `p.Allows` (a key not allowed model `b` must not see `b` listed) and is capped
      (e.g. first 20 sorted names, then a `…` marker) so the message can't grow unbounded.
- [ ] In the 404 branch of `messages.go` (currently `writeErr(w, 404, "not_found_error",
      "unknown model: "+parsed.Model)`), build the available list from
      `h.r.AllModels()` filtered by `p.Allows`, sorted, capped; append
      `. Available models: a, b, c` (or `. No models available for this key.` when the
      filtered list is empty). Do NOT change status, error type, audit call, or metrics.
- [ ] Apply the identical change in the 404 branch of `chat.go`, preserving the OpenAI
      error envelope shape.
- [ ] `gofmt`, `go vet`, `go test ./internal/server/...` green.

---

### Task 2: Config-file model aliases

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/live/live.go`
- Modify: `internal/server/anthropicapi/messages.go`
- Modify: `internal/server/openaiapi/chat.go`
- Modify: `examples/config.json`
- Test: `internal/config/config_test.go`
- Test: `internal/live/live_test.go`
- Test: `internal/server/anthropicapi/messages_test.go`

Steps:
- [ ] Write a config test: a `ModelConfig` with `"aliases": ["apac.anthropic.claude-sonnet-4-6"]`
      loads, and load REJECTS a config where an alias collides with an existing model name
      or with another model's alias (duplicate-name error, same style as existing config
      validation errors).
- [ ] Write a `live` test: `BuildState` builds an alias→canonical map;
      `State.Canonical("apac.anthropic.claude-sonnet-4-6")` returns the canonical model
      name, and `Canonical` of an unknown/canonical name returns it unchanged (identity).
      `State.Route` still only accepts canonical names.
- [ ] Write an ingress test: POST `/v1/messages` with an alias model name → routes to the
      canonical target, and the audit record + the resolved model are the CANONICAL name
      (alias must not double-count as a separate model in RBAC/metrics/pricing/audit).
- [ ] Add `Aliases []string \`json:"aliases,omitempty"\`` to `config.ModelConfig`
      (`config.go`). Config-file path only — do NOT touch the providerstore/UI-write DB
      DTO (out of scope; note it as follow-up in the ADR).
- [ ] In `live.BuildState`, after building `models`, build an `aliases map[string]string`
      (alias→canonical); validate no alias equals any canonical name and no alias is
      declared twice; store on `State`. Add `func (s *State) Canonical(name string) string`
      returning the canonical name (alias hit) or the input unchanged.
- [ ] In both ingress handlers, normalize `parsed.Model`/`canonical.Model` via
      `State.Canonical(...)` BEFORE the `p.Allows` check and before `ResolveChain`, so
      allow-list, audit, metrics, and pricing all key off the canonical name. The
      forwarded request body (`RawBody`) is NOT modified by normalization — the existing
      per-target model rewrite path is unchanged (cache invariant preserved).
- [ ] `/v1/models` (`anthropicapi/models.go`, `openaiapi/models.go`) continues to list
      canonical names only — do not advertise aliases. Confirm no change needed there
      (aliases live in config, not in `AllModels()`), or add a test locking that behavior.
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
- [ ] Write a `governance` test: `UsageOf(team)` for a team with a config/record policy
      returns budget limit + spent + remaining (all int64 microUSD) and quota limit +
      used (tokens) with the window; for an ungoverned team returns `ok=false`. Assert
      spent reflects a prior `Settle` (debit visible on the next read).
- [ ] Write a `usageapi` handler test: an authenticated principal (KeyAuth) GETting
      `/v1/usage` receives `{team, budget:{...}, quota:{...}}` JSON with integer microUSD
      fields and NO `key_id` anywhere; an ungoverned team yields null budget/quota (not an
      error); a request with no principal → 401.
- [ ] Add `Governor.UsageOf(team string) (UsageStatus, bool)` in `governance.go`:
      combine `policyOf(team)` (limits + on_exceeded) with `bud.Spent(...)` and
      `lim.QuotaUsed(...)` over the policy's windows. Read-only — no Debit/Check, no state
      mutation. Define an exported `UsageStatus` struct (int64 micros/tokens, window
      strings). Remaining = max(0, limit-spent).
- [ ] Create `internal/server/usageapi/usage.go`: a handler resolving `principal.From`,
      calling `Governor.UsageOf(p.Team)`, and marshaling the response. Never include
      `key_id`; never a float.
- [ ] Mount `GET /v1/usage` on the data plane in `server.go` behind the same KeyAuth used
      by `/v1/messages` (data-plane mux, not admin).
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
