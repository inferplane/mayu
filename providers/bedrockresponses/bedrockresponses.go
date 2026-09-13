// Package bedrockresponses registers an IAM-signed Bedrock Mantle transport
// for native Responses requests. The Responses provider owns the wire protocol;
// this package bounds the destination, authentication and guardrail support.
package bedrockresponses

import (
	"context"
	"errors"
	"iter"
	"net/http"
	"regexp"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/inferplane/inferplane/providers"
	_ "github.com/inferplane/inferplane/providers/openairesponses" // Register the delegated native transport.
)

func init() { providers.Register("bedrock_responses", factory) }

// Match the entire input, including the literal path, so URL normalization
// cannot admit userinfo, ports, escaped paths, queries or fragments. Region
// spelling is restricted to the commercial partition; endpoint/model
// availability within that partition is still an operator choice.
var baseURLPattern = regexp.MustCompile(`^https://bedrock-mantle\.((?:af|ap|ca|eu|il|me|mx|sa|us)-(?:central|north|northeast|northwest|south|southeast|southwest|east|west)-[1-9][0-9]*)\.api\.aws/openai(?:/v1)?/?$`)

// Tests replace this factory before construction; requests retain their own
// credential cache and never consult a mutable global provider.
var credentialProviderFactory = defaultCredentialProvider

func defaultCredentialProvider(ctx context.Context, region string) (aws.CredentialsProvider, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, err
	}
	return cfg.Credentials, nil
}

func factory(cfg providers.Config) (providers.Provider, error) {
	match := baseURLPattern.FindStringSubmatch(cfg.BaseURL)
	if match == nil {
		return nil, errors.New("bedrock_responses: base URL must be an explicit HTTPS commercial Bedrock Mantle /openai or /openai/v1 endpoint")
	}
	region := match[1]
	if cfg.APIKey != "" {
		return nil, errors.New("bedrock_responses: API keys are unsupported; IAM default credentials are required")
	}
	// Live non-bedrock configuration supplies no Settings. Accept only
	// redundant default-chain/region declarations from direct registry callers.
	for key, value := range cfg.Settings {
		if value == "" {
			continue
		}
		if key == "auth_mode" && value == "default" || key == "region" && value == region {
			continue
		}
		return nil, errors.New("bedrock_responses: unsupported settings; use endpoint-derived region and IAM default credentials")
	}
	credentials, err := credentialProviderFactory(context.Background(), region)
	if err != nil || credentials == nil {
		return nil, errors.New("bedrock_responses: AWS credential configuration failed")
	}
	if _, cached := credentials.(*aws.CredentialsCache); !cached {
		credentials = aws.NewCredentialsCache(credentials)
	}
	client := http.Client{}
	if cfg.HTTPClient != nil {
		client = *cfg.HTTPClient
	}
	base := client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	endpoint := strings.TrimSuffix(cfg.BaseURL, "/")
	if !strings.HasSuffix(endpoint, "/v1") {
		endpoint += "/v1"
	}
	client.Transport = &signingTransport{
		base: base, endpoint: endpoint + "/responses", region: region,
		credentials: credentials, signer: v4.NewSigner(),
	}
	cfg.Type = "openai_responses"
	cfg.HTTPClient = &client
	// Config.Credentials is globally injected for the old bedrock broker
	// opt-in. This transport must use only its own default SDK chain.
	cfg.Credentials = nil
	native, err := providers.New(cfg)
	if err != nil {
		return nil, err
	}
	return &provider{Provider: native}, nil
}

// Name is inherited as openai_responses: ingress uses it as the protocol
// identity. Only the unsupported guardrail path needs an additional gate.
type provider struct{ providers.Provider }

func (*provider) SupportsIngress(ingress string) bool { return ingress == "responses" }

func (*provider) checkGuardrail(req *providers.ProxyRequest) error {
	if req != nil && (req.GuardrailID != "" || req.GuardrailVersion != "") {
		return errors.New("bedrock_responses: guardrails are unsupported on the Mantle Responses transport")
	}
	return nil
}

func (p *provider) Complete(ctx context.Context, req *providers.ProxyRequest) (*providers.ProxyResponse, error) {
	if err := p.checkGuardrail(req); err != nil {
		return nil, err
	}
	return p.Provider.Complete(ctx, req)
}

func (p *provider) Stream(ctx context.Context, req *providers.ProxyRequest) (iter.Seq2[*providers.StreamEvent, error], error) {
	if err := p.checkGuardrail(req); err != nil {
		return nil, err
	}
	return p.Provider.Stream(ctx, req)
}

var _ providers.Provider = (*provider)(nil)
