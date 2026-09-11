package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/inferplane/inferplane/internal/adminauth"
	"github.com/inferplane/inferplane/internal/alert"
	"github.com/inferplane/inferplane/internal/audit"
	"github.com/inferplane/inferplane/internal/bodystore"
	"github.com/inferplane/inferplane/internal/filter"
	"github.com/inferplane/inferplane/internal/governance"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/limiter"
	"github.com/inferplane/inferplane/internal/live"
	"github.com/inferplane/inferplane/internal/metrics"
	"github.com/inferplane/inferplane/internal/principal"
	"github.com/inferplane/inferplane/internal/router"
	"github.com/inferplane/inferplane/internal/server/adminapi"
	"github.com/inferplane/inferplane/internal/server/adminui"
	"github.com/inferplane/inferplane/internal/server/analyticsapi"
	"github.com/inferplane/inferplane/internal/server/anthropicapi"
	"github.com/inferplane/inferplane/internal/server/auditapi"
	"github.com/inferplane/inferplane/internal/server/authapi"
	"github.com/inferplane/inferplane/internal/server/bedrockapi"
	"github.com/inferplane/inferplane/internal/server/configapi"
	"github.com/inferplane/inferplane/internal/server/openaiapi"
	"github.com/inferplane/inferplane/internal/server/usageapi"
	"github.com/inferplane/inferplane/internal/telemetry"
	"github.com/inferplane/inferplane/pkg/ulid"
)

// AuthConfigView is the secret-free bootstrap payload the admin console SPA
// reads to decide whether to show the SSO button and, if so, which public
// OAuth2 identifiers to use (ADR-026). It never carries a secret — issuer and
// client_id are the public identifiers of a PKCE public client.
type AuthConfigView struct {
	SSO      bool   `json:"sso"`
	Issuer   string `json:"issuer,omitempty"`
	ClientID string `json:"client_id,omitempty"`
}

// DataMuxOption configures optional DataMux wiring.
type DataMuxOption func(*dataMuxOptions)

type dataMuxOptions struct {
	usage           *telemetry.Collector
	maxRequestBytes int64
	governanceGate  func() (bool, string)
}

// WithGovernanceGate installs the control_plane.require_sync gate (review/
// fable5 §08 B2/B3): every KeyAuth-authenticated, GOVERNED data-plane request
// is refused with 503 + Retry-After while gate() reports not-ready (no policy
// generation received yet, or the last one is older than max_policy_age).
// The two count_tokens routes are exempt — they must never return non-200 —
// and so are /v1/models reads. nil = no gate (the default fail-open posture).
func WithGovernanceGate(gate func() (bool, string)) DataMuxOption {
	return func(o *dataMuxOptions) { o.governanceGate = gate }
}

// WithUsageCollector threads the control-plane usage collector into every
// generation handler's settle path (nil-safe; absent = standalone default).
func WithUsageCollector(c *telemetry.Collector) DataMuxOption {
	return func(o *dataMuxOptions) { o.usage = c }
}

// defaultMaxRequestBytes mirrors internal/config's constant of the same name —
// a small same-valued local copy per package, the repo's established pattern
// (see isLoopbackHost's three copies), because this package deliberately does
// not import internal/config.
const defaultMaxRequestBytes = 64 << 20

// WithMaxRequestBytes bounds every KeyAuth-guarded data-plane request body
// via http.MaxBytesReader (C9). n<=0 selects the default (64 MiB) — the same
// defaulting DataMux applies when this option is omitted entirely, so a
// caller that does not go through config.Load still gets a bound.
func WithMaxRequestBytes(n int64) DataMuxOption {
	return func(o *dataMuxOptions) { o.maxRequestBytes = n }
}

