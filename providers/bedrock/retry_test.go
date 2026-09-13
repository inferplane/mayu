package bedrock

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
)

func TestGenerationSDKCannotRetryBelowGatewayReservation(t *testing.T) {
	for _, operation := range []string{"invoke", "invoke-stream", "converse", "converse-stream"} {
		t.Run(operation, func(t *testing.T) {
			var calls atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(500)
				_, _ = w.Write([]byte(`{"__type":"InternalServerException","message":"retryable test failure"}`))
			}))
			defer srv.Close()
			cfg := aws.Config{Region: "us-east-1", Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
				return aws.Credentials{AccessKeyID: "test-id", SecretAccessKey: "test-secret"}, nil
			}), HTTPClient: srv.Client(),
				Retryer: func() aws.Retryer {
					return retry.NewStandard(func(o *retry.StandardOptions) {
						o.MaxAttempts = 2
						o.Backoff = retry.BackoffDelayerFunc(func(int, error) (time.Duration, error) { return 0, nil })
					})
				}}
			c := &awsClient{rt: bedrockruntime.NewFromConfig(cfg, func(o *bedrockruntime.Options) { o.BaseEndpoint = aws.String(srv.URL) })}
			var err error
			switch operation {
			case "invoke":
				_, err = c.Invoke(context.Background(), "model", []byte(`{}`), Guardrail{})
			case "invoke-stream":
				_, err = c.InvokeStream(context.Background(), "model", []byte(`{}`), Guardrail{})
			case "converse":
				_, err = c.Converse(context.Background(), "model", ConverseRequest{})
			case "converse-stream":
				_, err = c.ConverseStream(context.Background(), "model", ConverseRequest{})
			}
			if err == nil || calls.Load() != 1 {
				t.Fatalf("gateway attempt caused %d transport attempts; error=%v", calls.Load(), err)
			}
		})
	}
}
