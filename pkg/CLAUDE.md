# pkg Module

## Role
Public, importable packages with no dependency on `internal/`. Safe for external
consumers and providers to import.

## Key Packages
- `schema/` — the canonical message schema (Anthropic-superset). `blocks.go` (ContentBlock with `*string` streaming fields + `CacheControl`), `message.go`, `request.go`, `response.go`, `chunk.go`, `extra.go` (unknown-field preservation, case-collision rejection, semantic equality), `model_info.go`, `sse.go` (`WriteAnthropicSSE`), `roundtrip_test.go` (golden fixtures).
- `ulid/` — monotonic ULID (Crockford base32, crypto/rand, big-endian carry) for audit record IDs.

## Rules
- **Canonical schema invariant:** same-protocol round-trip is lossless. Pipeline-interpreted fields are typed; everything else is preserved via `Extra map[string]json.RawMessage`.
- Streaming-frame string fields are `*string` so empty values (`"text":""`) survive a round-trip.
- `extra.go` must reject case-variant key collisions (e.g. `Model` vs `model`) to prevent duplicate-key smuggling.
- No imports from `internal/` — keep `pkg/` consumable by providers and external code.
- Changes here ripple across every provider and ingress; cover with golden fixtures in `roundtrip_test.go`.