// DataMux builds the data-plane (:8080) handler: Anthropic, Bedrock, and OpenAI
// ingress endpoints behind virtual-key auth (M3). holder provides the live
// configuration snapshot used by Bedrock model-ID resolution. All endpoints
// resolve a Principal via the key store before reaching the router. aud is the
// audit writer (may be nil) used for the two-phase request_started/request_completed
// records on generation endpoints.
// gov is the governance pipeline (rate/quota/budget + cost); when non-nil the
// generation handlers enforce it, when nil governance is bypassed. m is the
// Prometheus metrics sink threaded into the ingress handlers (nil → no-op).
// teamPolicy is a fresh-per-request team-record lookup (D6/D7, ADR-016
// posture — no caching); nil disables per-team overrides entirely. bodies is
// the opt-in body-capture recorder (D4, ADR-018); nil disables it.
//
// cliVerifier + cliMapping + cliConfig wire the opt-in CLI login endpoints
// (ADR-028, `mayu login`): GET /v1/auth/config (unauthenticated
// discovery), POST /v1/auth/key (mints a short-lived virtual key from a
// verified ID token — same OIDC-verify + groups→team middleware as the admin
// plane, keyed to the CLI's own client_id so a console-audience token is
// never accepted here), and DELETE /v1/auth/key (self-revoke, behind KeyAuth
// like any other data-plane route). cliVerifier nil ⇒ none of the three are
// mounted (404) — the feature is off unless oidc.cli_login is configured.
// A trailing DataMuxOption list carries the optional control-plane wiring
// (usage telemetry today) without growing the positional signature — every
// existing call site compiles unchanged.
func DataMux(r *router.Router, holder *live.Holder, store keystore.Store, aud *audit.Writer, gov *governance.Governor, m *metrics.Metrics, mask *filter.Masking, teamPolicy func(team string) (keystore.TeamRecord, bool), bodies *bodystore.Recorder, cliVerifier OIDCVerifier, cliMapping adminauth.MappingConfig, cliConfig func() authapi.ConfigView, cliKeyTTL time.Duration, opts ...DataMuxOption) http.Handler {
	var o dataMuxOptions
	for _, opt := range opts {
		opt(&o)
	}
	limit := o.maxRequestBytes
	if limit <= 0 {
		limit = defaultMaxRequestBytes
	}
	mux := http.NewServeMux()
	msgs := anthropicapi.NewMessagesHandlerMetrics(r, aud, gov, m)
	msgs.SetMasking(mask) // PII masking for configured teams (ADR-009); nil = off
	msgs.SetTeamPolicy(teamPolicy)
	msgs.SetBodyRecorder(bodies)
	msgs.SetUsageCollector(o.usage)
	mux.Handle("POST /v1/messages", msgs)
	ct := anthropicapi.NewCountTokensHandler(r)
	ct.SetMasking(mask)          // mask the count body too (T6); never 500
	ct.SetTeamPolicy(teamPolicy) // region lock (D7, ADR-020): never call an out-of-region TokenCounter
	mux.Handle("POST /v1/messages/count_tokens", ct)
	chat := openaiapi.NewChatHandlerMetrics(r, aud, gov, m)
	chat.SetMasking(mask) // masked teams rejected on the OpenAI ingress (T6b)
	chat.SetTeamPolicy(teamPolicy)
	chat.SetBodyRecorder(bodies)
	chat.SetUsageCollector(o.usage)
	mux.Handle("POST /v1/chat/completions", chat)
	invoke := bedrockapi.NewInvokeHandlerMetrics(r, holder, aud, gov, m, false)
	invoke.SetMasking(mask)
	invoke.SetTeamPolicy(teamPolicy)
	invoke.SetBodyRecorder(bodies)
	invoke.SetUsageCollector(o.usage)
	mux.Handle("POST /model/{modelId}/invoke", invoke)
	stream := bedrockapi.NewInvokeHandlerMetrics(r, holder, aud, gov, m, true)
	stream.SetMasking(mask)
	stream.SetTeamPolicy(teamPolicy)
	stream.SetBodyRecorder(bodies)
	stream.SetUsageCollector(o.usage)
	mux.Handle("POST /model/{modelId}/invoke-with-response-stream", stream)
	bct := bedrockapi.NewCountTokensHandler(r, holder)
	bct.SetMasking(mask)
	bct.SetTeamPolicy(teamPolicy)
	mux.Handle("POST /model/{modelId}/count-tokens", bct)
	// Both the Anthropic (Claude Code) and OpenAI (OpenCode) clients hit the
	// same GET /v1/models path but expect different response shapes, so we
	// content-negotiate: Anthropic clients send an `anthropic-version` header,
	// OpenAI clients do not. (Documented heuristic, M5 §3.2.)
	mux.Handle("GET /v1/models", negotiateModels(
		anthropicapi.NewModelsHandler(r), openaiapi.NewModelsHandler(r)))
	mux.Handle("GET /v1/usage", usageapi.NewHandler(gov))
	var emit func(audit.Record)
	if aud != nil {
		emit = aud.Append
	}
	if cliVerifier != nil {
		// Self-revoke (ADR-028 `mayu logout`) authenticates with the
		// virtual key itself — same posture as every other data-plane route,
		// so it belongs on the INNER mux, behind KeyAuth.
		mux.Handle("DELETE /v1/auth/key", authapi.RevokeHandler(store, emit))
	}
	// Unauthenticated convenience redirect: a browser hitting the bare data-plane
	// root has no API key to offer and almost certainly means to reach the admin
	// console, not the LLM API. Exact path only, ahead of KeyAuth (which would
	// otherwise 401 it) — every other data-plane route (including /v1/*) stays
	// behind KeyAuth unchanged.
	dataMux := http.NewServeMux()
	dataMux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/ui/", http.StatusFound)
	})
	if cliVerifier != nil {
		// GET /v1/auth/config and POST /v1/auth/key authenticate with an IdP
		// ID token, not a virtual key, so both are mounted AHEAD of KeyAuth —
		// same reasoning as the /{$} redirect above, just OIDC instead of
		// unauthenticated. cliDenied audits an authenticated-but-unmapped
		// identity (403), mirroring adminDenialEmitter for the admin plane.
		dataMux.Handle("GET /v1/auth/config", authapi.NewConfigHandler(cliConfig))
		mintLimiter := limiter.NewMemory() // per-subject mint throttle (ADR-028 follow-up r1); instance-local, like every other in-memory governance store
		dataMux.Handle("POST /v1/auth/key", AdminAuth(nil, cliVerifier, cliMapping, cliDenialEmitter(emit), authapi.MintHandler(store, cliKeyTTL, mintLimiter, emit)))
	}
	var governed http.Handler = mux
	if o.governanceGate != nil {
		governed = governanceGateMiddleware(o.governanceGate, mux)
	}
	dataMux.Handle("/", maxBytesMiddleware(limit, KeyAuth(store, governed)))
	return dataMux
}

