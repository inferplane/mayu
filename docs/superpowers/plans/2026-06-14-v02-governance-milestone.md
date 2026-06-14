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

## Design decisions (for the gate — revised after P2 round-1, both panels)

- **Audit-verify reads a live-appended file.** Verify only the COMPLETE prefix
  (up to the last `\n`); a partial trailing line is ignored. **Be honest about
  it:** the result carries `partial_tail bool` and the UI says "complete prefix
  OK (N records)" — never claim bytes were verified that weren't (codex MAJOR).
- **All file sinks, per-sink results.** The endpoint verifies EVERY configured
  `file` sink and returns an array `[{path, ok, records, broken_at, reason,
  partial_tail}]` (not just the first). No file sink → `{"sinks":[]}` (clear,
  200, not 500). `os.Stat` each path; skip non-regular files with a reason
  (rotation/symlink-swap safety — codex MAJOR).
- **DoS guard:** a file larger than a cap (16 MiB) is not scanned synchronously
  — its per-sink result is `{ok:false, reason:"too large for online verify; use
  `inferplane audit verify`"}` (AdminAuth is not a DoS control — both panels).
- **Chargeback aggregation:** only `request_completed` records with a non-nil
  `cost` are summed. Group by team, or team+**resolved** model (`ModelResolved`
  — the billed model; fall back to `ModelRequested` if empty) — codex MAJOR.
  Output via **`encoding/csv`** (team/model names may contain commas/quotes —
  both panels). Period: `--since` inclusive, `--until` exclusive, both parsed as
  `time.Time` (RFC3339/Nano, offset-aware); invalid `ts` on a record → skip +
  count + stderr warning; malformed JSON line → skip + count. The CLI trims a
  partial trailing line (shared complete-line reader). **USD is formatted
  directly from integer micros** as a sign-aware `$d.dddddd` (micros/1e6 with a
  zero-padded 6-digit fraction) — never float division/accumulation (codex MAJOR).
- **Console governance is client-side** from `/metrics`. Quota → the real gauge
  `inferplane_quota_utilization_ratio{team,window}` (a 0..1 ratio) shown as a
  per-team+window bar. Budget → `inferplane_budget_spend_usd_total{team,...}` is
  a **counter**, shown honestly as "spend (cumulative since start)" — NOT a
  fake utilization gauge (a true budget gauge needs the limit, deferred). No new
  endpoint for gauges; cardinality is config-bounded.

## Security invariants

- `/admin/audit/verify` is behind AdminAuth (any authenticated admin identity;
  read-only). It returns only {ok, records, broken_at, reason} — no record
  contents, no secrets.
- The report CLI reads a local file (operator bootstrap, like `audit verify`);
  it prints team/model/cost — no key material (audit records already carry no
  secrets, only `key_id`/team).
- Every task ends green on: `CGO_ENABLED=0 go build -trimpath -o bin/inferplane ./cmd/inferplane`,
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
      `runReport` aggregates µUSD by team (and by team+**resolved** model with
      `--by team,model`), sorted deterministically; `--since` inclusive /
      `--until` exclusive filter parsed as `time.Time`; records without cost
      excluded; **`encoding/csv`** output with a team name containing a comma +
      a quote (escaping); **USD formatted from integer micros** (`$d.dddddd`,
      incl. a large value with no float drift); edge cases — empty file,
      all-unsettled, a malformed JSON line (skipped + counted), nil cost,
      missing team/model, a negative micros value.
- [ ] Implement `report.go` (`reportCmd(args) int`, `runReport(io.Reader, opts)
      (rows, skipped, error)`) — a complete-line reader (trims a partial
      trailing line), `encoding/csv` writer, integer-micros USD formatter; wire
      `report` into `main.go` dispatch + usage.
- [ ] All four checks green. Commit (DCO sign-off).

### Task 2: Audit-verify admin endpoint (complete-lines-only)

**Files:**

- Create: `internal/server/auditapi/auditapi.go`
- Create: `internal/server/auditapi/auditapi_test.go`
- Modify: `internal/server/server.go`
- Modify: `internal/server/server_test.go`
- Modify: `cmd/inferplane/gateway.go`

**Steps:**

- [ ] Failing tests: `GET /admin/audit/verify` → 200 `{"sinks":[{path,ok,records,
      partial_tail,...}]}`; valid chain → ok:true; tampered → ok:false+broken_at;
      **partial trailing line (no `\n`) → ok:true, partial_tail:true, verifies the
      complete prefix, NOT tampering**; over-cap file → ok:false,
      reason contains "too large"; non-regular path skipped with a reason; no
      file sink → `{"sinks":[]}`; authenticated non-GET → 405 with `Allow: GET`;
      no token → 401 (AdminAuth, wired in server_test, runs before method check).
- [ ] Implement `auditapi.Handler(paths []string)`: for each path `os.Stat`
      (skip non-regular w/ reason), enforce the 16 MiB cap, read + trim to the
      last `\n`, `audit.Verify` the complete prefix, set `partial_tail` if bytes
      were trimmed; return the per-sink array. Wire into AdminMux behind
      AdminAuth; gateway passes ALL `file` sink paths.
- [ ] All four checks green. Commit (DCO sign-off).

### Task 3: Console Governance tab (quota/budget gauges + verify button)

**Files:**

- Modify: `internal/server/adminui/static/index.html`
- Modify: `internal/server/adminui/static/app.js`
- Modify: `internal/server/adminui/static/style.css`
- Modify: `internal/server/adminui/adminui_test.go`

**Steps:**

- [ ] Failing asset tests: assets still contain no `ik_`/`localStorage`/
      `document.cookie`; the verify button uses the existing `api()` helper
      (in-memory admin token, handles 401) to GET `/admin/audit/verify` (grep
      it routes through `api(`, not a bare unauthenticated `fetch`); no inline
      `style=`/`onclick=` attributes in the new markup (CSP guard).
- [ ] Add a **Governance** tab: per-team **quota-utilization** bars from the
      real `inferplane_quota_utilization_ratio{team,window}` gauge, and a
      **budget spend (cumulative)** figure from `inferplane_budget_spend_usd_total`
      — labeled honestly as a counter since process start, NOT a utilization
      gauge — both parsed client-side via the existing `/metrics` parser; plus
      an **Audit integrity** card whose "Verify chain" button renders the
      per-sink results (complete-prefix OK / broken-at / too-large). CSP
      unchanged (`default-src 'self'`); no inline styles/handlers.
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
