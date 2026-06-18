package bedrock

import (
	"context"
	"errors"
	"iter"
	"strings"
	"testing"

	"github.com/aws/smithy-go"
)

// errConverser is a converser whose Converse returns a configurable error, so
// the health classification can be exercised without AWS.
type errConverser struct{ err error }

func (e *errConverser) Converse(_ context.Context, _ string, _ ConverseRequest) (ConverseResponse, error) {
	return ConverseResponse{}, e.err
}
func (e *errConverser) ConverseStream(_ context.Context, _ string, _ ConverseRequest) (iter.Seq2[ConverseStreamEvent, error], error) {
	return nil, e.err
}

func newHealthProvider(err error) *provider {
	return &provider{conv: &errConverser{err: err}}
}

func TestHealthCheck_Success(t *testing.T) {
	res := newHealthProvider(nil).HealthCheck(context.Background())
	if !res.OK {
		t.Fatalf("nil error must be OK, got %+v", res)
	}
}

func TestHealthCheck_PostSignatureErrorsAreHealthy(t *testing.T) {
	for _, code := range []string{"ValidationException", "ResourceNotFoundException", "AccessDeniedException", "ModelNotReadyException", "ThrottlingException"} {
		// ErrorMessage carries a sensitive-looking detail that must NOT leak.
		apiErr := &smithy.GenericAPIError{Code: code, Message: "arn:aws:iam::123456789012:user/secret"}
		res := newHealthProvider(apiErr).HealthCheck(context.Background())
		if !res.OK {
			t.Errorf("%s should be healthy (signature accepted), got %+v", code, res)
		}
		if strings.Contains(res.Detail, "123456789012") || strings.Contains(res.Detail, "arn:aws") {
			t.Errorf("%s detail leaked the error message: %q", code, res.Detail)
		}
	}
}

func TestHealthCheck_CredentialErrorsAreUnhealthy(t *testing.T) {
	for _, code := range []string{"UnrecognizedClientException", "InvalidSignatureException", "ExpiredTokenException", "InvalidClientTokenId"} {
		apiErr := &smithy.GenericAPIError{Code: code, Message: "nope"}
		res := newHealthProvider(apiErr).HealthCheck(context.Background())
		if res.OK {
			t.Errorf("%s must be unhealthy, got %+v", code, res)
		}
	}
}

func TestHealthCheck_TransportErrorUnhealthy(t *testing.T) {
	res := newHealthProvider(errors.New("dial tcp: connection refused")).HealthCheck(context.Background())
	if res.OK {
		t.Fatal("transport error must be unhealthy")
	}
	if strings.Contains(res.Detail, "connection refused") {
		t.Errorf("detail leaked raw error: %q", res.Detail)
	}
}
