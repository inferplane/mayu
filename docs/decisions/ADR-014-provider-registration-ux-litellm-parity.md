# ADR-014: Provider registration UX — LiteLLM-parity guided "Add Model", secret-ref-honest

**Date:** 2026-06-15
**Status:** Accepted — implemented (T1–T11) on `feat/provider-registration-ux`.
Plan in `docs/superpowers/plans/2026-06-15-provider-registration-ux.md`. Hardened
through the `/co-agent:consensus` plan gate (antigravity/Gemini 3.1 Pro High;
codex unavailable on this host — 404 "Engine not found", as in ADR-009) over
3 rounds: R1 → 2 CRITICAL (test-before-save endpoint shape; probe SSRF/
secret-exfil) + 1 MAJOR (bedrock IAM) + 2 MINOR; R2 confirmed C1/C2, added
cache-poisoning + bedrock-AccessDenied + DNS-rebinding fixes; R3 closed the
bedrock classification by an inverse rule. Each task shipped behind TDD + scope
guard; a P4 cumulative gate runs on the full diff.
**Related:** ADR-008 (UI-write provider registration — the DB-authoritative
store + write API this builds on), ADR-002 (console grows but stays
toolchain-free), ADR-005 (provider visibility), ADR-003 (governance/usability
differentiation vs LiteLLM), spec §7 (secret-ref mandate — inline keys rejected),
§4.5/§5.4 (hot-reload semantics), §8 (provider isolation / zero-core-diff)

## Context

ADR-008 shipped UI-write provider registration: a `providerstore` (SQLite,
refs-only) plus `PUT/DELETE /admin/providers|models` and a console form (T8).
First operator reaction (2026-06-15): the registration UX feels *weak next to
LiteLLM*. Concretely, the current console (`adminui/static/index.html:155-204`)
is **two flat forms** with every field always visible and **no feedback loop**:

1. **Provider form** shows `base_url`, `region`, `auth mode`, `api_key_ref`
   kind/value all at once — irrelevant fields for the chosen type are still
   shown (region/auth on an `anthropic` provider; base_url on `bedrock`).
2. **No connection test.** You register a provider *blind*: the form cannot tell
   you whether the env/file ref actually resolves, whether the endpoint is
   reachable, or whether the key is valid. The first signal is a failed *real*
   coding-agent request later.
3. **Model routes are free-text.** The operator hand-types both the public model
   name and each upstream model id and the provider name — a typo routes to a
   non-existent provider and only surfaces at request time.
4. **No model catalog / typeahead**, no health status, no guided flow.

LiteLLM's admin UI is the benchmark operators compare against. Its "Add Model"
flow (verified via litellm.ai docs, 2026-06):
- a **provider dropdown** that **morphs the form** — pick Vertex AI and you get
  Vertex Project/Location/Credentials; pick OpenAI and you get API Base/API Key;
- **reusable credentials** — add a credential once, reuse it across models via an
  "Existing Credentials" dropdown;
- a **Test Connection** button (on both the add page and the model-info page)
  that makes a real probe and shows pass/fail + the error detail;
- a **health-status column** in the model list (pass/fail, last-checked, error);
- **public model name vs litellm model name** mapping with wildcard support;
- multiple deployments under one public name for **load balancing** (rpm/tpm/
  weight); add/edit/delete live via `/model/new` with **no restart**.

The product's stated differentiator (ADR-003) is governance *with* usability.
LiteLLM matches usability and paywalls governance; we must at least match the
**registration usability** while keeping our governance + security edge.

## The hard constraint LiteLLM ignores

LiteLLM lets the operator **paste a raw API key** into the UI; the proxy stores
it in its DB. inferplane's **secret-ref mandate (§7)** forbids this: the console
and the store hold a **reference** (`{env: NAME}` / `{file: PATH}`) only — never
a secret value (`ProviderWrite` has no field that can carry one;
`ParseProviderWrite` rejects inline `api_key`). So we **cannot** copy LiteLLM's
"paste key → test" flow verbatim.

