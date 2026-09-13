# Adaptive routing and Codex

Use `examples/config.adaptive-routing.json` as an isolated node-local example.
The endpoints, model IDs, context windows, capabilities and prices are
illustrative; replace them with verified deployment values. Qwen, Gemma and Kimi
names do not determine cost, model size, protocol support or data boundary.

## Routing contract

| Requirement | Behavior |
|---|---|
| Weak / normal / strong | `simpleModel` / optional `normalModel` / `complexModel`; local size and latest-user keyword signals |
| Avoid frequent changes | Explicit `stability`; default 5-minute hold, 3 successful requests, 30-minute session TTL |
| PII dedicated model | `InternalOnly` with explicit approved models and internal provider metadata |
| PII masking | `Mask` on detected inspectable content; complete transformation plus reinspection before egress |
| Budget cutover | `budgetTiers.enforceTargets: true`; threshold 100 is supported; all retries stay within the selected model |
| Total hard limit | Remains independent; exhaustion denies even a cheap/internal target |
| Availability | Local decisions and approved backend failover; no central classifier/affinity service |

An incompatible target, unknown boundary, denied model or empty intersection
does not become an external/expensive fallback. Provider cache warmth is a
preference beneath these constraints. A hold can delay a new context preference;
choose its duration according to task quality as well as cache cost.

The session key is scoped to the authenticated key/team, ingress and requested
model. `X-Inferplane-Session-ID` is an optional opaque performance hint. Without
it, mayu derives a stable conversation-prefix fingerprint. Neither identifies
or authorizes a user. Do not put personal information into session headers.
Pins are local and bounded; restarting/rerouting a data plane may lose warmth.

## Example configuration

The example's `auto` model initially resolves to the normal route. Its policy
chooses weak, normal or strong after workload gates pass. It has a $100 switching
threshold and a separate $150 total hard budget. These are demonstration
settings, not recommended spending limits.

Load `governance.yaml` with **one** of `privacy-internal.yaml` or
`privacy-mask.yaml`. Do not load the whole example directory unless the
intersection of both privacy policies is intended. Loading both requires masking
and an internal destination, not either/or.

```bash
mkdir -p /tmp/inferplane-adaptive-demo
mayu pricing check --config examples/config.adaptive-routing.json
mayu keys create --team engineering --models '*' --store /tmp/inferplane-adaptive-demo/keys.db
mayu serve --config examples/config.adaptive-routing.json
```

Supply the referenced environment secrets through your secret manager. The
virtual key is displayed once; do not put it in config files or commit it.

Context starts in Shadow. To activate selection, change that rule's mode to
Enforce after representative evaluation. `normalModel` requires
`maxNormalInputTokens > maxSimpleInputTokens`. The estimator includes the full
request, including tool schemas; these are conservative estimates rather than
a provider tokenizer's exact counts.

Each policy supports at most 256 rules; tier ladders have at most 100 entries.
The CRD includes finite collection bounds and is checked with Kubernetes' native
schema/CEL cost validator in CI, not only with a YAML shape check.

The soft threshold referenced by a strict tier is accounting-only for admission.
It does not replace a higher total hard cap with a smaller blocking limit.
Usage is still measured; absent any real cap, a warn-only meter feeds the routing
decision. Actual costs, including approved low-cost traffic, continue to count
toward the total cap. Existing local/lease accuracy limits still apply.

## Codex local configuration

Configure the custom provider in your user-level `~/.codex/config.toml`.
Project-local provider configuration is not the supported path in current
Codex. Store the gateway virtual key in `INFERPLANE_API_KEY`.

```toml
model = "auto"
model_provider = "inferplane"
web_search = "disabled"

[model_providers.inferplane]
name = "inferplane"
base_url = "http://127.0.0.1:8080/v1"
wire_api = "responses"
env_key = "INFERPLANE_API_KEY"
```

The gateway supports HTTP Responses streaming; this example does not advertise
WebSocket support. `auto` can use the stateless Chat Completions bridge to
approved local models. The separate `codex-native` model demonstrates a native
Responses provider for compatible OpenAI-style upstreams.

The bridge supports text, function/custom tools, tool results and compatible
stateless history. Provider-owned conversation identifiers, encrypted reasoning
transfer, asynchronous response storage and unsupported built-in tools are not
silently converted. Use a native compatible target or a fresh portable
conversation when those features are involved. Models still need their own
coding/tool-use evaluation.