// governanceRetryAfterSeconds is the Retry-After the gate advertises: the
// syncer's minimum heartbeat cadence (internal/policy.MinPolicySyncInterval,
// 15s) — the soonest the gate can flip to ready. A same-valued local copy,
// like defaultMaxRequestBytes above, because this package does not import
// internal/policy.
const governanceRetryAfterSeconds = "15"

// governanceGateMiddleware refuses governed traffic while the control-plane
// sync gate is not ready. Sits INSIDE KeyAuth (an unauthenticated caller
// learns nothing about gateway state) and exempts count_tokens (never
// non-200) and the /v1/models listing plus any /v1/models/ sub-path
// (read-only, RBAC-filtered, no spend).
// 503 with Retry-After: the condition is transient by construction — the
// syncer keeps retrying with backoff.
func governanceGateMiddleware(gate func() (bool, string), next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if strings.HasSuffix(p, "/count_tokens") || strings.HasSuffix(p, "/count-tokens") || p == "/v1/models" || strings.HasPrefix(p, "/v1/models/") {
			next.ServeHTTP(w, r)
			return
		}
		if ok, reason := gate(); !ok {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", governanceRetryAfterSeconds)
			w.WriteHeader(http.StatusServiceUnavailable)
			body, _ := json.Marshal(map[string]any{
				"type":  "error",
				"error": map[string]string{"type": "api_error", "message": "governance not ready: " + reason},
			})
			_, _ = w.Write(body)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// maxBytesMiddleware bounds every data-plane request body to limit bytes
// BEFORE KeyAuth and before any ingress handler's io.ReadAll (C9 — one
// oversized body must not OOM the gateway). A declared Content-Length over
// the limit is rejected immediately with 413 — cheap, nothing is read. An
// undeclared or understated length is still capped by http.MaxBytesReader,
// whose read error surfaces through the SAME io.ReadAll each generation
// ingress already treats as a malformed body — so this wrap changes no
// handler's error path, only whether/when it fires. The two count_tokens
// handlers ignore the read error entirely and fall back to the local
// estimator, so they stay 200 either way (the never-non-200 invariant).
func maxBytesMiddleware(limit int64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > limit {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_, _ = w.Write([]byte(`{"error":"request body too large"}`))
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		next.ServeHTTP(w, r)
	})
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
func AdminMux(store keystore.Store, adminTokens []string, verifier OIDCVerifier, mapping adminauth.MappingConfig, configView func() configapi.View, auditFileSinks []string, aud *audit.Writer, anchorReader audit.AnchorReader, readyGate func() (bool, string), m *metrics.Metrics, writer configapi.Writer, configExport func() configapi.ExportDoc, capabilities func() configapi.Capabilities, analyticsQ analyticsapi.Querier, teamStore keystore.TeamStore, configTeams func() []keystore.TeamRecord, alertFires func() []alert.Fire, healthSnapshot func() map[string]configapi.HealthRecord, bodiesRec *bodystore.Recorder, authConfig func() *AuthConfigView, connectSrc []string, probeAllowedHosts ...string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	// /readyz reflects the require_sync gate when one is wired (review/fable5
	// §08 B2): a scale-out during a control-plane outage must NOT pass health
	// checks while the new replica is ungoverned. nil gate = always ready.
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if readyGate != nil {
			if ok, reason := readyGate(); !ok {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				body, _ := json.Marshal(map[string]any{"ready": false, "reason": reason})
				_, _ = w.Write(body)
				return
			}
		}
		w.WriteHeader(200)
	})
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
	// Periodic provider health status (ADR-014 deferred item), FULL-ADMIN only
	// (same gating as the on-demand probe, POST /admin/providers/test). nil
	// healthSnapshot → omitted (provider_health_check capability off).
	if healthSnapshot != nil {
		mux.Handle("GET /admin/providers/health", AdminAuth(adminTokens, verifier, mapping, denied,
			requireAdmin(configapi.HealthHandler(healthSnapshot), emit)))
	}
	// UI-write provider/model registration (ADR-008), behind the same AdminAuth
	// AND requireAdmin (S2): a provider write persists a base_url plus an
	// api_key_ref that live traffic will resolve and send — strictly more
	// dangerous than the probe below, which is already full-admin for exactly
	// that reason. Before this gate a team-mapped identity could register an
	// attacker-controlled base_url and exfiltrate a resolved secret ref via
	// ordinary request traffic. writer is nil when no provider store is
	// configured → every write returns 405 (ADR-005 stage-1 posture preserved).
	// Mutations are secret-free (refs only) and run build-once-swap-once in the
	// assembly.
	providersW := AdminAuth(adminTokens, verifier, mapping, denied, requireAdmin(configapi.WriteHandler("providers", writer, emit), emit))
	mux.Handle("/admin/providers/", providersW)
	modelsW := AdminAuth(adminTokens, verifier, mapping, denied, requireAdmin(configapi.WriteHandler("models", writer, emit), emit))
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
	// per-sink hash-chain check, returns no record contents. anchorReader, when
	// non-nil (an anchorer that also implements audit.AnchorReader — s3anchor
	// does), adds the external-anchor cross-check: without it a truncated tail
	// or a whole-file replacement verifies OK (review/fable5 S3).
	auditInstance := ""
	if aud != nil {
		auditInstance = aud.Instance()
	}
	mux.Handle("/admin/audit/verify", AdminAuth(adminTokens, verifier, mapping, denied, auditapi.Handler(auditFileSinks, anchorReader, auditInstance)))
	// Minimal embedded key console (ADR-001): data-free static assets, served
	// unauthenticated like /metrics — every data call it makes goes through the
	// token-gated /admin/keys handlers above.
	if authConfig != nil {
		mux.HandleFunc("GET /admin/auth/config", func(w http.ResponseWriter, r *http.Request) {
			cfg := authConfig()
			if cfg == nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(cfg)
		})
	}
	mux.Handle("/admin/ui/", http.StripPrefix("/admin/ui", adminui.Handler(connectSrc...)))
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

// cliDenialEmitter is adminDenialEmitter's twin for the data-plane CLI-login
// mint endpoint (ADR-028): an authenticated-but-unmapped identity hitting
// POST /v1/auth/key is the same class of event, just on a different ingress —
// tagging it "cli" instead of "admin" is the only difference, so an operator
// reading the audit chain can tell which plane rejected the identity.
func cliDenialEmitter(emit func(audit.Record)) func(r *http.Request, subject string) {
	if emit == nil {
		return nil
	}
	return func(_ *http.Request, subject string) {
		method := "oidc"
		emit(audit.Record{
			SchemaVersion: 1,
			Event:         "cli_denied",
			ID:            ulid.New(),
			TS:            time.Now().UTC().Format(time.RFC3339Nano),
			Principal:     audit.PrincipalRef{User: &subject, AuthMethod: &method},
			Request:       audit.RequestRef{Ingress: "cli"},
		})
	}
}
