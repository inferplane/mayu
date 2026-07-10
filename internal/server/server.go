package server

import (
	"net/http"
	"time"

	"github.com/inferplane/inferplane/internal/adminauth"
	"github.com/inferplane/inferplane/internal/alert"
	"github.com/inferplane/inferplane/internal/audit"
	"github.com/inferplane/inferplane/internal/bodystore"
	"github.com/inferplane/inferplane/internal/filter"
	"github.com/inferplane/inferplane/internal/governance"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/metrics"
	"github.com/inferplane/inferplane/internal/principal"
	"github.com/inferplane/inferplane/internal/router"
	"github.com/inferplane/inferplane/internal/server/adminapi"
	"github.com/inferplane/inferplane/internal/server/adminui"
	"github.com/inferplane/inferplane/internal/server/analyticsapi"
	"github.com/inferplane/inferplane/internal/server/anthropicapi"
	"github.com/inferplane/inferplane/internal/server/auditapi"
	"github.com/inferplane/inferplane/internal/server/configapi"
	"github.com/inferplane/inferplane/internal/server/openaiapi"
	"github.com/inferplane/inferplane/internal/server/usageapi"
	"github.com/inferplane/inferplane/pkg/ulid"
)

// DataMux builds the data-plane (:8080) handler: Anthropic ingress endpoints
// behind virtual-key auth (M3). All endpoints resolve a Principal via the key
// store before reaching the router. aud is the audit writer (may be nil) used
// for the two-phase request_started/request_completed records on /v1/messages.
// gov is the governance pipeline (rate/quota/budget + cost); when non-nil the
// /v1/messages handler enforces it, when nil governance is bypassed. m is the
// Prometheus metrics sink threaded into the ingress handlers (nil → no-op).
// teamPolicy is a fresh-per-request team-record lookup (D6/D7, ADR-016
// posture — no caching); nil disables per-team overrides entirely. bodies is
// the opt-in body-capture recorder (D4, ADR-018); nil disables it.
func DataMux(r *router.Router, store keystore.Store, aud *audit.Writer, gov *governance.Governor, m *metrics.Metrics, mask *filter.Masking, teamPolicy func(team string) (keystore.TeamRecord, bool), bodies *bodystore.Recorder) http.Handler {
	mux := http.NewServeMux()
	msgs := anthropicapi.NewMessagesHandlerMetrics(r, aud, gov, m)
	msgs.SetMasking(mask) // PII masking for configured teams (ADR-009); nil = off
	msgs.SetTeamPolicy(teamPolicy)
	msgs.SetBodyRecorder(bodies)
	mux.Handle("POST /v1/messages", msgs)
	ct := anthropicapi.NewCountTokensHandler(r)
	ct.SetMasking(mask)          // mask the count body too (T6); never 500
	ct.SetTeamPolicy(teamPolicy) // region lock (D7, ADR-020): never call an out-of-region TokenCounter
	mux.Handle("POST /v1/messages/count_tokens", ct)
	chat := openaiapi.NewChatHandlerMetrics(r, aud, gov, m)
	chat.SetMasking(mask) // masked teams rejected on the OpenAI ingress (T6b)
	chat.SetTeamPolicy(teamPolicy)
	chat.SetBodyRecorder(bodies)
	mux.Handle("POST /v1/chat/completions", chat)
	// Both the Anthropic (Claude Code) and OpenAI (OpenCode) clients hit the
	// same GET /v1/models path but expect different response shapes, so we
	// content-negotiate: Anthropic clients send an `anthropic-version` header,
	// OpenAI clients do not. (Documented heuristic, M5 §3.2.)
	mux.Handle("GET /v1/models", negotiateModels(
		anthropicapi.NewModelsHandler(r), openaiapi.NewModelsHandler(r)))
	mux.Handle("GET /v1/usage", usageapi.NewHandler(gov))
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
func AdminMux(store keystore.Store, adminTokens []string, verifier OIDCVerifier, mapping adminauth.MappingConfig, configView func() configapi.View, auditFileSinks []string, aud *audit.Writer, m *metrics.Metrics, writer configapi.Writer, configExport func() configapi.ExportDoc, capabilities func() configapi.Capabilities, analyticsQ analyticsapi.Querier, teamStore keystore.TeamStore, configTeams func() []keystore.TeamRecord, alertFires func() []alert.Fire, bodiesRec *bodystore.Recorder, probeAllowedHosts ...string) http.Handler {
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
	// Self-service identity (ADR-010): the caller's resolved identity (opaque
	// subject + entitled teams + flags), secret-free, behind the same AdminAuth.
	// Lets the console offer self-service key issuance scoped to the user's teams.
	mux.Handle("/admin/whoami", AdminAuth(adminTokens, verifier, mapping, denied, adminapi.WhoamiHandler()))
	// Read-only provider/model topology (ADR-005), behind the same AdminAuth —
	// secret-free, so it carries no governance weight beyond authentication.
	mux.Handle("/admin/config", AdminAuth(adminTokens, verifier, mapping, denied, configapi.Handler(configView)))
	// Capability map (spec §4.4), behind the same AdminAuth — secret-free
	// booleans/enums the console reads on bootstrap to render each section's
	// enabled/disabled affordance (degradation contract §9.1). nil → omitted.
	if capabilities != nil {
		mux.Handle("/admin/capabilities", AdminAuth(adminTokens, verifier, mapping, denied, configapi.CapabilitiesHandler(capabilities)))
	}
	// Analytics read API (spec §4 / D1). FULL-ADMIN only in Phase 1a (team-scoped
	// views await team records, D3) — same requireAdmin gate as the probe. nil →
	// omitted (analytics index disabled).
	if analyticsQ != nil {
		mux.Handle("GET /admin/analytics/summary", AdminAuth(adminTokens, verifier, mapping, denied,
			requireAdmin(analyticsapi.SummaryHandler(analyticsQ), emit)))
		mux.Handle("GET /admin/analytics/timeseries", AdminAuth(adminTokens, verifier, mapping, denied,
			requireAdmin(analyticsapi.TimeSeriesHandler(analyticsQ), emit)))
		mux.Handle("GET /admin/analytics/health", AdminAuth(adminTokens, verifier, mapping, denied,
			requireAdmin(analyticsapi.HealthHandler(analyticsQ), emit)))
		mux.Handle("POST /admin/analytics/rebuild", AdminAuth(adminTokens, verifier, mapping, denied,
			requireAdmin(analyticsapi.RebuildHandler(analyticsQ), emit)))
		// Logs list (D4, ADR-018): recent request events, id-keyset paginated.
		// Metadata only — bodies are fetched separately via /admin/bodies/{ref},
		// gated on the bodiesRec dependency below (may be off even when
		// analyticsQ is on: logs metadata does not require body capture).
		mux.Handle("GET /admin/logs", AdminAuth(adminTokens, verifier, mapping, denied,
			requireAdmin(analyticsapi.LogsHandler(analyticsQ), emit)))
	}
	// Body fetch/erase (D4, ADR-018): full-admin only — resolves a decrypted
	// captured body server-side. nil bodiesRec omits the mount (log_bodies off).
	if bodiesRec != nil {
		bodiesH := adminapi.NewBodiesHandler(bodiesRec, emit)
		mux.Handle("/admin/bodies/", AdminAuth(adminTokens, verifier, mapping, denied, requireAdmin(bodiesH, emit)))
	}
	// Team governance records (D3, ADR-016): teams as first-class keystore rows.
	// Reads are available to any AdminAuth identity; writes are full-admin only
	// (requireAdmin) — a team-mapped identity must not raise its own team's
	// budget. nil teamStore omits the mount (same optional-dependency shape as
	// analyticsQ above), though in practice the keystore always supports
	// TeamStore once wired by the assembly. Users are a derived read-only
	// projection of key owners (no users table) — any AdminAuth identity may
	// read /admin/users.
	if teamStore != nil {
		teamsH := adminapi.NewTeamsHandler(teamStore, configTeams, emit)
		mux.Handle("GET /admin/teams", AdminAuth(adminTokens, verifier, mapping, denied, teamsH))
		mux.Handle("PUT /admin/teams/", AdminAuth(adminTokens, verifier, mapping, denied, requireAdmin(teamsH, emit)))
		mux.Handle("DELETE /admin/teams/", AdminAuth(adminTokens, verifier, mapping, denied, requireAdmin(teamsH, emit)))
		mux.Handle("GET /admin/users", AdminAuth(adminTokens, verifier, mapping, denied, adminapi.NewUsersHandler(store)))
	}
	// Budget-alert recent-fires ring (D5b, ADR-017), FULL-ADMIN only — a fire
	// carries cross-team spend figures, same posture as the analytics summary
	// endpoints. nil alertFires → omitted (budget_alerts capability off).
	if alertFires != nil {
		mux.Handle("GET /admin/alerts/recent", AdminAuth(adminTokens, verifier, mapping, denied,
			requireAdmin(adminapi.AlertsHandler(alertFires), emit)))
	}
	// UI-write provider/model registration (ADR-008), behind the same AdminAuth.
	// writer is nil when no provider store is configured → every write returns
	// 405 (ADR-005 stage-1 posture preserved). Mutations are secret-free (refs
	// only) and run build-once-swap-once in the assembly.
	providersW := AdminAuth(adminTokens, verifier, mapping, denied, configapi.WriteHandler("providers", writer, emit))
	mux.Handle("/admin/providers/", providersW)
	modelsW := AdminAuth(adminTokens, verifier, mapping, denied, configapi.WriteHandler("models", writer, emit))
	mux.Handle("/admin/models/", modelsW)
	// Connection probe (ADR-014 D2): tests a DRAFT provider's upstream before a
	// route is trusted. FULL-ADMIN ONLY — it resolves a secret ref to an
	// operator-supplied host, so the team-mapped provider-write tier must not
	// reach it (requireAdmin). storeEnabled mirrors the write path (405 when no
	// provider store). The exact POST route is more specific than the
	// /admin/providers/ prefix, so it wins for POST.
	probeH := AdminAuth(adminTokens, verifier, mapping, denied,
		requireAdmin(configapi.ProbeHandler(writer != nil, probeAllowedHosts), emit))
	mux.Handle("POST /admin/providers/test", probeH)
	// Model catalog (ADR-014 D3): read-only typeahead hints, behind AdminAuth.
	catalogH := AdminAuth(adminTokens, verifier, mapping, denied, configapi.CatalogHandler())
	mux.Handle("GET /admin/providers/catalog", catalogH)
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

// requireAdmin wraps a handler so only a FULL admin identity reaches it (ADR-014
// D2). It runs INSIDE AdminAuth (the identity is already in context): a
// team-mapped, non-admin OIDC identity is admitted by AdminAuth but rejected
// here with 403. Fails closed if no identity is present.
func requireAdmin(next http.Handler, emit func(audit.Record)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := principal.AdminFrom(r.Context())
		if !ok || !id.IsAdmin {
			if ok && emit != nil {
				sub, method := id.Subject, id.AuthMethod
				emit(audit.Record{
					SchemaVersion: 1,
					Event:         "admin_denied",
					ID:            ulid.New(),
					TS:            time.Now().UTC().Format(time.RFC3339Nano),
					Principal:     audit.PrincipalRef{User: &sub, AuthMethod: &method},
					Request:       audit.RequestRef{Ingress: "admin"},
				})
			}
			http.Error(w, `{"error":"admin only"}`, http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
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