Function-schema strictness is preserved on the Chat bridge; a strict function
request requires a target explicitly declaring `structured_output`. The
Anthropic adapter currently accepts non-strict tools only. It emits a real
Messages envelope and supplies `max_tokens: 4096` when no output bound was sent.
Responses message-phase bookkeeping stays on the Responses side of that adapter.
Failed and truncated attempts retain observed usage, and retry admission checks
the remaining hard budget and current strict target again.

OpenAI reference material used:
- https://developers.openai.com/codex/config-reference
- https://developers.openai.com/codex/config-advanced
- https://developers.openai.com/api/reference/resources/responses/methods/create
- https://developers.openai.com/api/docs/guides/streaming-responses

## Codex with Amazon Bedrock IAM credentials

The `bedrock` provider's Converse/InvokeModel routes do not accept Responses
ingress. Use `bedrock_responses` for Bedrock Mantle's native Responses API:

```json
{
  "providers": {
    "codex-bedrock": {
      "type": "bedrock_responses",
      "base_url": "https://bedrock-mantle.us-west-2.api.aws/openai"
    }
  },
  "models": {
    "openai.gpt-6-astra": {
      "targets": [
        {"provider": "codex-bedrock", "model": "openai.gpt-6-astra"}
      ]
    }
  }
}
```

Merge these entries into an existing gateway configuration and declare the
model's verified price under `pricing.overrides.codex-bedrock`. Confirm model
access in the selected region. The endpoint determines the signing region;
only explicit commercial Bedrock Mantle HTTPS endpoints are accepted.
The Astra example uses `us-west-2`, its documented Mantle region:
[AWS model card](https://docs.aws.amazon.com/bedrock/latest/userguide/model-card-openai-gpt-6-astra.html).

Mayu uses its own standard AWS credential chain and signs requests for the
`bedrock` service. This provider uses IAM credentials, not `api_key_ref` or
the credential broker. Client virtual keys terminate at mayu. The native
Responses transport supplies streaming and usage accounting; the signing
transport preserves request bytes. No Converse conversion is involved.
Requests requiring Bedrock Guardrails are refused because this transport
cannot enforce them.

Use the custom Codex provider configuration above with
`model = "openai.gpt-6-astra"` and the local gateway's actual port. Keep
`supports_websockets = false` and `web_search = "disabled"` unless a supported
search path has separately been configured. The `amazon-bedrock` Codex
provider connects to AWS directly; select the custom `inferplane` provider
to use gateway authentication, routing and accounting.

## Privacy and cache limits

An enabled legacy PII filter also applies to Responses through the complete
request transformer. Clean inspectable requests remain usable; protected text
must satisfy both detector sets, and opaque/unsafe transformations still refuse.

Finite detectors cover email, supported phone formats, Luhn-valid cards, SSNs,
IPv4 and Korean resident-ID shapes. This is not universal PII recognition.
Unknown/opaque content takes the configured internal/block path. Masking refuses
protected structural keys, identifiers or numeric values when a safe
type-preserving transformation is unavailable.

Identical transformed requests produce identical bytes, so subsequent masked
prefixes can still benefit from provider caching. Switching model/provider or
introducing masking starts a different cache namespace. No cache-hit or savings
percentage is promised without measurements.

## Availability and verification

Mayu performs classification, policy inspection and pin lookup locally.
Backend failover is constrained by the same PII, capability, region and cost
limits. Replicate approved backends and keep control-plane management off the
inference path. Global monetary authority now has an opt-in Postgres ledger and
durable node journals; see [durable budgets](durable-budgets.md) and
[ADR-045](decisions/ADR-045-durable-budget-authority.md). For shared keys and global rate/token quotas, use the explicit
[shared gateway profile](shared-governance.md), which requires an available HA
Postgres service. The node-local profile retains its existing rate/key limits.

Regular repository tests use fake providers and disposable stores. An optional
installed-client check performs a real Codex CLI tool round trip against local
fake upstreams with isolated client configuration:

```bash
INFERPLANE_TEST_CODEX=1 go test ./cmd/mayu -run TestE2ECodexCLI -v -count=1
```

This checks the protocol and gateway pipeline. It does not call a real model,
prove production model quality, or certify multi-replica budget correctness.
