# LLM gateway core concerns — auto routing, PII guard, prompt integrity, context awareness: where inferplane stands

- Status: design note + gap analysis (proposed, 2026-09-09). Not an ADR — each
  gap below names the ADR it would need before implementation starts; no code
  changes accompany this document.
- Companion reading: the product-agnostic chapter *LLM Gateway (Inference
  Gateway) Deep Dive* in the `kubernetes-docs` training repo
  (`en/ai-ml/08-llm-gateway.md`, same author, same date) explains each
  concern from first principles with diagrams. This document is the
  inferplane-specific half: what is implemented, what is not, and what the
  shape of the missing pieces should be so they do not violate the invariants
  the repo already holds.
- Every "today" claim cites the file or ADR it was checked against; "gap"
  rows were verified by grep against the tree at commit `08d7869`.

---

## 0. Purpose alignment

`CLAUDE.md` → Core Purpose: a change that serves none of the five goals needs
a stated reason. This is how the four concerns map.

| Concern | Goal served | Trade-off that must be stated |
|---|---|---|
| Auto routing (name, policy, cost, availability) | #2 per-user model choice, #3 cost-driven substitution, #5 no SPOF | none new — shipped |
| Auto routing (intent / complexity) | #3 only weakly | a classifier in the request path is a new SPOF candidate (#5) and a new cost; **recommended: do not build** (§1.4) |
| PII guard | #1 — the single entry point is the only place an org can prove "no PII left" | destroys the prompt cache for masked traffic (ADR-009 already states this) |
| Prompt integrity (policy prefix, tool gate) | #1 — a single entry point for *agent* traffic is only trustworthy if it can see and gate tool use; #4 — visibility of what agents actually do | a body mutation, so the same cache warning as ADR-009; a response-side gate adds per-block buffering latency |
| Context awareness (cache affinity) | #3 — a cold cache is a 10× cost event; #4 — visibility of cache hit rate | node-local session state (bounded, lossy) |

---

## 1. Auto routing — the seven layers

The chapter describes seven layers. Five are implemented in `internal/router`
and the ingress handlers; two are not, and one of those should stay that way.

```
                         request: model="claude-sonnet", team=payments, ~38k tokens
                                          │
  L1 name resolution     Canonical()/Allows() alias fold (ADR-021)      ─┐
                         ResolveModel(): unrouted → model_fallbacks /    │ shipped
                         same-family default (ADR-029)                   │ internal/router/router.go
                                          │                              │
  L2 policy              Principal.Allows + SetPolicyGate (ADR-033)      │ internal/keystore, internal/policy
                         FilterRegions (ADR-020 region lock)             │
                                          │                              │
  L3 cost tier           SubstituteTier(): active tier from the          │ internal/tier, ADR-041
                         control-plane ledger; narrows only; latched     │
                         per budget window                               │
                                          │                              │
  L4 intent/complexity   ── not implemented — see §1.4 ──                │ gap (deliberate)
                                          │                              │
  L5 context fit         models.<name>.context_window estimate-only      │ internal/config, anthropicapi
                         fast-fail 400 before PreCheck; count_tokens      │ context_window_test.go
                         exempt (never non-200)                          │
                                          │                              │
  L6 availability        ResolveChain(): priority chain + per-provider   │ internal/router
                         circuit breaker; failover pre-TTFT only;        │
                         ADR-029 appends the fallback model's targets    │
                                          │                              │
  L7 endpoint picking    ── out of scope — delegated ──                  │ see §1.5
                                          │                             ─┘
  RBAC re-check          FilterModelAllowed / FilterRegions AFTER routing  internal/CLAUDE.md Invariants
                         (every ingress handler)
                                          ▼
                         x-inferplane-model-fallback header (messages.go:138,333)
                         audit RequestRef.ModelSubstitutedFrom (ADR-041)
                         inferplane_model_substitution_total{team,from,to}
```

### 1.1 Invariants already held (do not regress)

