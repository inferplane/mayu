# plugins Module

## Role
Request-transform filter plugins — the spec's filter chain ⑥ (v0.2+). This is an
**extension surface** mirroring `providers/`: a new filter is **one package**
under `plugins/<name>/` plus a blank import in `cmd/inferplane/main.go`, with
**zero core diff**. Core packages (`internal/server`, `router`) import only the
`internal/filter` interface, never a concrete plugin.

## Key Packages
- `piimask/` — opt-in PII masking (ADR-009). Regex + Luhn detectors (email,
  phone, credit-card, SSN, IPv4) → typed placeholders (`‹EMAIL›` …). One-way: no
  vault, no un-masking, **no PII stored**. Registers as `pii-mask` in `init()`.

## Rules
- A plugin registers itself with `filter.Register(...)` in `init()`; it is
  activated by the `plugins` config block (per-team or global). Registration
  happens at process start (blank import), before config validation, so an
  unknown plugin name is rejected at load.
- **Masking is opt-in and its cost is explicit.** Mutating the body abandons
  verbatim `RawBody` forwarding → destroys the prompt cache (up to 10× cost,
  §4.4). Enabling a masking filter must log a warning + emit a metric/audit; it
  must never be silent (ADR-009, spec §308).
- **Never store PII / secrets.** Filters are one-way transforms; there is no
  original↔placeholder map at rest (that would make the gateway a PII vault).
- **Never touch structural fields.** A text filter masks only message text
  (user/assistant `text` blocks / string content) — never `system` (§302),
  `cache_control`, `tool_use`/`tool_result`, or `thinking`/`redacted_thinking`.
- **Fail closed for security filters.** A masker error on a request path rejects
  the request (never forwards unmasked); `count_tokens` returns 200 with a local
  estimate (never 500, never leak). A protocol the filter cannot redact (OpenAI
  ingress in v1) is refused for masked teams, not silently passed.
