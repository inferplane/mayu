# Final branch fix wave — I1 / M1

Workspace: `/tmp/inferplane-policy-routing`. Base: `0eb1b64`.

I1 is fixed in the console replacement caller. Provider edits load the existing
secret-free export, preserve unexposed settings (including auth header/profile),
prefill the boundary and refs, and send the edited boundary in PUT. Model edits
prefill context/capabilities and preserve existing aliases and ordered targets.
Clearing is explicit; successful saves and NEW actions reset the draft. A reset
invalidates an in-flight provider edit fetch. Failed writes retain the draft.

M1 is corrected: supported native Bedrock text requests can use the example's
OpenAI-compatible targets, subject to the current cross-protocol feature limits.
No provider implementation or runtime dependency was added.

## Form contract for parent browser QA

Flow: console `/` → unlock → Providers & Models → existing provider/model EDIT →
verify prefill → SAVE → inspect actual PUT → GET/export readback → protected
request still routes internally; then exercise deliberate clearing and NEW.

| Control | Contract |
|---|---|
| `#pf-data-boundary` | Select `""` = unknown/clear, `internal`, `external`; a stored `unknown` prefills the unknown option |
| `#mf-context-window` | Number; blank or 0 emits `context_window: 0`; nonnegative safe integers only |
| `#mf-cap-tools` | Checkbox emits `tools` when selected |
| `#mf-cap-vision` | Checkbox emits `vision` when selected |
| `#mf-cap-reasoning` | Checkbox emits `reasoning` when selected |
| `#mf-cap-structured-output` | Checkbox emits `structured_output` when selected |
| `#pf-new`, `#mf-new` | Native reset buttons; discard prior edit state |

Model PUT includes `targets`, `aliases`, `context_window`, `capabilities`.
Unchecking all capabilities emits `[]`. Aliases are retained for the edited name;
a different/new name does not inherit them. Provider Edit waits for
`GET /admin/config/export`; wait for populated fields before saving. Export
failure leaves the form untouched and reports an error. Authentication still
uses the existing in-memory token and `api()` helper. No inline styles/handlers,
browser persistence, CDN resources, package setup, or installations were added.

## Red → green evidence

Before the production patch, the new behavioral harness executed shipped
`app.js` and its submit handlers. It emitted:

```json
{"provider":{"path":"/admin/providers/private","body":{"type":"anthropic","base_url":"https://private.invalid","region":"us-east-1"}},"model":{"path":"/admin/models/private","body":{"targets":[{"provider":"private","model":"upstream","api":"invoke_model"},{"provider":"private","model":"backup"}]}}}
```

`TestRoutingFormBehavior` failed with `ordinary provider save erased boundary`
(`undefined` instead of `internal`). The real PUT/readback regression also failed:
boundary/ref, context/capabilities/aliases were lost, and routing returned
`no_safe_route`. These were observed failures, not inferred assertions.

After the patch, this focused command passed with the race detector:

```bash
env GOCACHE=/tmp/inferplane-go-build GOPROXY=off go test \
  ./internal/server/adminui ./internal/server/configapi \
  -run 'TestRoutingFormBehavior|TestConsoleReplacementPayloadKeepsRoutingUsable|TestRoutingMetadata' \
  -race -count=1
```

Both packages passed. `node --check` passed for `app.js` and `i18n.js`;
`gofmt` was applied to the two new Go files; `git diff --check` passed.

The deterministic form harness uses only Node's standard library, observes actual
submit-handler fetch payloads, and tests preserve/edit/clear/reset behavior,
env/file refs, bearer auth, IAM profile/guardrails, failed-save retry, invalid
context values, alias isolation, and reset during an in-flight export.
The Go tests skip explicitly if Node is unavailable; Node is installed here.
No new source-string presence assertions were added.

`TestConsoleReplacementPayloadKeepsRoutingUsable` consumes these generated
payloads, drives the production PUT handlers with a real SQLite store, performs
GET readback, builds the production live topology and routes protected text via
the preserved alias to both internal targets. Its test adapter only maps Writer
method names to SQLite operations; it does not reproduce payload parsing or
replacement logic. It runs without sockets, credentials, or upstream requests.

## Exact files

- `internal/server/adminui/static/app.js`
- `internal/server/adminui/static/index.html`
- `internal/server/adminui/static/i18n.js`
- `internal/server/adminui/static/style.css`
- `internal/server/adminui/routing_forms_test.go`
- `internal/server/adminui/testdata/routing_forms.cjs`
- `internal/server/configapi/console_routing_test.go`
- `docs/policy-routing.md`
- `final-fix-report.md`

## Pending parent proof / gates

Browser plugin not available. Per task ownership, the parent runs the installed
Python Playwright/Chromium QA. No browser was launched in this fix wave. Page
identity, visible prefill/clear/reset, console/CSP health, desktop/mobile
screenshots, and the actual browser → full gateway → routing interaction remain
pending. The DOM-boundary harness is not rendered browser evidence.

An attempted existing admin UI package run stopped in `TestServesIndex`: its
shared helper binds an httptest listener, blocked by the sandbox (`socket:
operation not permitted`). No socket approval was requested; the controller owns
that suite and the final full build/race/vet/format/harness gates. Focused tests
above passed independently without listeners.

No subagents, push, merge, deploy, or dependency installation.
