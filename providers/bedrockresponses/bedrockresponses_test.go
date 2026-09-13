package bedrockresponses

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/inferplane/inferplane/providers"
)

const testBase = "https://bedrock-mantle.us-east-1.api.aws/openai"
const completed = `{"id":"r","model":"upstream","status":"completed","output":[],"usage":{"input_tokens":10,"input_tokens_details":{"cached_tokens":8},"output_tokens":2}}`

// These are deliberately fake signing inputs, never environment credentials.
var testCredentials = aws.Credentials{
	AccessKeyID: "test-access", SecretAccessKey: "test-secret", SessionToken: "test-session",
}

func TestRegistryReturnsNativeResponses(t *testing.T) {
	stubCredentials(t, aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		t.Fatal("construction must not retrieve credentials")
		return aws.Credentials{}, nil
	}))
	p, err := providers.New(providers.Config{
		Type: "bedrock_responses", BaseURL: testBase,
		// This is globally injected for broker consumers; this provider must
		// neither reject it nor use it.
		Credentials: forbiddenBroker{t},
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.Name() != "openai_responses" {
		t.Fatalf("ingress needs native Responses transport, got %q", p.Name())
	}
	support, ok := p.(interface{ SupportsIngress(string) bool })
	if !ok || !support.SupportsIngress("responses") || support.SupportsIngress("openai") {
		t.Fatal("lost native ingress capability")
	}
}

func TestGuardrailsRefusedBeforeCredentialsOrTransport(t *testing.T) {
	var retrieves atomic.Int32
	stubCredentials(t, aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		retrieves.Add(1)
		return testCredentials, nil
	}))
	p := newTestProvider(t, providers.Config{HTTPClient: forbiddenClient(t)})
	for _, fields := range [][2]string{{"private-id", ""}, {"", "private-version"}, {"private-id", "private-version"}} {
		req := simpleRequest()
		req.GuardrailID, req.GuardrailVersion = fields[0], fields[1]
		if resp, err := p.Complete(context.Background(), req); resp != nil || err == nil ||
			!strings.Contains(err.Error(), "guardrail") || strings.Contains(err.Error(), "private") {
			t.Fatalf("unguarded or unsafe Complete: %v", err)
		}
		req.Stream = true
		req.RawBody = []byte(`{"model":"public","stream":true}`)
		if stream, err := p.Stream(context.Background(), req); stream != nil || err == nil ||
			!strings.Contains(err.Error(), "guardrail") || strings.Contains(err.Error(), "private") {
			t.Fatalf("unguarded or unsafe Stream: %v", err)
		}
	}
	if retrieves.Load() != 0 {
		t.Fatal("guardrail refusal retrieved credentials")
	}
}

func TestEndpointAndAuthValidationBeforeCredentials(t *testing.T) {
	old := credentialProviderFactory
	credentialProviderFactory = func(context.Context, string) (aws.CredentialsProvider, error) {
		t.Fatal("invalid configuration reached AWS credential loading")
		return nil, nil
	}
	t.Cleanup(func() { credentialProviderFactory = old })
	for _, base := range []string{
		"", "http://bedrock-mantle.us-east-1.api.aws/openai",
		"https://user:private@bedrock-mantle.us-east-1.api.aws/openai",
		testBase + "?secret=private", testBase + "?", testBase + "#private", testBase + "#",
		"https://bedrock-mantle.us-east-1.api.aws:443/openai",
		"https://bedrock-mantle.us-east-1.api.aws:/openai",
		"https://bedrock-mantle.us-east-1.api.aws.evil.test/openai",
		"https://bedrock-runtime.us-east-1.api.aws/openai",
		"https://bedrock-mantle.us-east-1.api.aws./openai",
		"https://bedrock-mantle.US-EAST-1.api.aws/openai",
		"https://bedrock-mantle.cn-north-1.api.aws/openai",
		"https://bedrock-mantle.us-gov-west-1.api.aws/openai",
		"https://bedrock-mantle.us-iso-east-1.api.aws/openai",
		"https://bedrock-mantle.us-east-1.amazonaws.com/openai",
		"https://localhost/openai", "https://127.0.0.1/openai",
		"https://bedrock-mantle..api.aws/openai", "https://bedrock-mantle.us-east-1.api.aws",
		testBase + "//", testBase + "/responses", testBase + "/v2",
		testBase + "/../openai", testBase + "/%76%31", testBase + "%2fv1",
		"https://bedrock-mantle.us-east-1.api.aws/%6fpenai", " " + testBase,
	} {
		t.Run(base, func(t *testing.T) {
			p, err := providers.New(providers.Config{Type: "bedrock_responses", BaseURL: base})
			if p != nil || err == nil || strings.Contains(err.Error(), "private") {
				t.Fatalf("invalid/secret-bearing endpoint accepted or echoed: %v", err)
			}
		})
	}
	for _, cfg := range []providers.Config{
		{APIKey: "private"},
		{Settings: map[string]string{"auth_mode": "broker"}},
		{Settings: map[string]string{"auth_mode": "profile", "profile": "private"}},
		{Settings: map[string]string{"auth_mode": "static"}},
		{Settings: map[string]string{"profile": "private"}},
		{Settings: map[string]string{"region": "us-west-2"}},
		{Settings: map[string]string{"endpoint": "private"}},
		{Settings: map[string]string{"guardrail_id": "private"}},
	} {
		cfg.Type, cfg.BaseURL = "bedrock_responses", testBase
		p, err := providers.New(cfg)
		if p != nil || err == nil || strings.Contains(err.Error(), "private") {
			t.Fatalf("unsupported authentication/settings accepted or echoed: %v", err)
		}
	}
}

