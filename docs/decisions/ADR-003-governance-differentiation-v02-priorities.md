# ADR-003: Governance differentiation vs LiteLLM — v0.2 priority ordering

**Date:** 2026-06-12
**Status:** Accepted
**Related:** spec §1.2 (경쟁 분석), §9 (로드맵), ADR-001/002 (admin console)

## Context

LiteLLM is the de-facto standard LLM proxy and its perceived strength is
governance/manageability. Inspection of its positioning (litellm.ai, 2026-06)
confirms the spec's §1.2 read: adoption UX and provider breadth are genuinely
strong, but the governance core is enterprise-gated — **SSO, JWT auth, and
audit logs are paid**; audit logs are mutable DB rows; cost is float-accumulated;
UI-driven policy lives in Postgres and drifts from Git.

Enterprise adoption gates (the user's framing, which we adopt): usability,
enterprise governance, cost optimization, and PII validation. Performance alone
does not win deals.

## Decision

Double down on the spec's wedge — **"거버넌스 전부 무료 + tamper-evident
audit"** — expressed across the four adoption gates, and reorder the v0.2
roadmap by enterprise-adoption impact:

| Priority | Item | Rationale |
|---|---|---|
| 1 | **OIDC SSO free** (Dex/Keycloak/Okta, groups→team mapping) | Dead-center of competitors' paywall; the #1 procurement-failure reason removed |
| 2 | **Console governance views + audit-verify button** | `quota_utilization` / `budget_spend` metrics already exist; one-click tamper-evidence demo no competitor can copy |
| 3 | **Chargeback report** (`inferplane report` subcommand: monthly per-team CSV from audit µUSD) | Reuses existing audit data; finance-team lock-in |
| 4 | **PII masking plugin** | As specced — explicit opt-in with cache-destruction + cost warning (honest trade-off vs competitors' silent rewriting) |
| 5 | **S3 Object Lock audit anchoring** | Upgrades tamper-*evident* toward tamper-*resistant* (§5.4 wording discipline) |

Standing positioning (marketing line):
> "LiteLLM은 거버넌스를 팔고, inferplane은 거버넌스가 무료 코어다 — 게다가
> 증명 가능하다 (tamper-evident)."

Assets we already hold and must keep loud:

- **Integer µUSD, round-half-even** — month-end reconciliation that adds up
  (vs float drift): cost *accuracy* as a governance trust feature.
- **Verbatim passthrough (§4.4) = cost optimization** — body-rewriting proxies
  break prompt-cache hits and silently raise spend; we preserve them by design.
- **Content-free audit** — records carry metadata/usage/cost, never prompt
  bodies: governance without content retention is a PII-compliance default
  advantage, not an add-on.
- **Policy-as-code** — file config is the policy ledger (GitOps review/rollback);
  the console stays a read-only view of policy + a write path for keys only.

## Alternatives considered

1. **Compete on provider breadth (100+).** Rejected — LiteLLM's moat; our
   provider isolation (§8) keeps the door open without chasing parity.
2. **Gate advanced governance as a paid tier.** Rejected — recreates the exact
   gap we exist to fill; conflicts with CNCF-sandbox vendor-neutral posture.
3. **UI-writable policy (parity with LiteLLM admin UI).** Rejected — converts
   our GitOps/no-drift advantage into their DB-state weakness.

## Consequences

- v0.2 sequencing changes: OIDC SSO moves to the front; observability/console
  items ride on already-shipped metrics.
- `inferplane report` adds a CLI surface bound to the audit schema — schema
  changes must update the reporter (docs/reference/data.md sync rule applies).
- PII masking stays opt-in; we accept losing checkbox-comparisons against
  silent-default-masking products and answer with the cache/cost honesty story.
