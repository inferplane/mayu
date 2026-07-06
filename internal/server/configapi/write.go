package configapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/inferplane/inferplane/internal/audit"
	"github.com/inferplane/inferplane/internal/config"
	"github.com/inferplane/inferplane/internal/principal"
	"github.com/inferplane/inferplane/internal/providerstore"
	"github.com/inferplane/inferplane/pkg/ulid"
)

// nameRe bounds a provider/model resource name (path segment): no slashes, no
// surprises — config-bounded so it can safely appear in audit labels.
var nameRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// maxWriteBody caps a write body — provider/model registrations are tiny.
const maxWriteBody = 64 << 10

// secretRefWrite is the auth reference in a write body — a NAME, never a value.
type secretRefWrite struct {
	Env  string `json:"env,omitempty"`
	File string `json:"file,omitempty"`
}

// ProviderWrite is the register/replace DTO. It has NO field that can hold a
// secret value — only the ref (env name / file path) and the bedrock IAM mode.
type ProviderWrite struct {
	Type       string          `json:"type"`
	BaseURL    string          `json:"base_url,omitempty"`
	Region     string          `json:"region,omitempty"`
	Auth       authWrite       `json:"auth,omitempty"`
	APIKeyRef  *secretRefWrite `json:"api_key_ref,omitempty"`
	AuthHeader string          `json:"auth_header,omitempty"`
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
		AuthMode: w.Auth.Mode, AuthProfile: w.Auth.Profile, AuthHeader: w.AuthHeader,
	}
	if w.APIKeyRef != nil {
		// Validate the ref SHAPE through the shared guard (config.ValidateSecretRef
		// — the same check the file→DB seed path runs), so a pasted secret is
		// rejected and the error never echoes the value (ADR-008 gate C1).
		ref := &config.SecretRef{Env: w.APIKeyRef.Env, File: w.APIKeyRef.File}
		if err := config.ValidateSecretRef(ref); err != nil {
			return zero, fmt.Errorf("api_key_ref invalid: %w", err)
		}
		row.APIKeyRefEnv = ref.Env
		row.APIKeyRefFile = ref.File
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
// ErrInvalidTopology (below) maps to 400; providerstore.ErrNotFound maps to 404.
type Writer interface {
	WriteProvider(ctx context.Context, row providerstore.ProviderRow) error
	DeleteProvider(ctx context.Context, name string) error
	WriteModel(ctx context.Context, name string, targets []providerstore.Target) error
	DeleteModel(ctx context.Context, name string) error
}

// ErrInvalidTopology wraps a candidate-build failure (the proposed write would
// not produce a valid topology — unknown type, unresolvable ref, route to a
// missing provider). The assembly returns it; the handler maps it to 400. Its
// message is safe to surface: refs are shape-validated (no secret value) and a
// build error fires before any secret VALUE is produced.
var ErrInvalidTopology = errors.New("invalid topology")

// WriteHandler serves the provider/model write resources. resource is
// "providers" or "models". When w is nil (no provider store configured) every
// write returns 405 — registration stays config-driven (ADR-005, unchanged).
// emit (nil-safe) receives a secret-free admin-action audit record on each
// successful write (§5.5; refs/names only, never a value).
func WriteHandler(resource string, w Writer, emit func(audit.Record)) http.Handler {
	prefix := "/admin/" + resource + "/"
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if w == nil {
			writeErr(rw, http.StatusMethodNotAllowed, "provider store not enabled; registration is config-driven (set provider_store to enable UI writes, ADR-005/008)")
			return
		}
		name := strings.TrimPrefix(r.URL.Path, prefix)
		if name == "" || strings.Contains(name, "/") || !nameRe.MatchString(name) {
			writeErr(rw, http.StatusBadRequest, "invalid resource name")
			return
		}
		isProvider := resource == "providers"
		switch r.Method {
		case http.MethodPut:
			body, err := io.ReadAll(io.LimitReader(r.Body, maxWriteBody))
			if err != nil {
				writeErr(rw, http.StatusBadRequest, "cannot read body")
				return
			}
			if isProvider {
				row, perr := ParseProviderWrite(name, body)
				if perr != nil {
					writeErr(rw, http.StatusBadRequest, perr.Error())
					return
				}
				werr := w.WriteProvider(r.Context(), row)
				if werr == nil {
					emitEvent(emit, r, "provider_registered", name, "")
				}
				mapWriteResult(rw, werr)
				return
			}
			targets, perr := ParseModelWrite(body)
			if perr != nil {
				writeErr(rw, http.StatusBadRequest, perr.Error())
				return
			}
			werr := w.WriteModel(r.Context(), name, targets)
			if werr == nil {
				emitEvent(emit, r, "model_route_updated", "", name)
			}
			mapWriteResult(rw, werr)
		case http.MethodDelete:
			if isProvider {
				werr := w.DeleteProvider(r.Context(), name)
				if werr == nil {
					emitEvent(emit, r, "provider_deleted", name, "")
				}
				mapWriteResult(rw, werr)
				return
			}
			werr := w.DeleteModel(r.Context(), name)
			if werr == nil {
				emitEvent(emit, r, "model_route_deleted", "", name)
			}
			mapWriteResult(rw, werr)
		default:
			writeErr(rw, http.StatusMethodNotAllowed, "use PUT or DELETE")
		}
	})
}

// emitEvent records a secret-free admin-action audit record (§5.5): the event,
// the actor (opaque OIDC subject / break-glass — never PII), and the resource
// NAME (provider or model — config-bounded, never a secret value).
func emitEvent(emit func(audit.Record), r *http.Request, event, provider, model string) {
	if emit == nil {
		return
	}
	rec := audit.Record{
		SchemaVersion: 1,
		Event:         event,
		ID:            ulid.New(),
		TS:            time.Now().UTC().Format(time.RFC3339Nano),
		Request:       audit.RequestRef{Ingress: "admin", Provider: provider, ModelRequested: model},
	}
	if id, ok := principal.AdminFrom(r.Context()); ok {
		sub, method := id.Subject, id.AuthMethod
		rec.Principal = audit.PrincipalRef{User: &sub, AuthMethod: &method}
	}
	emit(rec)
}

// mapWriteResult maps a Writer error to an HTTP status: nil→204, invalid
// topology→400, not-found→404, else→500. The 400 carries a FIXED, sanitized
// message (ADR-008 gate C1 + P4 M1) — the raw build/resolve error is NEVER
// echoed to the client, because it could carry a ref or (after resolution) a
// provider-construction detail; the assembly logs the detail server-side.
func mapWriteResult(rw http.ResponseWriter, err error) {
	switch {
	case err == nil:
		rw.WriteHeader(http.StatusNoContent)
	case errors.Is(err, ErrInvalidTopology):
		writeErr(rw, http.StatusBadRequest, "the provider/model configuration is invalid (check the type, endpoint, routes, and that the api_key_ref resolves); see the gateway logs for detail")
	case errors.Is(err, providerstore.ErrNotFound):
		writeErr(rw, http.StatusNotFound, "not found")
	default:
		writeErr(rw, http.StatusInternalServerError, "write failed")
	}
}

func writeErr(rw http.ResponseWriter, code int, msg string) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(code)
	_ = json.NewEncoder(rw).Encode(map[string]string{"error": msg})
}