The honest adaptation: the gateway **resolves the registered ref server-side**
and probes the upstream **itself**. The client sends no secret in either the
register or the test call. This is *more* secure than LiteLLM's flow and gives
the same UX payoff (pass/fail before you trust the route).

## Decision

Upgrade the provider/model registration UX to LiteLLM parity **within the
existing envelope** — vanilla HTML/CSS/JS + `go:embed` (ADR-002), DB-authoritative
`providerstore` (ADR-008), zero-core-diff provider isolation (§8), secret-ref
mandate (§7). Six capabilities:

### D1 — Provider-aware dynamic form
The provider form fields **morph by `type`** (data-driven from a small JS field
schema). `anthropic` → `base_url` + `api_key_ref`; `openai_compatible` →
`base_url` + `api_key_ref`; `bedrock` → `region` + `auth.mode`/`auth.profile`,
no key field. Irrelevant fields are hidden, not just ignored. No new write-API
fields — the DTO (`ProviderWrite`) is unchanged.

### D2 — Server-side connection probe (the headline feature)
New endpoint **`POST /admin/providers/test`** (full-admin-gated). It accepts a
**`ProviderWrite` body** (refs only — the DTO has no field that can carry a
secret), so an operator can **test a *draft* provider before saving it** (the
"test-before-trust" loop; a stored provider is tested by submitting its current
fields). It:
1. parses + validates the `ProviderWrite` body (rejects inline secrets, same
   guard as the register path),
2. **resolves the ref server-side** (the same `config` resolution the data plane
   uses) — the client sends **no** secret,