func TestNativeBodySigningAndUsage(t *testing.T) {
	stubCredentials(t, aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return testCredentials, nil
	}))
	const raw = ` { "input" : [{"type":"reasoning","encrypted_content":"opaque"}], "model" : "public", "store":false, "unknown": [1,2] } `
	const rewritten = ` { "input" : [{"type":"reasoning","encrypted_content":"opaque"}], "model" : "upstream", "store":false, "unknown": [1,2] } `
	for _, tc := range []struct{ base, upstream, want, region string }{
		{testBase, "public", raw, "us-east-1"},
		{testBase + "/v1", "upstream", rewritten, "us-east-1"},
		{"https://bedrock-mantle.eu-west-1.api.aws/openai/", "upstream", rewritten, "eu-west-1"},
		{"https://bedrock-mantle.ap-southeast-2.api.aws/openai/v1/", "upstream", rewritten, "ap-southeast-2"},
	} {
		t.Run(tc.base, func(t *testing.T) {
			calls := 0
			transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != http.MethodPost || r.URL.Path != "/openai/v1/responses" {
					t.Fatalf("wrong native endpoint: %s %s", r.Method, r.URL.Path)
				}
				body, err := io.ReadAll(r.Body)
				r.Body.Close()
				if err != nil || string(body) != tc.want || r.ContentLength != int64(len(tc.want)) {
					t.Fatalf("body/length changed: %q, length %d, error %v", body, r.ContentLength, err)
				}
				replay, err := r.GetBody()
				if err != nil {
					t.Fatal(err)
				}
				replayed, err := io.ReadAll(replay)
				replay.Close()
				if err != nil || string(replayed) != tc.want {
					t.Fatal("signing consumed or replaced replay body")
				}
				assertSignature(t, r, tc.want, tc.region, testCredentials)
				for _, key := range []string{"X-Api-Key", "Proxy-Authorization", "X-Client-Secret", "Cookie"} {
					if r.Header.Get(key) != "" {
						t.Fatalf("forwarded client header %s", key)
					}
				}
				return response(http.StatusOK, completed), nil
			})
			client := &http.Client{Transport: transport, Timeout: time.Minute}
			p := newTestProvider(t, providers.Config{
				BaseURL: tc.base, HTTPClient: client, Credentials: forbiddenBroker{t},
				Settings: map[string]string{"auth_mode": "default", "region": tc.region, "profile": ""},
			})
			req := &providers.ProxyRequest{
				IngressProtocol: "responses", Model: "public", Upstream: tc.upstream, RawBody: []byte(raw),
				Headers: http.Header{
					"Authorization": {"Bearer client"}, "X-Api-Key": {"gateway"},
					"X-Client-Secret": {"private"}, "Cookie": {"private"},
				},
			}
			resp, err := p.Complete(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			if calls != 1 || string(resp.RawBody) != completed || resp.Parsed == nil ||
				resp.Parsed.Model != "public" || *resp.Parsed.Usage.InputTokens != 2 ||
				*resp.Parsed.Usage.CacheReadInputTokens != 8 || *resp.Parsed.Usage.OutputTokens != 2 {
				t.Fatalf("native response/accounting lost: %+v", resp)
			}
			if string(req.RawBody) != raw || req.Headers.Get("Authorization") != "Bearer client" ||
				reflect.ValueOf(client.Transport).Pointer() != reflect.ValueOf(transport).Pointer() ||
				client.Timeout != time.Minute || client.CheckRedirect != nil {
				t.Fatal("caller-owned request/client changed")
			}
		})
	}
}