| Invariant | Where it is enforced |
|---|---|
| Re-check RBAC after every substitution | `FilterModelAllowed`/`FilterRegions` in each ingress after `ResolveChain`; `count_tokens_rbac_fallback_test.go` covers the count_tokens path |
| Substitution narrows, never widens, never denies | `SubstituteTier` fires only when the original passes `Allows` AND the target does (internal/CLAUDE.md `router/`) |
| Failover pre-TTFT only | `internal/router` — "a mid-stream failure is never retried" (docs/architecture.md) |
| Make it visible | header + `ModelSubstitutedFrom` + `ObserveModelSubstitution` |
| Price every route | ADR-030 `HasRate` at boot (`on_missing: block`) and at runtime; `mayu pricing check` CI guard |
| Router must not be the SPOF | there is no classifier in the path; a control-plane outage keeps the last tier state (`Syncer.Tiers`) |

### 1.2 Gap: the conversation is not pinned to one cache domain

`ResolveChain` walks the priority list per request. If `bedrock-apne2` trips
its breaker for one request and `anthropic` serves it, the *next* request of
the same Claude Code session may go back to Bedrock — two cold caches in a
row, because Anthropic-direct and Bedrock prompt caches do not transfer. This
is the routing face of the `cache/` gap in §4.

**Shape of the fix (ADR candidate, folds into the cache-affinity ADR):** a
node-local `(team, prefix-hash) → provider` pin with TTL, consulted between
`ResolveChain` and dispatch: prefer the pinned target if it is in the chain
and its breaker is closed; otherwise fall through and re-pin. Lossy by
design — losing the pin costs one cold cache, never correctness.

### 1.3 Gap: no `x-inferplane-cache-degraded` signal

ADR-009 surfaces masking in a header; a fallback that crosses providers is
surfaced as `x-inferplane-model-fallback` but a fallback that crosses *cache
domains without changing model* (Bedrock ap-northeast-2 → Bedrock us-west-2,
same model id) is invisible to the client. A single header
`x-inferplane-cache-domain: <provider>` on every response would let a client
(or the console) compute the hit-rate impact of routing decisions. Small,
additive, no ADR needed — but it is a wire addition, so it goes in
`docs/reference/api.md` in the same commit.

### 1.4 Deliberately not building: intent / complexity routing (L4)

The chapter's table is blunt: heuristics are cheap and weak, a classifier is
a new hop, LLM-as-router can cost more than it saves. Three inferplane-specific
reasons to leave L4 out:

1. **#5.** A classifier is a dependency in the request path. The fail-through
   rule ("classifier down ⇒ serve the requested model") keeps it from being a
   hard SPOF, but it is still a latency and correctness dependency on every
   request, which the split architecture (ADR-031) exists to avoid.
2. **Agent traffic.** Claude Code resends the whole history each turn.
   Changing the model mid-conversation cold-starts the cache and can make the
   new model reject prior `tool_use` id formats or `thinking` blocks. The safe
   variant — decide on turn one and pin — needs the session pin of §1.2
   first.
