package configapi

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/inferplane/inferplane/internal/providerstore"
)

// envRefRe is the allowed shape of an env-var secret ref: a POSIX-ish env var
// name. A pasted secret (sk-…, dashes, mixed case with leading digits) fails it,
// so a secret value can never be stored as a "ref" (ADR-008 gate C1).
var envRefRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// secretRefWrite is the auth reference in a write body — a NAME, never a value.
type secretRefWrite struct {
	Env  string `json:"env,omitempty"`
	File string `json:"file,omitempty"`
}

// ProviderWrite is the register/replace DTO. It has NO field that can hold a
// secret value — only the ref (env name / file path) and the bedrock IAM mode.
type ProviderWrite struct {
	Type      string          `json:"type"`
	BaseURL   string          `json:"base_url,omitempty"`
	Region    string          `json:"region,omitempty"`
	Auth      authWrite       `json:"auth,omitempty"`
	APIKeyRef *secretRefWrite `json:"api_key_ref,omitempty"`
}

type authWrite struct {
	Mode    string `json:"mode,omitempty"`
	Profile string `json:"profile,omitempty"`
}

// ModelWrite is the model-route DTO: an ordered target chain.
type ModelWrite struct {
	Targets []targetWrite `json:"targets"`
}

type targetWrite struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	API      string `json:"api,omitempty"`
}

// ParseProviderWrite validates a provider write body and returns the row to
// persist. It (1) rejects an inline api_key (§7, the same probe config.Load
// runs), (2) requires a type, (3) validates the ref SHAPE (env var name charset
// / absolute file path) so a pasted secret is rejected, and (4) returns
// sanitized errors that never echo the caller-supplied ref value (gate C1).
func ParseProviderWrite(name string, body []byte) (providerstore.ProviderRow, error) {
	var zero providerstore.ProviderRow

	// Inline-secret probe BEFORE structured parse — never deserialize a secret.
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(body, &probe); err != nil {
		return zero, fmt.Errorf("invalid provider body: %w", err)
	}
	if _, bad := probe["api_key"]; bad {
		return zero, fmt.Errorf("inline api_key is not allowed; register an api_key_ref (env var name or file path) and set the value in your secret store (§7)")
	}

	var w ProviderWrite
	if err := json.Unmarshal(body, &w); err != nil {
		return zero, fmt.Errorf("invalid provider body: %w", err)
	}
	if strings.TrimSpace(w.Type) == "" {
		return zero, fmt.Errorf("provider type is required")
	}

	row := providerstore.ProviderRow{
		Name: name, Type: w.Type, BaseURL: w.BaseURL, Region: w.Region,
		AuthMode: w.Auth.Mode, AuthProfile: w.Auth.Profile,
	}
	if w.APIKeyRef != nil {
		switch {
		case w.APIKeyRef.Env != "" && w.APIKeyRef.File != "":
			return zero, fmt.Errorf("api_key_ref must set either env or file, not both")
		case w.APIKeyRef.Env != "":
			if !envRefRe.MatchString(w.APIKeyRef.Env) {
				// Do NOT echo the value — it may be a pasted secret.
				return zero, fmt.Errorf("api_key_ref.env must be an environment variable NAME (it is a reference, not the secret value)")
			}
			row.APIKeyRefEnv = w.APIKeyRef.Env
		case w.APIKeyRef.File != "":
			if !strings.HasPrefix(w.APIKeyRef.File, "/") {
				return zero, fmt.Errorf("api_key_ref.file must be an absolute path (it is a reference, not the secret value)")
			}
			row.APIKeyRefFile = w.APIKeyRef.File
		}
	}
	return row, nil
}

// ParseModelWrite validates a model-route write body and returns the ordered
// target chain. A route with no targets, or a target with no provider/model, is
// rejected.
func ParseModelWrite(body []byte) ([]providerstore.Target, error) {
	var w ModelWrite
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, fmt.Errorf("invalid model body: %w", err)
	}
	if len(w.Targets) == 0 {
		return nil, fmt.Errorf("a model route requires at least one target")
	}
	out := make([]providerstore.Target, 0, len(w.Targets))
	for i, t := range w.Targets {
		if strings.TrimSpace(t.Provider) == "" || strings.TrimSpace(t.Model) == "" {
			return nil, fmt.Errorf("target[%d] requires both provider and model", i)
		}
		out = append(out, providerstore.Target{Provider: t.Provider, Model: t.Model, API: t.API})
	}
	return out, nil
}

// Writer is the assembly-provided callback set the write handlers invoke. Each
// method runs the build-once-swap-once mutation under the gateway's reload lock
// (validate the candidate effective topology, persist, swap the validated
// generation). The handlers own only HTTP shape; the assembly owns topology.
type Writer interface {
	WriteProvider(ctx context.Context, row providerstore.ProviderRow) error
	DeleteProvider(ctx context.Context, name string) error
	WriteModel(ctx context.Context, name string, targets []providerstore.Target) error
	DeleteModel(ctx context.Context, name string) error
}