func TestNativeStreamAndRedirectRejection(t *testing.T) {
	stubCredentials(t, aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return testCredentials, nil
	}))
	const frames = ": keepalive\r\n\r\nevent: response.completed\r\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":10,\"input_tokens_details\":{\"cached_tokens\":8},\"output_tokens\":2}}}\r\n\r\n"
	calls := 0
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			t.Fatal("native provider must reject redirects without consulting caller")
			return nil
		},
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			r.Body.Close()
			if calls == 1 {
				if r.Header.Get("Accept") != "text/event-stream" {
					t.Fatal("native streaming request lost Accept")
				}
				return response(http.StatusOK, frames), nil
			}
			resp := response(http.StatusTemporaryRedirect, "private")
			resp.Header.Set("Location", "https://evil.test/collect")
			return resp, nil
		}),
	}
	p := newTestProvider(t, providers.Config{HTTPClient: client})
	req := &providers.ProxyRequest{
		IngressProtocol: "responses", Model: "public", Upstream: "upstream",
		Stream: true, RawBody: []byte(`{"model":"public","stream":true}`),
	}
	stream, err := p.Stream(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	var raw strings.Builder
	sawUsage := false
	for ev, err := range stream {
		if err != nil {
			t.Fatal(err)
		}
		raw.Write(ev.Raw)
		if ev.Chunk != nil && ev.Chunk.Usage != nil {
			sawUsage = *ev.Chunk.Usage.InputTokens == 2 && *ev.Chunk.Usage.CacheReadInputTokens == 8 &&
				*ev.Chunk.Usage.OutputTokens == 2
		}
	}
	if raw.String() != frames || !sawUsage {
		t.Fatal("native SSE bytes/accounting changed")
	}
	_, err = p.Stream(context.Background(), req)
	var upstream *providers.UpstreamError
	if !errors.As(err, &upstream) || upstream.StatusCode != http.StatusTemporaryRedirect || calls != 2 ||
		strings.Contains(string(upstream.Body), "private") {
		t.Fatalf("redirect was followed or exposed: %v, calls=%d", err, calls)
	}
}

func TestCredentialFailuresAndCancellation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		creds aws.Credentials
		err   error
	}{
		{"retrieve error", aws.Credentials{}, errors.New("private credential process text")},
		{"missing keys", aws.Credentials{}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubCredentials(t, aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
				return tc.creds, tc.err
			}))
			p := newTestProvider(t, providers.Config{HTTPClient: forbiddenClient(t)})
			resp, err := p.Complete(context.Background(), simpleRequest())
			if resp != nil || err == nil || strings.Contains(err.Error(), "private") {
				t.Fatalf("unsafe credential failure: %v", err)
			}
		})
	}
	t.Run("load failure", func(t *testing.T) {
		old := credentialProviderFactory
		credentialProviderFactory = func(context.Context, string) (aws.CredentialsProvider, error) {
			return nil, errors.New("private profile text")
		}
		t.Cleanup(func() { credentialProviderFactory = old })
		p, err := providers.New(providers.Config{Type: "bedrock_responses", BaseURL: testBase})
		if p != nil || err == nil || strings.Contains(err.Error(), "private") {
			t.Fatalf("unsafe credential configuration failure: %v", err)
		}
	})
	t.Run("cancel while retrieving", func(t *testing.T) {
		entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
		t.Cleanup(func() { close(release); <-finished })
		stubCredentials(t, aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			defer close(finished)
			close(entered)
			// AWS cache deliberately shares refresh independently of any one
			// request's cancellation; the waiting request must still return.
			<-release
			return testCredentials, nil
		}))
		p := newTestProvider(t, providers.Config{HTTPClient: forbiddenClient(t)})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		result := make(chan error, 1)
		go func() {
			_, err := p.Complete(ctx, simpleRequest())
			result <- err
		}()
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("credential retrieval never started")
		}
		cancel()
		select {
		case err := <-result:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("lost cancellation: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("request blocked on credential refresh after cancellation")
		}
	})
}

