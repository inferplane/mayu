# Plan: v0.2 governance milestone — console governance views, audit-verify, chargeback, release

**Date:** 2026-06-14
**Related:** ADR-003 (v0.2 priorities #2, #3), spec §5.3/§5.4/§6
**Base:** main @ 7abc142 · **Produces:** ADR-007 (chargeback report contract)

## Goal

Complete the v0.2 governance wedge entirely in-binary (no new infra), then tag
**v0.2.0**:
- **#2 Console governance views** — per-team quota/budget gauges (from existing
  `/metrics`) + a one-click **audit-verify** button proving the chain is intact.
- **#3 Chargeback report** — `inferplane report` aggregates audit µUSD by team
  (finance lock-in; reuses the existing audit log).
- **Release prep** — version 0.2.0, CHANGELOG, README status, final review.

## Design decisions (for the gate)

- **Audit-verify endpoint reads a file being appended to.** The live file sink
  may have a partially-written trailing line; verifying it raw would spuriously
  fail. The endpoint verifies only COMPLETE lines (up to the last `\n`) — a
  trailing partial line is ignored, never treated as tampering.
- **Which file:** the endpoint verifies the FIRST configured `file` audit sink;
  if there is no file sink, it returns a clear "no file sink" result (not an
  error) — stdout-only deployments can't be verified at rest.
- **Endpoint cost:** verification is O(file) and operator-triggered behind
  AdminAuth; it is read-only and not on the data hot path. Documented; no cap
  in v0.2 (operator action, single file).
- **Chargeback aggregation:** only `request_completed` records with a `cost`
  are summed (started/denied/count_tokens carry no settled cost). Key by team,
  or team+model. Period filter parses `ts` as RFC3339 (`--since`/`--until`).
  Integer µUSD throughout; the report prints µUSD and a derived USD (µUSD/1e6)
  — never float accumulation.
- **Governance gauges are client-side** from `/metrics`
  (`inferplane_quota_utilization_ratio{team,window}`,
  `inferplane_budget_spend_usd_total{team,...}`) — no new endpoint, no secret,
  cardinality already config-bounded.

## Security invariants

- `/admin/audit/verify` is behind AdminAuth (any authenticated admin identity;
  read-only). It returns only {ok, records, broken_at, reason} — no record
  contents, no secrets.
- The report CLI reads a local file (operator bootstrap, like `audit verify`);
  it prints team/model/cost — no key material (audit records already carry no
  secrets, only `key_id`/team).
- Every task ends green on: `CGO_ENABLED=0 go build ./...`,
  `go test ./... -race`, `go vet ./... && gofmt -l .`, `bash tests/run-all.sh`.

## Non-goals

- PII masking (#4) and S3 Object-Lock anchoring (#5) — deferred to v0.2.x/v0.3.
- UI-write provider registration — separate plan on the hot-reload foundation.
- Multi-replica HA backends (Redis/Postgres).

---

### Task 1: ADR-007 + chargeback report CLI (`inferplane report`)

**Files:**

- Create: `docs/decisions/ADR-007-chargeback-report.md`
- Create: `cmd/inferplane/report.go`
- Create: `cmd/inferplane/report_test.go`
- Modify: `cmd/inferplane/main.go`

**Steps:**

- [ ] ADR-007: the report contract (inputs `--file/--since/--until/--by/--format`,
      µUSD aggregation, only settled records, CSV default), and why it reuses
      the audit log rather than a new store.
- [ ] Failing tests `report_test.go`: a fixture JSONL with mixed records
      (started, completed-with-cost across 2 teams/2 models, a denial) →
      `runReport` aggregates µUSD by team (and by team+model with `--by team,model`);
      `--since`/`--until` filter by `ts`; records without cost are excluded;
      CSV output columns + a deterministic order; USD column = µUSD/1e6 exact.
- [ ] Implement `report.go` (`reportCmd(args) int`, `runReport(io.Reader, opts)`)
      parsing audit.Record lines; wire `report` into `main.go` dispatch + usage.
- [ ] All four checks green. Commit (DCO sign-off).

### Task 2: Audit-verify admin endpoint (complete-lines-only)

**Files:**

- Create: `internal/server/auditapi/auditapi.go`
- Create: `internal/server/auditapi/auditapi_test.go`
- Modify: `internal/server/server.go`
- Modify: `internal/server/server_test.go`
- Modify: `cmd/inferplane/gateway.go`

**Steps:**

- [ ] Failing tests: `GET /admin/audit/verify` with a valid chain file → 200
      `{"ok":true,"records":N}`; a tampered file → `{"ok":false,"broken_at":K}`;
      **a file whose last line is a partial write (no trailing newline) verifies
      the complete prefix and does NOT report tampering**; no file sink
      configured → 200 `{"ok":false,"reason":"no file sink"}` (clear, not 500);
      non-GET → 405. Behind AdminAuth (401 without token — wired in server_test).
- [ ] Implement `auditapi.Handler(verifyPath string)` reading the file, trimming
      any trailing partial line at the last `\n`, running `audit.Verify` on the
      complete prefix; JSON result. Wire into AdminMux behind AdminAuth; gateway
      passes the first `file` sink path (empty if none).
- [ ] All four checks green. Commit (DCO sign-off).

### Task 3: Console Governance tab (quota/budget gauges + verify button)

**Files:**

- Modify: `internal/server/adminui/static/index.html`
- Modify: `internal/server/adminui/static/app.js`
- Modify: `internal/server/adminui/static/style.css`
- Modify: `internal/server/adminui/adminui_test.go`

**Steps:**

- [ ] Failing asset tests: assets still contain no `ik_`/`localStorage`/
      `document.cookie` (the new tab adds no storage/secret); the verify button
      calls `/admin/audit/verify` (grep the fetch path in app.js).
- [ ] Add a **Governance** tab: per-team quota-utilization bars and budget-spend
      figures parsed from `/metrics` (client-side, reusing the existing parser),
      and an **Audit integrity** card with a "Verify chain" button that GETs
      `/admin/audit/verify` and renders ok/records or broken-at. CSP unchanged
      (`default-src 'self'`); no inline styles/handlers.
- [ ] Browser smoke (Playwright) against a local gateway: Governance tab renders
      gauges; verify button shows "chain OK (N records)".
- [ ] All four checks green. Commit (DCO sign-off).

### Task 4: Release prep — v0.2.0

**Files:**

- Modify: `CHANGELOG.md`
- Modify: `README.md`
- Modify: `charts/inferplane/Chart.yaml`
- Modify: `docs/reference/api.md`
- Modify: `internal/CLAUDE.md`

**Steps:**

- [ ] CHANGELOG: a `0.2.0` entry summarizing OIDC SSO (ADR-004), config
      hot-reload (ADR-006), provider visibility (ADR-005), console dashboard
      (ADR-002), governance views + audit-verify, chargeback report.
- [ ] README: version badge → `0.2.0`, status line (still pre-announce per
      §1.3 legal hold — note APIs stabilizing), Docker tag refs → `0.2.0`,
      a `inferplane report` quickstart line.
- [ ] `charts/inferplane/Chart.yaml`: `version`/`appVersion` → `0.2.0`.
- [ ] `docs/reference/api.md` + `internal/CLAUDE.md`: `/admin/audit/verify`
      endpoint + `auditapi` package; `report` subcommand.
- [ ] All four checks green. Commit (DCO sign-off).
