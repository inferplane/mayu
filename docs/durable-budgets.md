# Durable global budgets

Use this profile for **global GovernancePolicy money budgets across node-local
mayu instances**. Postgres is authoritative for grants, reporting and budget
windows; inference continues using committed local authority without a database
call for each request.

## Control plane

Configure these through your secret manager/environment:

```text
INFERPLANED_POLICY_DSN        = secret Postgres connection reference
INFERPLANED_TOKEN             = machine heartbeat credential
INFERPLANED_POLICY_WRITE_TOKEN = distinct policy administration credential
INFERPLANED_DURABLE_BUDGETS    = true
```

Then start each control-plane replica against the same database/schema:

```bash
inferplaned --policies examples/durable-budgets/governance.yaml
```

The file is a one-time seed. Subsequent policy reads and writes use Postgres.
Authority transactions read those same policy rows; stale replica caches cannot
issue credit under an older policy. Initialization fails on migration or database
errors; it never silently switches to memory. `/readyz` reflects live authority
database connectivity.

For availability, use a replicated Postgres service and stable load-balanced
control-plane endpoint. No cloud deployment or production database is created by
enabling this code. Database backup/restore must preserve all authority tables
and policy tables together; restoring an old database while nodes hold newer
grants is unsafe.

## Data plane

Start from `examples/config.durable-budgets.json`. Replace illustrative provider
endpoints, model IDs, capability/context declarations and prices with verified
values. Each node needs its own stable ID and private persistent journal:

```json
{
  "budget_timezone": "UTC",
  "control_plane": {
    "url": "https://inferplaned.example.invalid",
    "dataplane": "unique-stable-node-id",
    "token_ref": {"env": "INFERPLANE_CONTROL_TOKEN"},
    "require_sync": true,
    "authority": {
      "journal_path": "/private/node-state/budget-authority.sqlite"
    }
  }
}
```

The journal and SQLite sidecars must be regular private `0600` files. Create its
parent directory first. Do not share or copy a running journal between nodes.
Changing authority mode, node identity or journal path requires a restart.
Remote authority traffic requires HTTPS; loopback HTTP remains available for
local development.

Each model needs a declared positive context window and known pricing. Admission
reserves a conservative bound using all input/cache classes and output. Small
requests can temporarily reserve more than their final cost; complete observed
usage releases the unused local portion for reuse. Unknown cache TTL breakdowns keep the full reservation; they cannot prove a
refund at the cheaper cache-write price. Multi-generation controls such as `n > 1`
are rejected before dispatch. Bedrock generation calls disable internal SDK
retries; gateway fallback reserves each subsequent attempt separately.
The central grant is at least
the pending request's bound when budget is available. A budget too small to
cover a safe bound refuses before invoking the provider.

The first request needing a grant can return 503 with `Retry-After: 1` while the
background worker obtains authority. Exhausted budget returns 402. No fallback
model bypasses this check. The existing soft switching threshold remains distinct
from a hard total cap; use the adaptive-routing policy fields normally.

## Failure behavior

| Event | Result |
|---|---|
| CP process restart/failover | Postgres retains every grant, report and tier |
| CP/DB unavailable | Valid local authority continues until its deadline |
| Lost grant response | Identical retry returns the original grant, never fresh credit |
| Expired grant or vanished node | No automatic central refund |
| Successful completed request | Actual cost consumed; proven unused local reservation released |
| Partial stream/cancellation/missing usage | Bound retained as uncertain; observed usage separate |
| Node/journal restart | Old open grants burned conservatively; no restored credit reuse |
| Budget-window rollover | Database-owned new window; late reports remain in the old one |
| Invalid/forged or conflicting current-boot report | Whole transaction refused |
| Restored old-boot report/meter | Preserve server maxima; no duplicated refund |
| Unavailable journal or invalid authority response | Fail closed |

Do not erase authority tables or edit journal JSON to reclaim uncertain funds.
Retention must preserve known unresolved liabilities and replay identities.
Obsolete unanswered requests may be abandoned conservatively: the server retains
any unknown grant and receives no refund request. Local request history is bounded. Malformed usage, checked-cost overflow and bound
violations indicate incorrect provider metadata/pricing or provider behavior;
they poison local admission and freeze affected central authority.

## Scope

This is exact reservation accounting against configured pricing and verified
provider bounds, not a guarantee about an external invoice. It does not globalize
legacy key-local quotas/rates or supply shared virtual-key storage. It also does
not add an OIDC identity migration: configure stable opaque user subjects through
the existing authenticated issuance path. See [ADR-045](decisions/ADR-045-durable-budget-authority.md).

## Validation

Repository tests use disposable local Postgres and independent schemas:

```bash
go test ./... -race
```

Set `INFERPLANE_TEST_PG_DSN` only to a disposable test service to run PostgreSQL
coverage. CI supplies Postgres17 and runs those tests; a skipped DB suite is not
evidence of shared authority correctness.
