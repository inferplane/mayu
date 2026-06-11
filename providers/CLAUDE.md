# providers Module

## Role
The upstream-provider extension surface. Every provider implements one interface and
lives in its own package. This is the project's headline extensibility promise
(design §8): **a new provider is one package + one blank-import line, with zero core diff.**

## Key Files
- `provider.go` — the `Provider` interface (`Name`, `Models`, `Complete`, `Stream`), optional `TokenCounter`, and transport types (`ProxyRequest`, `ProxyResponse`, `StreamEvent`, `IngressProtocol`).
- `registry.go` — `Register(type, factory)` / `New(Config)`.
- `errors.go` — `UpstreamError{StatusCode, Body, Header}` (so non-2xx upstream responses tee through losslessly).
- `anthropic/` — Messages passthrough; verbatim body, gateway-injected `x-api-key`; byte-exact SSE reader.
- `bedrock/` — Claude via InvokeModel, non-Claude via Converse; AWS SDK isolated behind invoker/converser interfaces.
- `openaicompat/` — vLLM/Ollama/any OpenAI endpoint; order-preserving model rewrite.
- `testing/mockprovider/` — deterministic provider for unit tests.

## Rules — adding a provider
1. Create `providers/<name>/<name>.go` implementing `Provider` (and `TokenCounter` if it supports counting).
2. Call `providers.Register("<type>", factory)` in an `init()`.
3. Add one blank import in `cmd/inferplane/main.go`.
4. Document it in `docs/reference/agent-llm.md`.
5. **Do not touch core packages.** A provider PR that edits `internal/*` has violated the boundary.

## Invariants
- When provider protocol == ingress protocol, forward `RawBody` verbatim (cache safety).
- Streaming: `Stream` returns an `iter.Seq2[*StreamEvent, error]`; never retry mid-stream (pre-TTFT failover only).
- Cache-affecting rewrites (e.g. Bedrock model injection) must be top-level-only so `cache_control` stays byte-stable.
