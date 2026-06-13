# ADR-005: Provider visibility in the console; UI-write deferred

**Date:** 2026-06-13
**Status:** Accepted (stage 1 shipped; stage 2 deferred)
**Related:** spec §7 (secret refs), 부록 A (secret-ref mandate, policy-as-code),
ADR-001/002 (console), ADR-003 (policy-as-code as a differentiator vs LiteLLM)

## Context

Other LLM gateways (LiteLLM et al.) let operators register providers and paste
upstream API keys in a UI, persisting them in a DB. Enterprise evaluators expect
that manageability. Our console showed only virtual keys — operators could not
see which providers were wired, their endpoints, or how the gateway
authenticates to them, which read as "the gateway can't talk to my models."

Two needs were conflated in that feedback and must be separated:
1. **Visibility** — "what providers/endpoints/auth are configured right now?"
2. **Registration** — "let me add a provider from the UI."

## Decision

**Stage 1 (this ADR, shipped): read-only visibility.** A token-gated
`GET /admin/config` returns a secret-free topology view (provider name, type,
`base_url`, auth *mode* — the env var name / file path / IAM mode, **never a
secret value**), plus the model routing/fallback order. A console "Providers"
tab renders it and offers a copyable config-block guide for adding providers.
Registration stays in config (policy-as-code, ADR-003).

The secret-free guarantee is structural, not careful coding: the view type
(`configapi.ProviderView`) has **no field that can hold a secret** — `ViewFrom`
derives the auth string solely from `APIKeyRef` (the ref name) and `Auth.Mode`,
never from the resolved `APIKey`. A test populates `APIKey` and asserts it never
appears in the serialized output.

**Stage 2 (deferred): UI-write registration.** Registering providers/models from
the UI requires a writable, runtime-mutable config — which the gateway does not
have (config is boot-static, no hot-reload). That is a real architectural change
with an unresolved source-of-truth question (DB-authoritative with Git export,
vs Git-authoritative with the UI opening a PR). **Even in stage 2, secret values
never enter the gateway** — the UI would register *which ref* a provider uses and
guide the operator to set it in their platform secret store. Stage 2 needs its
own ADR (hot-reload + source-of-truth) before implementation.

## Alternatives considered

1. **UI-write now, store keys in the gateway DB (LiteLLM model).** Rejected —
   violates the §7 secret-ref mandate (inline secrets rejected at load) and
   makes the gateway a secret store / breach target. The secret-ref posture is
   a stated compliance differentiator (ADR-003), not a limitation to "fix."
2. **Expose the raw config (including resolved keys) on the endpoint.**
   Rejected — a secret-leak CRITICAL; the dedicated secret-free view exists
   precisely so no code path can serialize a key.
3. **Documentation only, no endpoint.** Rejected — the operator's question is
   asked at the console, in the moment; external docs don't answer "what is
   wired right now."

## Consequences

- Enterprise gets the visibility they asked for (endpoints + auth mode +
  routing) without the gateway ever holding upstream secrets.
- The "add a provider" path remains GitOps — auditable, reviewable, no drift —
  with the console teaching the exact config block.
- `GET /admin/config` is behind `AdminAuth` (any authenticated admin identity;
  it carries no secret, so no per-team entitlement). It is read-only: writes
  return 405 until stage 2.
- Stage 2 (UI-write) is explicitly a separate ADR; the source-of-truth decision
  is not made here.
