# ADR-027: Admin console i18n — self-contained en/ko/zh/ja dictionary

**Date:** 2026-07-21
**Status:** Accepted
**Related:** ADR-001 (data-free console), ADR-002 (console CSP)

## Context

The admin console (`/admin/ui/`, `internal/server/adminui/`) shipped English-only,
with a handful of hint paragraphs carrying a hand-maintained Korean duplicate in
an inline `<span class="ko">`. The user asked for the console to support English,
Korean, Chinese, and Japanese as defaults.

Two constraints from prior ADRs bound the design:

- **ADR-002 CSP** (`default-src 'self'`) rules out a CDN-hosted i18n library —
  any dependency has to ship inside the embedded `static/` bundle.
- **ADR-001 data-free console** — `adminui_test.go` bans `localStorage` and
  `document.cookie` outright, and permits `sessionStorage` **only** for the
  three named OAuth PKCE keys (ADR-026). A persisted language preference would
  need a fourth storage key, widening that invariant for a low-stakes feature.

`adminui_test.go` also pins several strings verbatim inside `index.html` (e.g.
"per gateway instance", "OUTSIDE the audit chain", "fail-closed", "never the
secret value") and two strings inside `app.js` ("signed in as ", and the
analytics-index degraded-logs message) — any translation mechanism has to
leave those exact English substrings in the served source.

## Decision

**A self-contained `i18n.js` dictionary, applied at runtime; English stays the
canonical text baked into the HTML/JS source; language choice is never
persisted.**

- One new static file, `i18n.js` (served alongside `app.js`/`style.css`,
  `adminui.go`'s `contentTypes` map gained one entry). No new dependency.
- `MSG` is a flat `{ "section.key": { en, ko, zh, ja } }` table (~225 keys).
  Every key carries all four locales — checked by hand at authoring time and
  guarded by `TestAdminUI_i18nWired`'s markers; a missing locale falls back to
  `en` at the `msg()` call site rather than failing.
- Static markup opts in via `data-i18n="key"` (textContent) and
  `data-i18n-placeholder="key"` (input placeholder). `applyLang(lang)` walks
  both attribute sets and rewrites in place, plus sets `<html lang>`.
- Elements `app.js` **also** writes to at runtime (health status text, view
  title, table bodies, status messages, confirm() dialogs, empty-row text)
  are deliberately **never** marked `data-i18n` — they call the same
  `msg("section.key")` helper directly at render time instead. This avoids a
  footgun where `applyLang`'s blanket sweep would stomp a value `app.js` just
  computed (e.g. resetting a live "unreachable" health status back to a
  translated "checking" on every language switch).
- The translation helper is named `msg()`, not `t()` — `app.js` already used
  `t` pervasively as the loop variable for team-record objects
  (`for (const t of rows)`, `teamLimitsSummary(t)`, `fillTeamForm(t)`); naming
  the global function `t` would have shadowed inside every one of those scopes
  and called a team object as a function.
- Hints containing inline `<code>`/`<b>` children (e.g. "...declared in
  `config` (policy-as-code)...") are split into multiple `data-i18n` spans
  around the untouched child element, rather than replacing the whole
  paragraph's `textContent` (which would delete the child element). Each run
  of surrounding text gets its own dictionary key.
- **No persistence.** `LANG` is detected once per page load from
  `navigator.language` (prefix match against `ko`/`zh`/`ja`, else `en`) and
  held only in a JS variable. The `<select id="lang-select">` in the sidebar
  footer switches it for the rest of the session; a reload re-detects. This
  keeps the ADR-001 data-free invariant exactly as narrow as it was pre-ADR-026
  (three PKCE keys, nothing else) at the cost of the language choice not
  surviving a reload — an acceptable trade for a low-stakes preference.
- Two strings stay hardcoded English in `app.js`, matching the pre-existing
  test pins rather than editing those tests' intent: the `"signed in as "`
  identity line (`TestSelfServiceWhoamiUI`) and the analytics-index-off
  degraded-logs message (`TestAdminUI_logsViewDegradesWhenIndexOffButBodiesOn`,
  which greps `app.js`'s own source for the phrase "analytics index"). Moving
  either into `i18n.js` would make the pinned substring disappear from
  `app.js` and fail an existing test that predates this ADR.
- One header cell (`<th>key</th>` in the budget-alerts table) stays
  untranslated for the same reason — `TestAdminUI_alertsTableShowsKeyColumn`
  greps for that exact four-character substring with no attributes.
- The old `<span class="ko">...</span>` duplicate-text idiom and its `.ko` CSS
  rule are removed; the Korean text they carried moved into the dictionary.

## Consequences

- Adding a fifth language means extending every dictionary entry with one more
  locale key — no structural change.
- A translator/reviewer can audit coverage by scanning `i18n.js` alone; no
  string is duplicated between `index.html` and the dictionary except the
  small set of test-pinned English exclusions listed above (each commented
  in place).
- The `#usage-models` routable-models placeholder and the `stat-*-sub`
  overview labels stay in their initially-rendered language until the next
  event that re-renders them (a key issued, a metrics fetch); this is the same
  category of gap as the pinned dynamic strings above and was judged not worth
  a special-cased refresh path.
