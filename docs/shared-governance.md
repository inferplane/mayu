# Shared keys and global rate/token quotas

Use the shared profile when gateways must resolve the same virtual keys and
enforce the same RPM, TPM, token quota and monetary counters. Key lookup and
admission require Postgres. Run a replicated database endpoint and multiple
gateways to tolerate individual process/node failure; database loss or a network
partition refuses new shared requests.

The default SQLite profile and ADR-045 node-local budget profile remain available.
Shared mode's database dependency is explicit; it is not an offline key cache.

## Enable the profile

Start inferplaned with `INFERPLANED_POLICY_DSN`,
`INFERPLANED_DURABLE_BUDGETS=true` and its normal machine/admin credentials.
Use `examples/shared-governance/governance.yaml` as the initial policy seed.
All control planes and shared gateways must address the **same database/schema**.

Start gateways from `examples/config.shared-governance.json`:

```json
{
  "key_store": {
    "type": "postgres",
    "dsn_ref": {"env": "INFERPLANE_PG_DSN"}
  },
  "governance_store": {"type": "postgres"},
  "budget_timezone": "UTC",
  "control_plane": {
    "url": "https://inferplaned.example.invalid",
    "token_ref": {"env": "INFERPLANE_CONTROL_TOKEN"},
    "require_sync": true
  }
}
```

Replace illustrative endpoints, model IDs, prices and context bounds with verified
provider settings. Do not set a SQLite `path`, `control_plane.authority`, local
`policies`, or a per-replica `provider_store`. Each replica defaults to a unique
hostname/boot ID; an explicit `dataplane` must be unique per replica.

Namespace and generation checks reject a different policy database even if its
limits happen to match. `/readyz` includes initial synchronization and database
availability. `/healthz` remains process health. Backend/connection/profile changes
require restart.

## Limits and routing

Shared team records and virtual-key options control RPM/TPM and existing money
limits. Team `tokens_per_day` uses a UTC calendar-day window. Policy rules support
user-only subjects across teams and team+user subjects within one team:

```yaml
apiVersion: inferplane.dev/v1alpha1
kind: GovernancePolicy
metadata: {name: developer-limits, generation: 1}
spec:
  subject: {user: opaque-user-id}
  rules:
    - name: throughput
      failurePolicy: FailClosed
      rate: {rpm: 60, tpm: 2000000}
    - name: monthly-tokens
      failurePolicy: FailClosed
      tokenQuota: {limitTokens: 100000000, period: CalendarMonth}
```

Use `tokenQuota` for shared monthly token limits; the legacy config
`quota.tokens_per_month` is rejected in this profile because it has no authoritative
team-store column. Multiple rules and team/key layers all apply; a warn source
cannot loosen a blocking rule.

Each provider attempt reserves a conservative bound: five declared context-window
token classes and the corresponding integer price bound. A cap smaller than that
bound can refuse even a short request. Complete usage returns the unused portion;
partial streams or missing usage retain uncertainty. Unknown cache TTL pricing
does not prevent settlement of an independently known token quantity.

Soft budget-tier switching is based on settled/uncertain consumption, not an
in-flight reservation. Existing context stability, PII routing/masking and strict
budget target restrictions still apply. All hard money limits remain binding,
including across shared gateways and ADR-045 node-local grants.

## Keys and migration

Replica startup is insert-only. Original seed fingerprints let an unchanged config
restart without overwriting administrator edits. Conflicting declarations refuse
startup. Use the admin API for subsequent updates; all admitted keys require a
current shared team record. Deleting the team stops shared admission for its keys.

Key CLI commands accept either the legacy SQLite `--store` or `--config`:

```bash
mayu keys create --config config.json --team engineering --models auto,weak,normal,strong,economy
mayu keys list --config config.json
mayu keys revoke --config config.json --id KEY_ID
```

These commands resolve only `key_store` credentials. They do not need provider,
control-plane or console credentials. Plaintext is emitted only for a newly
created key.

For migration:

1. Back up the SQLite store and drain/freeze the old deployment's requests and
   key/team mutations.
2. Initialize the destination control-plane policy authority.
3. Run `mayu keys import --config shared-config.json --sqlite old-keys.db`.
4. Start shared gateways against that same database and validate authentication,
   revocation, readiness and global limits before directing traffic.

Import retains hashes and revocation tombstones. It is atomic and retries
idempotently; a conflicting destination refuses the import. Batches have a
five-second deadline and may need operational preparation for a large deployment.
Do not roll back to stale SQLite or restore an old database while newer authority
remains active: that can resurrect identities or budgets.

## Availability and operations

`examples/helm.shared-governance.yaml` renders two replicas. Build/select an image
containing ADR-046 first, provide the referenced existing Secret, and replace
illustrative provider configuration. With persistence enabled the chart renders
a StatefulSet and separate PVCs, a headless Service, required node anti-affinity
and a disruption budget. Supply at least two schedulable nodes; deploy the
database and control planes across appropriate failure domains. A shared existing
PVC is rejected so replicas cannot collide on one audit WAL.

Ship and verify every replica's audit segment. Use the existing analytics Mode B
when the console must query one shared log index; the default local index remains
local. `/v1/usage` identifies `enforcement_mode: shared` and returns subject-scoped
`shared_limits`, where `used` includes committed reservations/uncertainty and
`reserved` identifies the retained portion. Rate usage represents bucket debt.

Generation returns 429 for rate/token exhaustion, 402 for money exhaustion and
503 for an unavailable store or changed snapshot. Counts always stay local in the
shared profile; unavailable identity storage produces a local zero estimate.
Neither a cheap model nor a failed request bypasses a hard reservation.

Historical accounting and permit identities are retained. An open permit after
a crash or ambiguous commit remains reserved; it is not automatically converted
to terminal consumption, refunded, or included in a soft switching tier. Its
full commitment continues to constrain hard budgets and its original quota window. No automated pruning
or frozen-account reconciliation API ships here. Storage retention must preserve
liabilities and replay safety; do not truncate counters to restore service.
Provider-bound violations require investigation, not an automatic refund.
