# Plan: Provider registration UX — LiteLLM parity (ADR-014)

**ADR:** docs/decisions/ADR-014-provider-registration-ux-litellm-parity.md
**Trunk:** main
**Discipline:** TDD (failing test → minimal code → refactor). One commit per task.
Backend tasks ship Go tests; frontend tasks (vanilla JS, no JS test harness) are
covered by their backing Go handler tests + `bash tests/run-all.sh`.

## Security checklist (applies to every task)

- No inline secret/key field anywhere in the UI or the write DTO (§7).
- Probe `Detail` never echoes the ref value or any secret (sanitized).
- Probe is admin-gated, bounded-timeout, and not an open SSRF amplifier
  (operator-supplied `openai_compatible` base_url is the operator's own config —
  same trust boundary as today's data plane; document it).
- `/metrics` unaffected; no new high-cardinality labels.
- DCO sign-off on every commit (`git commit -s`).

## Out of scope (ADR-014 follow-ups)

- Weighted load balancing (rpm/tpm/weight) — routing-engine change.
- Periodic/background health checks — needs scheduler + bounded status store.
- Live `/v1/models`-sourced catalog enrichment.

---

### Task 1: HealthChecker optional provider capability

- Modify: `providers/provider.go`

Add an optional interface alongside `TokenCounter`:
`HealthChecker interface { HealthCheck(ctx context.Context) (HealthResult, error) }`
with `type HealthResult struct { OK bool; LatencyMS int64; Detail string }`.
Doc-comment it as optional (like `TokenCounter`): a provider that does not
implement it ⇒ "probe unsupported". No behavior change to existing providers.

- [ ] Write the failing test asserting the interface shape + that a non-implementer is detectable via type assertion
- [ ] Add `HealthChecker` + `HealthResult` to `providers/provider.go`
- [ ] `go test ./providers/...` green; commit `feat(providers): optional HealthChecker capability (ADR-014 T1)`

### Task 2: anthropic + openai_compatible HealthCheck

- Create: `providers/anthropic/health.go`
- Test: `providers/anthropic/health_test.go`
- Create: `providers/openaicompat/health.go`
- Test: `providers/openaicompat/health_test.go`

Implement `HealthCheck` as a bounded `GET {base_url}/v1/models` (or `/models`)
with the resolved key, mapping 2xx→OK, non-2xx→`{OK:false, Detail: sanitized
status}`. Never put the key/ref in `Detail`. Use the provider's existing HTTP
client.

- [ ] Write failing `httptest`-backed tests: 200→OK+latency; 401→not-OK+sanitized detail; timeout→not-OK; detail contains no secret
- [ ] Implement `HealthCheck` in both providers
- [ ] `go test ./providers/anthropic/... ./providers/openaicompat/...` green; commit `feat(anthropic,openaicompat): /v1/models health probe (ADR-014 T2)`

### Task 3: bedrock HealthCheck

- Create: `providers/bedrock/health.go`
- Test: `providers/bedrock/health_test.go`

Implement `HealthCheck` as a bounded `ListFoundationModels` (or 1-token converse)
via the existing AWS client seam (`providers/bedrock/client.go` interface); map
errors to a sanitized `Detail`.

- [ ] Write failing test mocking the bedrock client: OK path + error path; assert no credential in detail
- [ ] Implement `HealthCheck` in `providers/bedrock`
- [ ] `go test ./providers/bedrock/...` green; commit `feat(bedrock): health probe via ListFoundationModels (ADR-014 T3)`

### Task 4: server-side provider connection probe handler

- Create: `internal/server/configapi/probe.go`
- Test: `internal/server/configapi/probe_test.go`

Handler for `POST /admin/providers/{name}/test`: (1) 405 when no provider store;
(2) 404 when the named provider is unknown; (3) load the stored row, resolve its
ref server-side (same `config` resolution the data plane uses — client sends no
secret), build the live provider, call `HealthCheck` under a bounded
`context.WithTimeout`; (4) return `{ok, latency_ms, detail}` JSON. A provider
without `HealthChecker` ⇒ `{ok:false, detail:"probe unsupported for this
provider type"}` at HTTP 200 (never 500).

- [ ] Write failing tests: 405 (no store), 404 (unknown), unsupported-capability 200, sanitized detail (fake failing provider → body has neither ref value nor secret), timeout honored
- [ ] Implement `probe.go`
- [ ] `go test ./internal/server/configapi/...` green; commit `feat(configapi): server-side provider connection probe (ADR-014 T4)`

### Task 5: embedded model catalog endpoint

- Create: `internal/server/configapi/catalog.go`
- Test: `internal/server/configapi/catalog_test.go`
- Create: `internal/server/configapi/models_catalog.json`

`GET /admin/providers/catalog?type=<anthropic|openai_compatible|bedrock>` →
`{models: [...]}` from a `go:embed` JSON. Unknown type ⇒ `{models:[]}` (never
500); missing type ⇒ 400.

- [ ] Write failing tests: known type non-empty; unknown type empty; missing type 400
- [ ] Implement `catalog.go` + `models_catalog.json`
- [ ] `go test ./internal/server/configapi/...` green; commit `feat(configapi): embedded model catalog for typeahead (ADR-014 T5)`

### Task 6: wire probe + catalog routes

- Modify: `internal/server/server.go`
- Modify: `cmd/inferplane/gateway.go`

Register `POST /admin/providers/{name}/test` and `GET /admin/providers/catalog`
behind the same `AdminAuth` guard as the other write routes; pass the
providerstore + ref resolver from the assembly.

- [ ] Write failing server-level test: both routes require auth (401 without token); 405/404 wiring matches Task 4
- [ ] Wire routes in `server.go` + pass deps from `gateway.go`
- [ ] `go test ./internal/server/... ./cmd/...` green; commit `feat(server): wire provider probe + catalog routes (ADR-014 T6)`

### Task 7: provider-aware dynamic register form (D1)

- Modify: `internal/server/adminui/static/index.html`
- Modify: `internal/server/adminui/static/app.js`
- Modify: `internal/server/adminui/static/style.css`

A JS field schema keyed by provider `type`; on `type` change show/hide relevant
fields (anthropic/openai_compatible → base_url + api_key_ref; bedrock → region +
auth, no key field). No write-API change; no inline secret field.

- [ ] Add the field schema + show/hide logic in `app.js`; add field data-attrs in `index.html`; style in `style.css`
- [ ] `bash tests/run-all.sh` green (structure/asset/secret-pattern checks); commit `feat(adminui): provider-aware dynamic register form (ADR-014 T7)`

### Task 8: connection test button + provider health status (D2/D5)

- Modify: `internal/server/adminui/static/index.html`
- Modify: `internal/server/adminui/static/app.js`
- Modify: `internal/server/adminui/static/style.css`

Add a TEST CONNECTION button to the provider form and a status cell to the
providers table; both call `POST /admin/providers/{name}/test` and render
●ok(latency) / ●fail(detail) / ○untested. Sanitized detail only.

- [ ] Add button + status-cell rendering in `app.js`/`index.html`; style badges in `style.css`
- [ ] `bash tests/run-all.sh` green; commit `feat(adminui): connection test + provider health status (ADR-014 T8)`

### Task 9: route provider dropdown + model typeahead (D3/D4)

- Modify: `internal/server/adminui/static/index.html`
- Modify: `internal/server/adminui/static/app.js`

Model-route form: provider field → `<select>` populated from registered
providers; upstream-model field → `<input list=...>` backed by
`GET /admin/providers/catalog`. Free-text fallback preserved (never block save on
catalog membership).

- [ ] Populate provider `<select>` from loaded providers; wire catalog `<datalist>` typeahead
- [ ] `bash tests/run-all.sh` green; commit `feat(adminui): route provider dropdown + model typeahead (ADR-014 T9)`

### Task 10: guided add-model flow + bilingual copy (D6)

- Modify: `internal/server/adminui/static/index.html`
- Modify: `internal/server/adminui/static/app.js`
- Modify: `internal/server/adminui/static/style.css`

Light unification of the two cards into a guided order (provider → test → model →
save), with EN/KO copy consistent with the existing console. Cosmetic; no API
change.

- [ ] Reorder/group the cards into the guided flow; add KO copy
- [ ] `bash tests/run-all.sh` green; commit `feat(adminui): guided add-model flow + KO copy (ADR-014 T10)`

### Task 11: docs sync

- Modify: `docs/reference/api.md`
- Modify: `internal/CLAUDE.md`
- Modify: `providers/CLAUDE.md`
- Modify: `docs/decisions/ADR-014-provider-registration-ux-litellm-parity.md`

Document `POST /admin/providers/{name}/test` + `GET /admin/providers/catalog`
(api.md), the configapi probe/catalog (internal/CLAUDE.md), the `HealthChecker`
capability (providers/CLAUDE.md), and flip ADR-014 status → Accepted after the
gate.

- [ ] Update all four docs
- [ ] commit `docs: provider probe/catalog + HealthChecker (ADR-014 T11)`
