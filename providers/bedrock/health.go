package bedrock

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/smithy-go"
	"github.com/inferplane/inferplane/providers"
)

// probeModelID is a dummy model id used only by the health probe. The probe
// never needs the model to exist: AWS validates the SigV4 signature BEFORE any
// model-level check, so a ValidationException/ResourceNotFoundException proves
// the credentials resolved (ADR-014 D2) — and, because the call is rejected
// before inference, it spends no tokens.
const probeModelID = "inferplane-health-probe"

// credentialErrorCodes are the smithy API error codes that mean the SigV4
// signature/credentials were REJECTED — the only class that maps to OK:false.
// Every other API error code is returned only AFTER the signature is accepted,
// so it proves the credentials resolve (healthy).
var credentialErrorCodes = map[string]bool{
	"UnrecognizedClientException": true,
	"InvalidSignatureException":   true,
	"SignatureDoesNotMatch":       true,
	"ExpiredTokenException":       true,
	"ExpiredToken":                true,
	"InvalidClientTokenId":        true,
	"MissingAuthenticationToken":  true,
	"IncompleteSignature":         true,
	"InvalidAccessKeyId":          true,
}

// HealthCheck probes Bedrock with a bounded 1-token Converse to a dummy model
// (ADR-014 D2). It uses the same IAM action (bedrock:InvokeModel/Converse) the
// data plane already needs — NOT ListFoundationModels. Classification is by the
// inverse rule: only signature/credential errors are unhealthy; a success OR any
// post-signature service error (AccessDenied, ValidationException,
// ResourceNotFound, ModelNotReady, …) is healthy. Detail carries only the AWS
// error CODE, never the error message (which can include account ids/arns).
func (p *provider) HealthCheck(ctx context.Context) providers.HealthResult {
	start := time.Now()
	_, err := p.conv.Converse(ctx, probeModelID, ConverseRequest{
		Messages:  []ConverseMessage{{Role: "user", Text: "ping"}},
		Inference: map[string]any{"maxTokens": 1},
	})
	latency := time.Since(start).Milliseconds()
	if err == nil {
		return providers.HealthResult{OK: true, LatencyMS: latency, Detail: "ok"}
	}
	if ctx.Err() != nil {
		return providers.HealthResult{OK: false, LatencyMS: latency, Detail: "probe timed out"}
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		code := apiErr.ErrorCode()
		if credentialErrorCodes[code] {
			return providers.HealthResult{OK: false, LatencyMS: latency, Detail: fmt.Sprintf("credentials rejected (%s)", code)}
		}
		// Returned after the signature was accepted ⇒ credentials resolve.
		return providers.HealthResult{OK: true, LatencyMS: latency, Detail: fmt.Sprintf("credentials valid (upstream returned %s)", code)}
	}
	// Not an API error: transport failure or credentials that never resolved —
	// cannot prove health. Report unhealthy without echoing the raw error.
	return providers.HealthResult{OK: false, LatencyMS: latency, Detail: "credentials unresolved or upstream unreachable"}
}
