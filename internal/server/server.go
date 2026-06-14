package server

import (
	"net/http"
	"time"

	"github.com/inferplane/inferplane/internal/adminauth"
	"github.com/inferplane/inferplane/internal/audit"
	"github.com/inferplane/inferplane/internal/filter"
	"github.com/inferplane/inferplane/internal/governance"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/metrics"
	"github.com/inferplane/inferplane/internal/router"
	"github.com/inferplane/inferplane/internal/server/adminapi"
	"github.com/inferplane/inferplane/internal/server/adminui"
	"github.com/inferplane/inferplane/internal/server/anthropicapi"
	"github.com/inferplane/inferplane/internal/server/auditapi"
	"github.com/inferplane/inferplane/internal/server/configapi"
	"github.com/inferplane/inferplane/internal/server/openaiapi"
	"github.com/inferplane/inferplane/pkg/ulid"
)

// DataMux builds the data-plane (:8080) handler: Anthropic ingress endpoints
// behind virtual-key auth (M3). All endpoints resolve a Principal via the key
// store before reaching the router. aud is the audit writer (may be nil) used
// for the two-phase request_started/request_completed records on /v1/messages.
// gov is the governance pipeline (rate/quota/budget + cost); when non-nil the
// /v1/messages handler enforces it, when nil governance is bypassed. m is the
// Prometheus metrics sink threaded into the ingress handlers (nil → no-op).
func DataMux(r *router.Router, store keystore.Store, aud *audit.Writer, gov *governance.Governor, m *metrics.Metrics, mask *filter.Masking) http.Handler {
	mux := http.NewServeMux()
	msgs := anthropicapi.NewMessagesHandlerMetrics(r, aud, gov, m)
	msgs.SetMasking(mask) // PII masking for configured teams (ADR-009); nil = off
	mux.Handle("POST /v1/messages", msgs)
	ct := anthropicapi.NewCountTokensHandler(r)
	ct.SetMasking(mask) // mask the count body too (T6); never 500
	mux.Handle("POST /v1/messages/count_tokens", ct)
	chat := openaiapi.NewChatHandlerMetrics(r, aud, gov, m)
	chat.SetMasking(mask) // masked teams rejected on the OpenAI ingress (T6b)
	mux.Handle("POST /v1/chat/completions", chat)
	// Both the Anthropic (Claude Code) and OpenAI (OpenCode) clients hit the
	// same GET /v1/models path but expect different response shapes, so we
	// content-negotiate: Anthropic clients send an `anthropic-version` header,
	// OpenAI clients do not. (Documented heuristic, M5 §3.2.)
	mux.Handle("GET /v1/models", negotiateModels(
		anthropicapi.NewModelsHandler(r), openaiapi.NewModelsHandler(r)))
	return KeyAuth(store, mux)
}

// negotiateModels routes GET /v1/models to the Anthropic-shaped handler when the
// request carries an `anthropic-version` header (sent by Claude Code and other
// Anthropic SDKs), and to the OpenAI-shaped handler otherwise (OpenCode / OpenAI
// clients). The two ingress protocols share the path but expect different JSON.
func negotiateModels(anthropicH, openaiH http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("anthropic-version") != "" {
			anthropicH.ServeHTTP(w, req)
			return
		}
		openaiH.ServeHTTP(w, req)
	})
}

// AdminMux builds the admin-plane (:9090) handler: health + /metrics + /admin/keys
// CRUD. /healthz, /readyz, and /metrics are unauthenticated; /admin/keys is guarded
// by AdminAuth — static break-glass tokens always, plus OIDC ID tokens when
// verifier is non-nil (ADR-004; mapping carries the groups→team rules). aud
// receives admin-action audit records (key create/revoke + denials, §5.5
// "admin API calls are audit events"); nil skips. When m is nil the /metrics
// endpoint is omitted.
func AdminMux(store keystore.Store, adminTokens []string, verifier OIDCVerifier, mapping adminauth.MappingConfig, configView func() configapi.View, auditFileSinks []string, aud *audit.Writer, m *metrics.Metrics, writer configapi.Writer, configExport func() configapi.ExportDoc) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	if m != nil {
		mux.Handle("GET /metrics", metricsHandler(m)) // unauthenticated (§5.5)
	}
	var emit func(audit.Record)
	if aud != nil {
		emit = aud.Append
	}
	keys := adminapi.NewKeysHandler(store, emit)
	// Middleware-level denial audit (authenticated 403s only — 401s never grow
	// the chain): the middleware knows no team, so Team stays empty.
	denied := adminDenialEmitter(emit)
	guard := AdminAuth(adminTokens, verifier, mapping, denied, keys)
	mux.Handle("/admin/keys", guard)
	mux.Handle("/admin/keys/", guard)
	// Read-only provider/model topology (ADR-005), behind the same AdminAuth —
	// secret-free, so it carries no governance weight beyond authentication.
	mux.Handle("/admin/config", AdminAuth(adminTokens, verifier, mapping, denied, configapi.Handler(configView)))
	// UI-write provider/model registration (ADR-008), behind the same AdminAuth.
	// writer is nil when no provider store is configured → every write returns
	// 405 (ADR-005 stage-1 posture preserved). Mutations are secret-free (refs
	// only) and run build-once-swap-once in the assembly.
	providersW := AdminAuth(adminTokens, verifier, mapping, denied, configapi.WriteHandler("providers", writer, emit))
	mux.Handle("/admin/providers/", providersW)
	modelsW := AdminAuth(adminTokens, verifier, mapping, denied, configapi.WriteHandler("models", writer, emit))
	mux.Handle("/admin/models/", modelsW)
	// Git export (ADR-008 §3): read-only, secret-free config fragment of the
	// current effective topology, mounted unconditionally (works with or without
	// a provider store). Behind the same AdminAuth.
	if configExport != nil {
		mux.Handle("/admin/config/export", AdminAuth(adminTokens, verifier, mapping, denied, configapi.ExportHandler(configExport)))
	}
	// Audit-chain verification (ADR-003 #2), behind the same AdminAuth: read-only
	// per-sink hash-chain check, returns no record contents.
	mux.Handle("/admin/audit/verify", AdminAuth(adminTokens, verifier, mapping, denied, auditapi.Handler(auditFileSinks)))
	// Minimal embedded key console (ADR-001): data-free static assets, served
	// unauthenticated like /metrics — every data call it makes goes through the
	// token-gated /admin/keys handlers above.
	mux.Handle("/admin/ui/", http.StripPrefix("/admin/ui", adminui.Handler()))
	mux.Handle("/admin/ui", http.RedirectHandler("/admin/ui/", http.StatusMovedPermanently))
	return mux
}

// adminDenialEmitter adapts the audit emit func to the AdminAuth denial hook:
// an authenticated identity that maps to no team is a governance event
// (admin_denied) even though it never reaches a handler (P2 gate r3).
func adminDenialEmitter(emit func(audit.Record)) func(r *http.Request, subject string) {
	if emit == nil {
		return nil
	}
	return func(_ *http.Request, subject string) {
		method := "oidc" // middleware denials only occur on the OIDC path
		emit(audit.Record{
			SchemaVersion: 1,
			Event:         "admin_denied",
			ID:            ulid.New(),
			TS:            time.Now().UTC().Format(time.RFC3339Nano),
			Principal:     audit.PrincipalRef{User: &subject, AuthMethod: &method},
			Request:       audit.RequestRef{Ingress: "admin"},
		})
	}
}
