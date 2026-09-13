# Bedrock native Responses transport

`bedrock_responses` delegates the Responses protocol to `openai_responses`
through an AWS SigV4 signing HTTP transport. Its protocol name remains
`openai_responses`, so Responses ingress retains native request bytes, the
existing top-level model rewrite, native SSE, usage accounting, and redirect
rejection.

Configure an explicit `base_url`, for example:

```json
{
  "type": "bedrock_responses",
  "base_url": "https://bedrock-mantle.us-east-1.api.aws/openai"
}
```

The only accepted base paths are `/openai` and `/openai/v1`, each with an
optional trailing slash, on an exact commercial
`https://bedrock-mantle.<region>.api.aws` host. Userinfo, ports, query strings,
fragments, encoded paths, other hosts, and other paths are rejected. The
endpoint fixes the signing region; the signing service is always `bedrock`.
There is no default endpoint or implicit region. Endpoint validation does not
guarantee a model or Mantle is available in that region.

Authentication is **IAM-only**, using the AWS SDK default credential chain
(including its environment/shared-profile and workload-role support).
Credentials are retrieved through the AWS credential cache on every request
and refreshed when needed. Do not configure an API key, a per-provider profile,
or broker authentication. The globally supplied `providers.Config.Credentials`
broker source is ignored. Production configuration does not require
`providers.Config.Settings`.

**Bedrock Guardrails are unsupported on this transport.** Both `Complete` and
`Stream` refuse any request with `GuardrailID` or `GuardrailVersion` before
credential retrieval or upstream dispatch. There is no broker or guardrail
fallback path.

Signing clones the request and headers, hashes the replay body without
consuming the outgoing bytes, and removes prior authentication headers.
Credential configuration, retrieval, and signing errors use fixed messages
without underlying credential text. Cancellation stops a request waiting for
credentials; the SDK may continue a shared refresh for other callers.

Tests inject a credential-provider factory and capturing HTTP transports;
they require no AWS credentials or network access. They establish transport
behavior, not live IAM permissions, model availability, or model quality.
The application must blank-import this package to enable its registration.