3. invokes a new **optional** provider capability `HealthChecker.HealthCheck(ctx)`
   — a minimal, cheap upstream probe (anthropic/openai_compatible → `GET
   /v1/models`; bedrock → a **bounded 1-token `InvokeModel`/`Converse`**, which
   uses the **same IAM action the data plane already needs** — NOT
   `ListFoundationModels`, which would require an extra `bedrock:ListFoundation
   Models` grant most deployments don't have). **The bedrock probe classifies by
   an inverse rule**: AWS validates the SigV4 signature *before* any
   service-level check, so **only signature/credential errors**
   (`UnrecognizedClientException`, `InvalidSignatureException`,
   `ExpiredTokenException`, missing-credentials) map to `OK:false`; **every other
   outcome — a 2xx OR any post-signature service error** (`AccessDenied`,
   `ModelNotReady`, `ValidationException`, `ResourceNotFoundException` from the
   probe's dummy model id) — maps to `OK:true` + a note, because reaching it
   proves the credentials resolved. This closes the whole class rather than
   enumerating exceptions,
4. returns `{ok: bool, latency_ms: int, detail: string}` with a **sanitized**
   detail that never echoes the ref value or the secret, under a **bounded
   timeout**.
`HealthChecker` is optional (like `TokenCounter`): a provider that doesn't
implement it returns a "probe unsupported" result, never an error. Adding it
touches only `providers/<name>/` + the interface decl — zero core diff (§8).

**Probe trust boundary + SSRF guard (gate-hardened).** The probe resolves a
secret ref and sends it to the request's `base_url`. For a **full admin** this
grants **no new capability** — the same admin can already register a route and
exfiltrate via a single data-plane request. The probe is therefore gated to
**`IsAdmin` only**, NOT the team-mapped lower-privilege provider-write tier
contemplated in ADR-008 (alt. 5): that tier must never be able to resolve a
secret to an arbitrary host. Defense-in-depth, regardless of caller:
- the probe **blocks the cloud metadata endpoint (169.254.169.254 /
  fd00:ec2::254)** — no legitimate LLM upstream lives there — and enforces it in
  the probe HTTP client's **`DialContext`** (checking the *connect-time* IP), not
  as a pre-request string check, so **DNS rebinding (TOCTOU) cannot bypass it**;
- an **optional `probe.allowed_hosts` allowlist** (config) constrains probe
  targets when set (unset = any host, preserving the internal-vLLM use case,
  which is a legitimate private address — so we do **not** blanket-block RFC1918).
This boundary is documented in the runbook and the API reference.

### D3 — Embedded model catalog + typeahead
A small embedded per-type catalog (`GET /admin/providers/catalog?type=<t>` →
known public model ids) backs an HTML `<datalist>` so the operator picks a model
instead of hand-typing it. Static (no upstream call); a follow-up may enrich it
from a live `/v1/models` probe.

### D4 — Route targets reference registered providers
The model-route target's provider field becomes a **dropdown populated from the
registered providers** (the secret-ref-honest analog of LiteLLM's "Existing
Credentials" reuse), and the upstream-model field gets the D3 typeahead. A route
can no longer be saved pointing at a provider that does not exist — the failure
moves from request-time to save-time. (Server-side validation already rejects
this via the candidate-topology build; this makes the UI match.)

### D5 — Health-status column
The providers table gains a **status cell** (●ok / ●fail / ○untested + last-
probe time), populated on demand by D2 (the same on-demand pattern as audit
`VERIFY CHAIN`). The probe endpoint is **stateless** — it does **not** cache
server-side (caching a *draft* test by provider name would poison the saved
provider's status, and there is no read path). Instead the **console caches the
last result in `sessionStorage`**, keyed by provider name, so status survives a
page refresh without backend state or cross-contamination. Persistent
server-side status and periodic background probing are explicit follow-ups.

### D6 — Guided "Add Model" affordance
The two cards are unified behind a single guided flow (select-or-create provider
→ test → pick model → save route), keeping the two underlying APIs. Bilingual
(EN/KO) copy consistent with the existing console.

## What we deliberately do NOT adopt from LiteLLM (and why)

- **Pasting raw keys into the UI / storing them in the DB** — violates §7. We
  test via server-resolved refs instead (strictly more secure).
- **Weighted load balancing (rpm/tpm/weight per deployment)** — that is a
  routing-engine change (shared-state rate accounting → ADR-013 HA territory),
  not a registration-UX change. Out of scope; tracked as a follow-up. Our route
  targets remain an **ordered fallback chain** (router `ResolveChain`).
- **Wildcard model mapping (`openai/*`)** — LiteLLM maps a wildcard public name
  to a provider endpoint. inferplane routes are **exact** `model → ordered
  targets`; wildcard matching is a router-engine change, not a registration-UX
  change. Explicitly out of scope for ADR-014; tracked as a follow-up.
- **Periodic/auto health checks** — v1 is on-demand (D5); background probing is a
  follow-up (it needs a scheduler + cardinality-bounded status storage).

## Consequences

- **Positive:** registration UX reaches LiteLLM parity on the dimensions that
  matter (dynamic form, test-before-trust, catalog, route safety, status) while
  *keeping* the secret-ref security edge LiteLLM lacks; no toolchain added; no
  write-API schema change; provider probe is zero-core-diff.
- **Negative / risks (for the gate to scrutinize):**
  - The probe makes an **outbound upstream call from the admin plane** —
    addressed by D2's trust boundary: full-admin-gated, bounded-timeout,
    error-sanitized (no secret/ref echo), metadata-endpoint blocked, optional
    host allowlist. Residual: a full admin can already exfiltrate via the data
    plane, so the probe adds no escalation **for that role**; the lower-privilege
    provider-write tier (ADR-008 alt. 5) must not be granted probe.
  - Probe cost/latency: `GET /v1/models` is cheap but non-zero; must be explicit
    and on-demand.
  - The embedded catalog goes stale as upstream model lists change — accept
    free-text fallback; never *block* a save on catalog membership.
- **Neutral:** load-balancing parity is explicitly deferred; this ADR is scoped
  to registration UX only.

## Alternatives considered

1. **Adopt LiteLLM's paste-the-key test verbatim.** Rejected — violates §7; the
   server-resolved-ref probe achieves the same UX more securely.
2. **Client-side-only form polish (no probe).** Rejected — the test-before-trust
   loop is the single biggest UX gap; cosmetic-only would not close it.
3. **Full SPA / framework rewrite of the console.** Rejected — violates ADR-002's
   no-toolchain envelope; the dynamic form is achievable in vanilla JS.