func TestExpiredCredentialsRefreshBeforeSigning(t *testing.T) {
	var retrieves atomic.Int32
	stubCredentials(t, aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		creds := testCredentials
		creds.CanExpire = true
		creds.Expires = time.Now().Add(time.Hour)
		if retrieves.Add(1) == 1 {
			creds.AccessKeyID = "expired-test-access"
			creds.Expires = time.Now().Add(-time.Hour)
		}
		return creds, nil
	}))
	var auth []string
	p := newTestProvider(t, providers.Config{HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		auth = append(auth, r.Header.Get("Authorization"))
		r.Body.Close()
		return response(http.StatusOK, completed), nil
	})}})
	for range 3 {
		if _, err := p.Complete(context.Background(), simpleRequest()); err != nil {
			t.Fatal(err)
		}
	}
	if retrieves.Load() != 2 || !strings.Contains(auth[0], "Credential=expired-test-access/") ||
		!strings.Contains(auth[1], "Credential=test-access/") || !strings.Contains(auth[2], "Credential=test-access/") {
		t.Fatalf("credentials frozen at construction or cache unused: retrieves=%d, auth=%v", retrieves.Load(), auth)
	}
}

func stubCredentials(t *testing.T, creds aws.CredentialsProvider) {
	t.Helper()
	old := credentialProviderFactory
	credentialProviderFactory = func(_ context.Context, region string) (aws.CredentialsProvider, error) {
		if region == "" {
			t.Fatal("endpoint did not supply signing region")
		}
		return creds, nil
	}
	t.Cleanup(func() { credentialProviderFactory = old })
}

func newTestProvider(t *testing.T, cfg providers.Config) providers.Provider {
	t.Helper()
	cfg.Type = "bedrock_responses"
	if cfg.BaseURL == "" {
		cfg.BaseURL = testBase
	}
	p, err := providers.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func simpleRequest() *providers.ProxyRequest {
	return &providers.ProxyRequest{IngressProtocol: "responses", Model: "public", Upstream: "upstream", RawBody: []byte(`{"model":"public","input":"hi"}`)}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func response(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
}

func forbiddenClient(t *testing.T) *http.Client {
	t.Helper()
	return &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("failure reached upstream transport")
		return nil, errors.New("unexpected transport")
	})}
}

type forbiddenBroker struct{ t *testing.T }

func (f forbiddenBroker) Credentials(context.Context) (string, string, string, time.Time, error) {
	f.t.Error("global broker credentials used by IAM default-chain provider")
	return "", "", "", time.Time{}, errors.New("unexpected broker")
}

// Verify the signature independently, including the real payload hash. Merely
// matching an AWS4 prefix would also pass for a signature over an empty body.
func assertSignature(t *testing.T, r *http.Request, body, region string, creds aws.Credentials) {
	t.Helper()
	date := r.Header.Get("X-Amz-Date")
	if _, err := time.Parse("20060102T150405Z", date); err != nil {
		t.Fatalf("invalid signing date: %q", date)
	}
	scope := date[:8] + "/" + region + "/bedrock/aws4_request"
	prefix := "AWS4-HMAC-SHA256 Credential=" + creds.AccessKeyID + "/" + scope + ", SignedHeaders="
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, prefix) || r.Header.Get("X-Amz-Security-Token") != creds.SessionToken {
		t.Fatalf("wrong signing scope/session: %q", auth)
	}
	signed, signature, ok := strings.Cut(strings.TrimPrefix(auth, prefix), ", Signature=")
	if !ok {
		t.Fatal("missing signature")
	}
	var headers strings.Builder
	for _, key := range strings.Split(signed, ";") {
		value := r.Header.Get(key)
		switch key {
		case "host":
			value = r.URL.Host
		case "content-length":
			// The signer includes ContentLength independently of Header.
			value = strings.TrimSpace(r.Header.Get("Content-Length"))
			if value == "" {
				value = fmt.Sprint(r.ContentLength)
			}
		}
		headers.WriteString(key + ":" + strings.Join(strings.Fields(value), " ") + "\n")
	}
	hash := func(s string) string {
		sum := sha256.Sum256([]byte(s))
		return hex.EncodeToString(sum[:])
	}
	canonical := r.Method + "\n/openai/v1/responses\n\n" + headers.String() + "\n" + signed + "\n" + hash(body)
	mac := func(key []byte, s string) []byte {
		h := hmac.New(sha256.New, key)
		h.Write([]byte(s))
		return h.Sum(nil)
	}
	key := mac([]byte("AWS4"+creds.SecretAccessKey), date[:8])
	key = mac(key, region)
	key = mac(key, "bedrock")
	key = mac(key, "aws4_request")
	want := hex.EncodeToString(mac(key, "AWS4-HMAC-SHA256\n"+date+"\n"+scope+"\n"+hash(canonical)))
	if signature != want {
		t.Fatal("signature does not authenticate the exact native request body")
	}
}
