package bedrockresponses

import (
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

func TestSigningClonesRequestAndClearsPriorCredentials(t *testing.T) {
	const body = ` { "model":"upstream", "input": "exact bytes" } `
	r, err := http.NewRequest(http.MethodPost, testBase+"/v1/responses", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header = http.Header{
		"Content-Type": {"application/json"}, "Authorization": {"Bearer private"},
		"Proxy-Authorization": {"private"}, "X-Api-Key": {"private"}, "Api-Key": {"private"},
		"Cookie": {"private"}, "X-Amz-Security-Token": {"private"},
		"X-Amz-Date": {"stale"}, "X-Amz-Content-Sha256": {"wrong"},
	}
	before := r.Clone(r.Context())
	creds := testCredentials
	creds.SessionToken = "" // long-lived IAM keys must not retain a prior session
	transport := &signingTransport{
		endpoint: testBase + "/v1/responses", region: "us-east-1", signer: v4.NewSigner(),
		credentials: aws.NewCredentialsCache(aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return creds, nil
		})),
		base: roundTripFunc(func(signed *http.Request) (*http.Response, error) {
			if signed == r || signed.URL == r.URL || signed.Body != r.Body || signed.GetBody == nil {
				t.Fatal("request clone/replay contract lost")
			}
			for _, key := range []string{"Proxy-Authorization", "X-Api-Key", "Api-Key", "Cookie", "X-Amz-Security-Token"} {
				if signed.Header.Get(key) != "" {
					t.Fatalf("prior credential forwarded: %s", key)
				}
			}
			if signed.Header.Get("X-Amz-Content-Sha256") == "wrong" {
				t.Fatal("stale payload hash forwarded")
			}
			assertSignature(t, signed, body, "us-east-1", creds)
			got, _ := io.ReadAll(signed.Body)
			signed.Body.Close()
			if string(got) != body {
				t.Fatal("signing consumed the original body")
			}
			return response(http.StatusOK, completed), nil
		}),
	}
	resp, err := transport.RoundTrip(r)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !reflect.DeepEqual(r.Header, before.Header) || !reflect.DeepEqual(r.URL, before.URL) ||
		r.ContentLength != before.ContentLength || r.GetBody == nil {
		t.Fatal("signer mutated caller-owned request")
	}
}

func TestSigningFailuresAreSanitizedAndCloseBody(t *testing.T) {
	for _, failure := range []string{"get body", "read body", "sign", "credentials", "wrong endpoint", "host override", "cancelled"} {
		t.Run(failure, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r, err := http.NewRequestWithContext(ctx, http.MethodPost, testBase+"/v1/responses", strings.NewReader("body"))
			if err != nil {
				t.Fatal(err)
			}
			body := &trackedBody{Reader: r.Body}
			r.Body = body
			credentials := aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
				if failure == "credentials" {
					return aws.Credentials{}, errors.New("private credential text")
				}
				if failure == "wrong endpoint" || failure == "host override" || failure == "cancelled" {
					t.Error("rejected request reached credentials")
				}
				return testCredentials, nil
			})
			signer := signerFunc(func(context.Context, aws.Credentials, *http.Request, string, string, string, time.Time, ...func(*v4.SignerOptions)) error {
				if failure != "sign" {
					t.Error("pre-sign failure reached signer")
				}
				return errors.New("private signing text")
			})
			switch failure {
			case "get body":
				r.GetBody = func() (io.ReadCloser, error) { return nil, errors.New("private replay text") }
			case "read body":
				r.GetBody = func() (io.ReadCloser, error) { return &trackedBody{Reader: failingReader{}}, nil }
			case "wrong endpoint":
				r.URL.Host = "evil.test"
			case "host override":
				r.Host = "evil.test"
			case "cancelled":
				cancel()
			}
			transport := &signingTransport{
				endpoint: testBase + "/v1/responses", region: "us-east-1", signer: signer,
				credentials: aws.NewCredentialsCache(credentials), base: forbiddenClient(t).Transport,
			}
			resp, err := transport.RoundTrip(r)
			if resp != nil || err == nil || strings.Contains(err.Error(), "private") || !body.closed {
				t.Fatalf("unsafe/open failed request: error=%v closed=%v", err, body.closed)
			}
			if failure == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("lost cancellation: %v", err)
			}
		})
	}
}

type signerFunc func(context.Context, aws.Credentials, *http.Request, string, string, string, time.Time, ...func(*v4.SignerOptions)) error

func (f signerFunc) SignHTTP(ctx context.Context, credentials aws.Credentials, r *http.Request, hash, service, region string, now time.Time, opts ...func(*v4.SignerOptions)) error {
	return f(ctx, credentials, r, hash, service, region, now, opts...)
}

type trackedBody struct {
	io.Reader
	closed bool
}

func (b *trackedBody) Close() error { b.closed = true; return nil }

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("private body text") }
