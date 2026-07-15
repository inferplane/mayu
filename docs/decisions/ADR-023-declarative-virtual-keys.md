# ADR-023: Declarative virtual keys (config-managed, restart-durable)

**Date:** 2026-07-15
**Status:** Accepted (implemented).
**Related:** ADR-016 (teams as first-class keystore records, DB-authoritative
precedence), ADR-006 (config hot-reload scope), §7 (secrets are referenced,
never inline).

## Context

Virtual keys are SHA-256-hashed at rest; the plaintext is shown once at
`Create()` and never stored (§8 D2, `internal/keystore`). This is correct for
an operator issuing a key interactively, but it means a virtual key has no
declarative representation: it cannot be expressed in `config.json`, checked
into a GitOps repo, or handed to Kubernetes as a Secret the way a provider's
`api_key_ref` can. Every virtual key is either created once via the CLI/admin
API and remembered by the operator, or lost if the key store is wiped.

This became a concrete operational problem for a demo deployment on ECS
Fargate with an ephemeral (`/tmp`-backed) SQLite key store: every container
restart wiped the store, and every client's virtual key stopped working until
an operator re-issued one by hand and updated every client. The same failure
mode applies to any single-replica Kubernetes deployment without a
`PersistentVolume` — restart-for-any-reason (OOM, node drain, rolling
upgrade) silently revokes every client.

LiteLLM's closest analog is `general_settings.master_key` — one operator-
supplied secret, declared once, always present. This ADR provides the same
operability without relaxing §7 ("secrets are referenced, never inline"):
config *references* a plaintext via the existing `SecretRef` (env/file)
mechanism, and the gateway persists only its SHA-256 hash — identical secret
posture to a provider's `api_key_ref`, just applied to virtual keys.

## Decision

**`virtual_keys` config block.** `Config.VirtualKeys []VirtualKeyConfig`, each
entry: `team`, `key_ref` (`SecretRef`), `allowed_models`, and the existing
per-key governance fields (`rpm`, `tpm`, `budget_usd_per_month`, `owner`,
`metadata`). `LoadRaw`'s existing inline-secret probe was extended to reject a
literal `virtual_keys[].key` the same way it already rejects an inline
`api_key` — the plaintext must never sit in the config file, ConfigMap, or git
history, even though the typed struct would silently ignore it (`Key string
\`json:"-"\`` is never populated by `Unmarshal`).

**`validateVirtualKeys`** (in `LoadRaw`'s existing validator chain — same
posture as `log_bodies.key_ref`/`budget_alerts`, so it also runs on SIGHUP
reload and admin-UI writes, both of which already call `LoadRaw`): resolves
each `key_ref`, requires the resolved plaintext to be ≥16 characters (no
`ik_` prefix required — the plaintext format is caller's choice, so a
LiteLLM-style `sk-...` value works identically), requires `allowed_models` to
be non-empty and free of adversarial entries (empty string, embedded comma,
control character — the keystore stores this list as a comma-joined column),
rejects two entries resolving to the same plaintext, and rejects a negative
`rpm`/`tpm`/`budget_usd_per_month` (governance only enforces limits `> 0` —
zero *or negative* means "unlimited," so an unvalidated negative would grant
unlimited spend instead of erroring, the opposite of a typo'd operator
intent; this is the same non-negative guard `adminapi/keys.go` already
applies to admin-API-created keys).

