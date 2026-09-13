package bedrockresponses

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

type signingTransport struct {
	base        http.RoundTripper
	endpoint    string
	region      string
	credentials aws.CredentialsProvider
	signer      v4.HTTPSigner
}

func (t *signingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// RoundTripper owns closing the body even when it fails before dispatch.
	// After dispatch, the underlying transport owns it.
	dispatched := false
	defer func() {
		if !dispatched && req.Body != nil {
			req.Body.Close()
		}
	}()
	ctx := req.Context()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req.URL == nil || req.URL.String() != t.endpoint ||
		req.Host != "" && req.Host != req.URL.Host {
		return nil, errors.New("bedrock_responses: invalid signing destination")
	}
	// Clone includes headers and URL but preserves Body and GetBody. Hash a
	// replay, leaving the exact native payload and its replay function intact.
	signed := req.Clone(ctx)
	hash := sha256.New()
	if req.Body != nil && req.Body != http.NoBody {
		if req.GetBody == nil {
			return nil, errors.New("bedrock_responses: request signing failed")
		}
		body, err := req.GetBody()
		if err != nil {
			return nil, errors.New("bedrock_responses: request signing failed")
		}
		_, err = io.Copy(hash, body)
		body.Close()
		if err != nil {
			return nil, errors.New("bedrock_responses: request signing failed")
		}
	}
	// Native Responses never forwards ingress headers. Also clear any prior
	// authentication on the cloned transport request before applying IAM.
	for _, key := range []string{
		"Authorization", "Proxy-Authorization", "X-Api-Key", "Api-Key", "Cookie",
		"X-Amz-Security-Token", "X-Amz-Date", "X-Amz-Content-Sha256",
	} {
		signed.Header.Del(key)
	}
	credentials, err := t.credentials.Retrieve(ctx)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil || !credentials.HasKeys() {
		return nil, errors.New("bedrock_responses: AWS credential retrieval failed")
	}
	if err := t.signer.SignHTTP(ctx, credentials, signed, hex.EncodeToString(hash.Sum(nil)), "bedrock", t.region, time.Now()); err != nil {
		return nil, errors.New("bedrock_responses: request signing failed")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dispatched = true
	return t.base.RoundTrip(signed)
}

var _ http.RoundTripper = (*signingTransport)(nil)