3. **#3 is already served.** Cost-driven substitution is what
   `routing.budgetTiers` (ADR-041) does, driven by a *global* utilization
   signal from the ledger rather than a per-request guess about difficulty.
   Per-user model choice (#2) lets a user pick a cheaper model deliberately.

If a future user need appears, the right seam is a `RequestFilter`-like
`Router` plugin under `plugins/` that returns a *proposed* model, followed by
the existing `Allows` + `SubstituteTier` + RBAC re-check — never a bypass of
them.

### 1.5 Out of scope by design: endpoint picking (L7)

For `openai_compatible` targets that are a self-hosted vLLM pool, picking
*which pod* (queue depth, KV-cache utilization, prefix-cache affinity, LoRA)
is the job of the Kubernetes Gateway API Inference Extension's Endpoint
Picker. inferplane should point an `openai_compatible` provider at the
InferencePool's Gateway address and stay a governance layer above it. No
work item.

---

## 2. PII guard — detect → decide → transform → restore

```
              client text ──▶ [ detect ] ──▶ [ decide ] ──▶ [ transform ] ──▶ provider
                                  │             │                │
  today (ADR-009):            regex only     mask, per team   messages[].content
                              email/SSN/IP/  or global;       text ONLY; system,
                              card(Luhn-ish)/ fail closed     tool_use/tool_result,
                              phone (US)                      thinking, cache_control
                                                              untouched (mask.go)
                                  │             │                │
  gap:                        NER · locale   pseudonymize /   deterministic per-session
                              packs (KR RRN, tokenize          placeholders (cache-stable)
                              passport, acct) (reversible)
                                                                  │
              client text ◀── [ output guard ] ◀── [ restore ] ◀── model output
                                  │                     │
  today:                       ── none ──            ── none ── (mask is one-way)
```

### 2.1 What ADR-009 got right and must survive any extension

- **Opt-in per team, cache destruction explicit** — config-load warning plus a
  response header; the chapter calls this the single most important honesty
  property of a PII guard.
- **Text blocks only; structural fields never** — `mask.go` re-emits
  `tool_use`/`tool_result`/`thinking`/`cache_control` verbatim.
- **Fail closed** — a masker error is a 400 with a fixed body; the unmasked
  body is never forwarded (`messages.go:223-235`).
- **Cross-protocol bypass closed** — a masked team on the OpenAI ingress is
  rejected rather than half-masked (ADR-009 round 2).
- **No PII at rest** — the audit record carries the redaction *count* only.

### 2.2 Gaps, in priority order

| Gap | Why it matters | Shape of the fix |
|---|---|---|
| **Placeholders are not yet indexed** | today's typed placeholders (`plugins/piimask`) are the same token for every occurrence, which *is* prefix-stable across turns — but the moment indexed pseudonymization (`<EMAIL_1>`, `<EMAIL_2>`) is added, an unstable numbering scheme makes every turn a cache miss | derive the placeholder index from a session-scoped map (`team + conversation prefix hash`), TTL-bound, memory-only, bounded like `budget.Memory` |
| **No reversible pseudonymization / response restore** | `mask` loses information; a refund-desk bot that cannot say *which* email it emailed is useless. Restore requires a response-side filter that de-tokenizes per completed content block | extend the seam (§2.3) with `ResponseFilter.OnBlock(text) string`; buffer one content block in the SSE relay (never token-by-token — placeholders split across frames) |
| **No output guard** | the model can emit PII memorized from training data, or a secret the tool result contained | same `ResponseFilter` seam; the same regex pack run on output; actions block/redact |
| **US-centric patterns** | `rePhone` is NANP-only; no KR resident registration number, passport, or bank-account pattern | `plugins/piimask/locale_<cc>.go` pattern packs selected by `plugins[].locales` |
| **No NER** | names and addresses are undetectable by regex | a *separate* plugin (`plugins/piiner`) calling a sidecar (Presidio-style); keep the regex plugin dependency-free — the static-binary mandate forbids an in-process model runtime |
| **`system` is never masked** | deliberate (spec §302): the system prompt is application-authored, not user data; leave as is, but document that a client that puts customer data in `system` bypasses the guard | doc note in `docs/reference/security.md` |

### 2.3 Seam extension (ADR candidate: "ADR-043 response-side filters")

`internal/filter.RequestFilter` is request-text-only by ADR-009's choice.
Adding a second, optional interface keeps that decision intact:

```go
// ResponseFilter is implemented by a plugin that also inspects or transforms
// OUTPUT text. Called once per completed text content block (streaming and
// non-streaming alike); never per token, never on tool_use input JSON.
type ResponseFilter interface {
    RequestFilter
    OnBlock(ctx context.Context, text string) (out string, action Action, err error)
}
```

Where it plugs in: the SSE relay (`providers` → ingress writer) already sees
`content_block_start`/`content_block_stop`; the filter runs on the buffered
block at `content_block_stop`. TTFT is unaffected; each block's *end* is
delayed by the block's own length. Error policy is ADR-009's: fail closed —
an `err` replaces the block with a fixed refusal text and sets a header.

The same seam is what the prompt-integrity tool gate (§3.3) needs, so the two
ADRs should share it.

### 2.4 Relationship to Bedrock Guardrails (ADR-019) — already correct

ADR-019 fixed the substantive bypass: a guardrail is an SDK-call parameter,
and every one of the four call paths sets it, with a per-team *override* but
**no opt-out**. The chapter lists this as a question to ask any gateway;
inferplane's answer is yes. Nothing to do.

---

## 3. Prompt integrity — policy prefix and injection defense

This is the widest gap. Today inferplane has **no** mechanism in any of the
five layers the chapter describes; the only content-policy control is the
provider-side Bedrock guardrail (§2.4), which does nothing for
Anthropic-direct or vLLM targets.

```
  one Messages request, by author                    control        today    proposal
  ┌──────────────────────────────────────────┐
  │ [gateway policy prefix]   operator        │  A policy prefix   ── none   §3.2 (ADR-044)
  │ system[]                  application     │  never removed     ✔ (mask never touches system)
  │ tools[]                   application     │  scan descriptions ── none   §3.3
  │ messages[user]            user            │  C scanner         ── none   §3.4 (deferred)
  │ messages[tool_result]     UNTRUSTED       │  B boundary mark   ── none   §3.4 (deferred)
  │ messages[assistant]       model           │  D tool_use gate   ── none   §3.3 (ADR-044)
  └──────────────────────────────────────────┘
  response tool_use / text                         D gate, E canary  ── none   §3.3
```

### 3.1 What exists that the design must respect

- **The cache invariant (§4.4 of the original spec).** Any prefix insertion
  is a body mutation and therefore inherits ADR-009's posture: explicit
  opt-in, warning at config load, header at runtime.
- **`system` is application-owned** (spec §302, `mask.go`). A policy prefix
  may be *prepended*; the client's blocks are never removed or reordered.
- **Streaming relay owns block boundaries** (`internal/openai.ReadChatSSE`
  synthesizes the Anthropic frame lifecycle for OpenAI upstreams) — so a
  per-block gate has one place to hook for all three ingresses.
- **Policy is the single truth** (`internal/policy`, ADR-031/033). A tool
  allow-list is a new *rule kind*, validated by `FromV1Alpha1` like the
  others ("exactly one rule kind per rule", `failurePolicy` required).

### 3.2 Policy prefix injection (ADR candidate: "ADR-044 prompt integrity")

Rule shape (one document, team subject):

```yaml
- name: payments-policy-prompt
  subject: { team: payments }
  promptPolicy:
    prefix:
      ref: { configMapKey: acme-policy-v3 }     # text is NOT inline in the rule
      sha256: "ab12…"                           # pinned; mismatch = policy load error
    canary: true                                 # random 24-char token appended once per prefix version
  failurePolicy: Block                           # Block = 400 if the prefix cannot be applied (string→array promotion fails, ref missing)
```

Placement and rules (each is a test):

1. **Front of `system`**, promoted from string to array if needed. Byte-identical across requests for a given `sha256`.
2. **Idempotent**: if `system[0]` already carries the marker
   `<inferplane-policy sha256=…>`, do nothing (multi-hop).
3. **Never drop or reorder client blocks**; `cache_control` on client blocks
   is preserved verbatim.
4. **Cache posture = ADR-009**: opt-in per team; `x-inferplane-cache-degraded:
   policy-prefix` header; load-time warning. The prefix itself is cache-*stable*
   after the first request, which the header text should say.
5. **Billing**: prefix tokens are the team's. `Settle` already charges actual
   usage; nothing to change, but the console should attribute them (a
   `policy_prefix_tokens` estimate on the audit `RequestRef`, appended at the
   struct end like every ADR-018/041 field).
6. **Audit**: `RequestRef.PolicyPrefixSHA256` (appended). Answers "which rules
   applied that day."
7. **Protocol placement**: Anthropic `system[0]`; OpenAI ingress `messages[0]`
   with role `system` (converted through the canonical schema, so the
   cross-protocol path gets it for free); Bedrock passthrough (ADR-024)
   InvokeModel body is Anthropic-shaped, same as case 1.
8. **Not a security boundary.** The ADR must say so in its Decision section,
   and the enforcement lives in §3.3.

### 3.3 Tool-use gate (same ADR-044)

Rule shape:

```yaml
- name: payments-tools
  subject: { team: payments }
  toolAccess:
    allow: [Read, Grep, Glob, Edit, Bash]        # tool names as they appear in tools[]
    denyArgPatterns:                              # applied to tool_use.input as JSON text
      - 'curl[^|]*\|\s*(ba)?sh'
      - 'rm\s+-rf\s+/'
      - 'prod-db\.internal'
  failurePolicy: Block
```

Enforcement points:

- **Request side**: `tools[]` names not in `allow` ⇒ 403 before PreCheck
  (no counter charged, same posture as RBAC). Tool *descriptions* are scanned
  against `denyArgPatterns` too — an MCP server's description is an injection
  channel.
- **Response side**: on `content_block_stop` of a `tool_use` block, the
  buffered `input` JSON is matched. A hit replaces the block with a `text`
  block `"gateway policy denied tool call <name>"`, sets
  `stop_reason: end_turn`, writes an audit `tool_denied` record (new
  `RequestRef` field, appended), and fires the ADR-017 alert emitter with a
  `tool_denied` event.
- **Streaming cost**: only `tool_use` blocks are buffered (their
  `input_json_delta` frames must be complete to parse anyway); text blocks
  stream through unless a `ResponseFilter` (§2.3) is active.
- **Metrics**: `inferplane_tool_denied_total{team,tool}` — `tool` is a
  policy-declared value, so cardinality is bounded (same posture as
  `ObserveModelSubstitution`).

This is the layer with real enforcement: whatever the model decided, the
execution request never reaches the client.

### 3.4 Deferred: injection scanner and trust-boundary marking

- **Boundary marking** wraps `tool_result` text in delimiters — a body
  mutation inside `messages[]`, which is exactly what the cache invariant
  makes expensive, and Claude Code's own harness already frames tool results.
  Defer; revisit if a non-agent (RAG) ingress becomes a primary use case.
- **Scanner**: heuristics on user and tool_result blocks. False positives on
  coding traffic are high ("ignore" appears in legitimate code constantly).
  If built, it must (a) scan only new blocks per session (delta), which needs
  the session table of §4, and (b) default to `Warn`, never `Block`, for the
  first release. Defer behind ADR-044.
- **Canary** (E) is cheap once the prefix exists (§3.2 item `canary: true`);
  the output check is one string match in the §2.3 `ResponseFilter`. Include
  in ADR-044.

---

## 4. Context awareness — what is known per request today

```
  context kind          signal                         today                       gap
  ─────────────────────────────────────────────────────────────────────────────────────────────
  request               token estimate                 PreCheck estimate           local tokenizer (est. accuracy)
                        context_window                 fast-fail 400, advertised   —
                        count_tokens                   never non-200; RBAC+region  —
                                                       +fallback covered by tests
                        cache_control positions        parsed, preserved           used for prefix placement (§3.2)
  principal             team/user/key/region           Principal                   —
                        active tier, lease remaining   Syncer.Tiers / LeaseTable   —
                        policy age                     GovernanceReady(maxAge)     —
  session               prefix hash → provider pin     ── none ──                  §1.2 / §4.1
                        PII placeholder map            ── none ──                  §2.2
                        scanned-block hashes           ── none ──                  §3.4 (deferred)
  infrastructure        breaker state per provider     router                      —
                        provider latency / error rate  metrics only                not a routing input (correct — L6/L7 only)
                        pod KV-cache / queue depth     ── delegated to EPP ──      §1.5
```

### 4.1 The `cache/` package and the affinity half of `routing`

`internal/cache.VolatileStore` is declared and unimported; the `routing`
rule's `Affinity` half is parsed (`internal/policy/policy.go:151`) but
`checkEnforceable` rejects it on the file channel. The roadmap lists the
"cache-affinity routing engine" as explicitly deferred. This document adds the
concrete minimum that unblocks three concerns at once:

```
  SessionTable (node-local, bounded, TTL)          keyed by  h = sha256(team ‖ tools[] ‖ system[0..k] ‖ messages[0..2])
  ┌──────────────────────────────────────────────┐          (early blocks only — stable across turns)
  │ h → { provider pin, pii placeholder map,     │
  │       scanned-block hashes, lastSeen }        │  consumers:  §1.2 pin · §2.2 placeholders · §3.4 delta scan
  └──────────────────────────────────────────────┘
  eviction: amortized sweep + maxEntries fail-open (a refused NEW key ⇒ no pin, not a deny)
```

Fail-**open** on table pressure is the opposite of `budget.Memory`'s
fail-closed cap, and deliberately so: the table only affects performance
(cache hits), never enforcement. The ADR must say why the two caps differ.

### 4.2 Infrastructure signals must stay out of L2

Provider latency and breaker state feed `ResolveChain` (L6) only. A tempting
"route around the slow region" feature would send a region-locked team's
traffic to a disallowed region; `FilterRegions` after routing is what makes
that impossible today. Any affinity or health-based routing added under §4.1
runs *before* the RBAC re-check, never after — same invariant, same test
shape as `count_tokens_rbac_fallback_test.go`.

---

## 5. Security — chapter checklist against the tree

| Chapter item | inferplane | Reference |
|---|---|---|
| virtual keys hashed at rest, plaintext shown once | ✔ | `internal/keystore` |
| provider secrets by reference only, inline rejected | ✔ | `internal/config` |
| client key never forwarded; upstream key never shown | ✔ | CLAUDE.md security mandates |
| `/metrics` unauthenticated but no secret / key_id, bounded labels | ✔ | `internal/metrics`, `_rejected` sentinel |
| `count_tokens` never non-200 | ✔ | `count_tokens_test.go` |
| tamper-evident audit + external anchoring | ✔ | `internal/audit`, ADR-012 S3 Object Lock |
| body capture encrypted, outside the chain, fail-closed fetch | ✔ | ADR-018 |
| upstream error scrubbing | ✔ | `messages.go:259` (ValidationException scrubbed) |
| request size bound distinct from capture bound | ✔ | `server.max_request_bytes` vs `audit.log_bodies.max_body_bytes` |
| fail-closed option on control-plane loss | ✔ | `control_plane.require_sync` + `max_policy_age` |
| short-lived cloud credentials, no long-lived IAM on data planes | ✔ | ADR-040 broker (≤ 1 h STS) |
| SSO → short-lived virtual keys | ✔ | ADR-028 |
| guardrail enforced on the SDK call, no team opt-out | ✔ | ADR-019 |
| tool allow-list per team | ✘ | §3.3 |
| policy prefix version/hash in audit | ✘ | §3.2 |
| response-side PII / secret guard | ✘ | §2.3 |

---

## 6. Proposed sequencing

| Order | Item | Blocks | Why this order |
|---|---|---|---|
| 1 | **ADR-043 response-side filter seam + session table** (§2.3, §4.1) | ADR-044, PII restore, provider pin | one seam three consumers share; touches the SSE relay once |
| 2 | **ADR-044 prompt integrity**: policy prefix + tool_use gate + canary (§3.2–3.3) | — | the widest gap; the tool gate is the only real enforcement layer for agent traffic |
| 3 | PII: deterministic placeholders + reversible pseudonymize + KR locale pack (§2.2) | — | needs 1; pure `plugins/piimask` work after that |
| 4 | Provider pin + `x-inferplane-cache-domain` header (§1.2–1.3) | — | needs 1; closes the cold-cache visibility gap |
| — | Intent routing (§1.4), boundary marking / scanner (§3.4), NER (§2.2) | — | deferred with reasons above |

Each ADR goes through the repo's multi-model design gate like ADR-009 and
ADR-019 did; the items above are the *inputs* to those gates, not their
outcome. Reference-doc sync per CLAUDE.md: item 1 touches
`docs/reference/api.md` (wire) and `docs/reference/security.md`; item 2 adds
a rule kind and therefore `api/v1alpha1`, `internal/policy`, `deploy/crd`,
`charts/inferplane` and `docs/reference/api.md` in one commit.
