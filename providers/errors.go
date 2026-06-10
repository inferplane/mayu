package providers

import (
	"fmt"
	"net/http"
)

// UpstreamError carries a non-2xx upstream response on the streaming path,
// where the (iter.Seq2, error) signature can't return a ProxyResponse. The
// ingress type-asserts this (errors.As) to tee the real status/body/headers
// to the client verbatim instead of fabricating a gateway error — symmetric
// with Complete's ProxyResponse for non-streaming (design doc §4.4 tee).
type UpstreamError struct {
	StatusCode int
	Body       []byte
	Header     http.Header
}

func (e *UpstreamError) Error() string {
	return fmt.Sprintf("upstream returned status %d", e.StatusCode)
}
