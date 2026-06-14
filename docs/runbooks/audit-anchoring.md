# Runbook: S3 Object Lock audit anchoring (ADR-012)

Periodic anchoring of the audit hash-chain head to a WORM (Write-Once-Read-Many)
S3 bucket upgrades the audit log from **tamper-evident** to **tamper-resistant**:
an attacker who rewrites the local audit file can no longer hide it, because the
immutable anchored head hashes prove what the chain contained at each anchor.

## ⚠ Tamper-resistance is CONDITIONAL

The gateway only PUTs anchor objects. The resistance guarantee holds **only if**
the bucket and IAM are configured correctly — otherwise the gateway is just
writing mutable JSON:

1. **Object Lock ENABLED at bucket creation** (it cannot be turned on later) with
   a **default retention in COMPLIANCE mode** (GOVERNANCE mode can be bypassed by
   privileged users — use COMPLIANCE for true WORM). Versioning is required (and
   implied by Object Lock).
2. **IAM that forbids bypass/delete**: the gateway's principal needs only
   `s3:PutObject`; **no** `s3:BypassGovernanceRetention`, `s3:DeleteObject`,
   `s3:DeleteObjectVersion`, or Object Lock config changes. A separate, tightly
   held auditor principal reads.
3. **Bounded window (RPO = anchor `interval`)**: records written since the last
   *successful* anchor are only tamper-evident until the next anchor. Choose the
   interval to match your forensic RPO.

If any of these is missing, the anchors are deletable/overwritable and the
upgrade does not hold.

## Create the bucket (one-time, operator/IaC)

```bash
aws s3api create-bucket --bucket my-inferplane-audit-anchors --region us-west-2 \
  --object-lock-enabled-for-bucket \
  --create-bucket-configuration LocationConstraint=us-west-2
aws s3api put-object-lock-configuration --bucket my-inferplane-audit-anchors \
  --object-lock-configuration '{"ObjectLockEnabled":"Enabled","Rule":{"DefaultRetention":{"Mode":"COMPLIANCE","Days":365}}}'
```

## Enable in config

```json
"audit": {
  "buffer": { "path": "/var/lib/inferplane/audit.wal" },
  "sinks": [ { "type": "file", "path": "/var/lib/inferplane/audit.jsonl" } ],
  "anchor": {
    "type": "s3", "bucket": "my-inferplane-audit-anchors", "prefix": "anchors",
    "region": "us-west-2", "interval": "5m", "retain_days": 365
  }
}
```

- `retain_days` sets per-object COMPLIANCE retention in addition to the bucket
  default (defense in depth). `endpoint` overrides for an S3-compatible WORM store
  (e.g. MinIO with Object Lock). Anchoring is **opt-in** — omit the block to
  disable (no S3, no worker).
- The gateway uses the standard AWS credential chain (IRSA / env / profile), the
  same as the bedrock provider.
- Use an **opaque `instance` id** posture: the anchor object key embeds the
  gateway instance id; avoid encoding tenant/host/PII in deployment naming.

## Anchor object

Each anchor is `s3://<bucket>/<prefix>/<instance>/<ts>-<count>.json`:

```json
{ "instance": "<host>-<ulid>", "head_hash": "sha256:…", "count": 12345,
  "ts": "2026-06-14T00:05:00.123456789Z" }
```

It carries **no secret and no PII** — only the chain head hash, the record count,
the instance id, and the timestamp. A failed anchor is retried on the next tick
(`inferplane_audit_anchor_failures_total` counts failures); the cursor advances
only on success.

## Verify a local chain against the WORM anchors (auditor procedure)

1. Fetch the **latest** anchor for the instance:
   ```bash
   aws s3 ls s3://my-inferplane-audit-anchors/anchors/<instance>/ | sort | tail -1
   aws s3 cp s3://my-inferplane-audit-anchors/anchors/<instance>/<latest>.json -
   ```
2. Re-verify the local chain (detects intra-chain tampering on its own):
   ```bash
   inferplane audit verify --file /var/lib/inferplane/audit.jsonl
   ```
3. **Cross-check**: re-compute the local chain's head hash at record `count`
   (the anchor's `count`) and assert it **equals** the anchor's `head_hash`. A
   mismatch means the local chain was altered after it was anchored — the WORM
   anchor is the source of truth (it cannot have been changed). Records beyond
   the anchored `count` are within the RPO window (anchored at the next tick).

> Automated anchor-aware `inferplane audit verify` (fetch + cross-check in one
> command) is a tracked follow-up; v1 ships the anchoring writer + this manual
> procedure.

## Incident: anchor failures climbing

`inferplane_audit_anchor_failures_total` increasing means anchors are not
reaching S3 (creds, network, bucket policy). The audit chain keeps writing (the
request path is unaffected — anchoring is best-effort), but the un-witnessed
window grows. Check the gateway logs (`audit anchor failed`), the IAM
`s3:PutObject` permission, and bucket reachability; the next successful tick
anchors the current head (no data lost — the head is recomputed each tick).
