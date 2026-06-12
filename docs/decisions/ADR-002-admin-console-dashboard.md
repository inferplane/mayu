# ADR-002: Admin console grows into an operator dashboard (still toolchain-free)

**Date:** 2026-06-12
**Status:** Accepted
**Supersedes parts of:** ADR-001 (the "3 static files" ceiling; every other
constraint of ADR-001 stands)

## Context

ADR-001 shipped the minimal key console and capped UI growth pending a new ADR.
First real operator feedback (ECS demo deployment, 2026-06-12) was immediate:
the bare two-form page "doesn't feel like an admin page" — after issuing a key
there was no guidance on using the gateway, no visibility into traffic, and no
sense of a control plane. The product's differentiator is *governance
visibility*, and the console showed none of it.

## Decision

Expand the console into a small operator dashboard — **within the same
no-toolchain envelope**: vanilla HTML/CSS/JS, `go:embed`, no framework, no npm,
no build step. The file-count ceiling moves from "3 files" to "a handful of
static assets in `internal/server/adminui/static/`" (currently 4: index, app.js,
style.css, favicon.svg).

What the dashboard adds:

- **Token gate → shell** layout (sidebar nav: Overview / Virtual keys /
  Quickstart) instead of a single scrolling form.
- **Overview**: stat cards (total requests, active keys, teams seen, budget
  spend µUSD) and a traffic-by-model table — parsed **client-side** from the
  same-plane unauthenticated `/metrics` Prometheus text. No new server
  endpoint, no new auth surface.
- **Quickstart**: copyable Claude Code / curl / OpenAI-client snippets that
  fill in with `window.location.origin` and the just-issued key, plus the
  routable model list fetched from `/v1/models` with that key (works when the
  planes share an origin, e.g. behind CloudFront; degrades to a hint on
  port-split setups).
- **Health LED** polling `/healthz`.

Security posture is unchanged from ADR-001: assets are data-free and
unauthenticated; every data call uses the existing token-gated `/admin/keys`
API or endpoints that are already unauthenticated by design (`/metrics`,
`/healthz`); the token and the shown-once plaintext live in page memory only
(tests still ban storage APIs and key material in assets); CSP stays
`default-src 'self'` — which is also why stagger animations use nth-child
rules instead of inline style attributes (CSP blocks those).

## Alternatives considered

1. **Keep the minimal page, add docs elsewhere.** Rejected — the operator's
   first-run moment is the page itself; external docs don't fix the empty feel.
2. **Server-rendered dashboard with new admin endpoints.** Rejected — every new
   authenticated endpoint is new attack surface; the data needed already exists
   on the plane (`/metrics`, `/admin/keys`).
3. **Adopt a small framework (preact/htmx).** Rejected — reintroduces the
   dependency/supply-chain tax ADR-001 exists to avoid; vanilla is sufficient
   at this size.

## Consequences

- The "frontend tax" grows from ~150 to ~600 lines of vanilla code — still no
  lockfile, no CI step, no dependency to audit.
- Client-side Prometheus parsing is best-effort and version-coupled to our own
  metric names; if metric names change, the Overview degrades (cards show "—")
  but nothing breaks.
- The v0.2 OIDC self-service page still builds on this console (unchanged).