**`keystore.KeyEnsurer`.** A new single-method interface beside the existing
`TeamStore` (same "separate interface so fake `Store`s elsewhere keep
compiling" reasoning): `EnsureKey(ctx, plaintext, team, allowedModels, opts)
(Principal, error)`. `(*SQLiteStore).EnsureKey` hashes the caller-supplied
plaintext (reusing the existing unexported `hashKey`, same derivation
`generateKey` already uses for `key_id`) and `INSERT ... ON CONFLICT(key_hash)
DO UPDATE SET ...` — **deliberately excluding `revoked` and `created_at` from
the SET list**. No schema or migration change; `key_hash` was already
`UNIQUE`.

**Boot-only bootstrap, in `newGateway`, immediately after
`keystore.OpenSQLite` succeeds** (before providerstore open/`SeedIfEmpty` and
before `buildEffective`): for each `virtual_keys` entry, verify the team
exists — in `cfg.Teams` **or** in the key store via `store.GetTeam` (ADR-016:
a team may be DB-only, so checking only the config map would reject a
perfectly valid declarative key for a DB-managed team) — then call
`store.EnsureKey`. A failure at any point closes the store and fails boot
(fail-closed, matching every other secret-ref path). The log line prints only
the returned `key_id` and team, never the plaintext; if the row already
exists and is revoked, the log says so explicitly rather than implying the
key is active.

**Revocation is not resurrected by re-declaration, but only while the store
persists.** The upsert never touches `revoked`, so revoking a leaked
declarative key via the admin API keeps it dead across any subsequent boot
that re-declares the same plaintext — verified by
`TestGateway_RevokedDeclaredKeyStaysRevokedAcrossReboot`. **This guarantee is
scoped to the store surviving**: if the store file itself is wiped (the
ephemeral-container scenario this feature exists to fix), the row is gone
entirely, and `EnsureKey`'s `INSERT` path creates a fresh `revoked=0` row —
a previously-revoked-then-wiped key comes back alive. Config re-declaration
is therefore an **availability** mechanism (clients keep working across
restarts even without a `PersistentVolume`), not a **revocation-durability**
mechanism — that guarantee requires the Helm chart's `persistence.enabled`
PVC (or an equivalent durable volume). Operators who need both restart
availability AND permanent revocation must enable persistence; documented
here as an explicit, accepted limitation rather than a silent gap.

**Helm chart: opt-in persistence (`persistence.enabled`, default `false`).**
Defaulting it `true` would be a breaking change on any cluster without a
default `StorageClass` (the PVC stays `Pending`, the pod never schedules, on
an upgrade that previously worked with an ephemeral store) — so it stays
opt-in, with a values.yaml comment explaining the restart-wipe trade-off.
When enabled: a `PersistentVolumeClaim` (or `existingClaim`) mounts at
`/var/lib/inferplane`, the Deployment switches to `strategy: Recreate` (an
RWO volume cannot be attached to two pods at once, which `RollingUpdate`
would attempt), and a template-level `{{ fail }}` guard refuses to render
when `persistence.enabled` is true and `replicaCount != 1`.

## Considered and rejected

- **SIGHUP-applying `virtual_keys` changes.** ADR-006 scopes hot-reload to the
  topology generation (providers/models/pricing) only; the keystore is
  explicitly one of the stateful components reload never rebuilds. Persisting
  a *validation* concern (shape/resolvability) in the shared `LoadRaw` chain
  is unavoidable given existing precedent, but the actual `EnsureKey` call —
  the persistence side-effect — stays boot-only, consistent with every other
  keystore mutation.
- **Rejecting a declarative key whose team isn't in `cfg.Teams`.** Checking
  only the config map (rather than also the store) would make declarative
  keys incompatible with ADR-016's DB-authoritative team path — caught in
  code review before merge.
- **Pruning/reconciling removed `virtual_keys` entries.** Out of scope: this
  ADR's promise is "a declared key survives a restart," not "config is the
  sole source of truth and prunes keys removed from it." An operator who
  wants a declared key gone should revoke it via the admin API (durable once
  a `PersistentVolume` is in place); adding automatic pruning is future work
  if a real need arises.
- **`StatefulSet` instead of `Deployment` + PVC.** A `kind` change breaks
  `helm upgrade` in place and buys nothing at `replicaCount: 1`; the `{{ fail
  }}` guard already prevents the one scenario (`replicaCount > 1` + RWO) a
  StatefulSet's per-replica volume claims would have handled differently.

## Consequences

- A config-declared virtual key now survives any container/pod restart, with
  or without a `PersistentVolume` — with one, both availability and
  revocation persist; without one, only availability does (see the
  revocation caveat above).
- `virtual_keys[].key_ref` follows the exact same secret-reference posture as
  every other secret in this codebase — no new trust boundary, no relaxation
  of §7.
- No keystore schema or migration change.
