package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/inferplane/inferplane/internal/adminauth"
	"github.com/inferplane/inferplane/internal/alert"
	"github.com/inferplane/inferplane/internal/analytics"
	"github.com/inferplane/inferplane/internal/analytics/pgstore"
	"github.com/inferplane/inferplane/internal/audit"
	"github.com/inferplane/inferplane/internal/audit/s3anchor"
	"github.com/inferplane/inferplane/internal/bodystore"
	"github.com/inferplane/inferplane/internal/budget"
	"github.com/inferplane/inferplane/internal/config"
	"github.com/inferplane/inferplane/internal/filter"
	"github.com/inferplane/inferplane/internal/governance"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/limiter"
	"github.com/inferplane/inferplane/internal/live"
	"github.com/inferplane/inferplane/internal/metrics"
	"github.com/inferplane/inferplane/internal/providerstore"
	"github.com/inferplane/inferplane/internal/router"
	"github.com/inferplane/inferplane/internal/server"
	"github.com/inferplane/inferplane/internal/server/analyticsapi"
	"github.com/inferplane/inferplane/internal/server/configapi"
	"github.com/inferplane/inferplane/internal/tracing"
	"github.com/inferplane/inferplane/pkg/ulid"
)

// gateway is the fully wired serve assembly with its listeners already bound.
// Binding in newGateway (rather than inside serve) makes ":0" configs testable:
// the OS-chosen ports are discoverable via DataAddr/AdminAddr before traffic.
type gateway struct {
	cfgPath       string
	cfg           *config.Config
	store         keystore.Store
	aud           *audit.Writer
	holder        *live.Holder
	router        *router.Router
	pstore        providerstore.Store         // nil unless provider_store is configured (ADR-008)
	pgstoreQ      *pgstore.Store              // nil unless analytics.mode_b is configured (ADR-015)
	pgstoreAgg    *pgstore.Aggregator         // nil unless Mode B aggregation is configured
	analyticsIdx  *analytics.Index            // nil unless the analytics index is enabled (spec §4 / D1)
	analyticsSink audit.Sink                  // async ingestion sink; drained+closed before analyticsIdx on shutdown
	otelDown      func(context.Context) error // OTel TracerProvider shutdown (nil unless otel configured, ADR-011)
	instance      string                      // audit chain instance id (matches the audit Writer)
	anchorer      audit.Anchorer              // nil unless audit.anchor is configured (ADR-012)
	anchorEvery   time.Duration               // anchor interval
	anchorLast    atomic.Int64                // highest record count successfully anchored (shared: worker + finalAnchor)
	metrics       *metrics.Metrics            // for the anchor-failure counter
	bodyStore     bodystore.Store             // nil unless audit.log_bodies is configured (D4, ADR-018)
	bodyRec       *bodystore.Recorder         // nil unless audit.log_bodies is configured
	bodyMaxBytes  int64                       // total body-store size cap, for the purge worker
	notifier      *alert.Notifier             // nil unless budget_alerts is configured (D5b, ADR-017)
	reloadMu      sync.Mutex                  // serializes reloads AND UI writes (concurrent SIGHUPs/triggers)
	dataLn        net.Listener
	adminLn       net.Listener
	dataSrv       *http.Server
	adminSrv      *http.Server
}

// newGateway loads config and assembles the full serve wiring — metrics,
// keystore, audit writer, providers, router, governor, both muxes — then binds
// the data and admin listeners. On error every partially opened resource is
// closed. The caller must call serve (which owns shutdown + resource closing).
func newGateway(cfgPath string) (*gateway, error) {
	// Parse the file WITHOUT resolving provider secrets (ADR-008 gate G1): when a
	// provider store is authoritative, file providers may be stale/ignored, so
	// resolving their refs here would crash boot before the overlay discards them.
	raw, err := config.LoadRaw(cfgPath)
	if err != nil {
		return nil, err
	}

	// Prometheus metrics sink: owned by main, threaded into the audit writer,
	// router, governor, and ingress handlers, and exposed on the admin /metrics.
	m := metrics.New()

	// Virtual-key store: KeyAuth resolves client keys against it (§5.1).
	store, err := keystore.OpenSQLite(raw.KeyStore.Path)
	if err != nil {
		return nil, fmt.Errorf("keystore: %w", err)
	}

	// Audit writer: build sinks from config, then the single-writer chain.
	sinks, err := buildSinks(raw.Audit.Sinks)
	if err != nil {
		store.Close()
		return nil, fmt.Errorf("audit sinks: %w", err)
	}
	// Analytics index (Mode A, spec §4): a DERIVED read-model fed as a best-effort
	// audit Sink, so it must join `sinks` BEFORE NewWriter fans out to them. At
	// boot it replays existing file-sink history (idempotent by ULID). Owned by
	// the gateway; closed on Stop. On a boot-error path the process exits and the
	// OS reclaims the handle, so we don't unwind it there.
	var analyticsIdx *analytics.Index
	var analyticsSink audit.Sink
	// When Mode B is configured, Mode A's local SQLite index is not wired up
	// at all (ADR-015 §1) — queries and ingestion both go to the shared
	// Postgres store instead, so opening a local index nobody reads/feeds
	// would just be wasted disk I/O and extra shutdown surface.
	if apath, on := config.ResolveAnalytics(raw); on && raw.Analytics.ModeB == nil {
		ix, aerr := analytics.OpenSQLite(apath)
		if aerr != nil {
			fmt.Fprintln(os.Stderr, "inferplane: analytics index disabled (open failed):", aerr)
		} else {
			analyticsIdx = ix
			for _, sk := range raw.Audit.Sinks {
				if sk.Type == "file" && sk.Path != "" {
					if f, e := os.Open(sk.Path); e == nil {
						if _, e := analyticsIdx.Replay(f); e != nil {
							fmt.Fprintln(os.Stderr, "inferplane: analytics replay warning:", e)
						}
						f.Close()
					}
				}
			}
			analyticsSink = analytics.NewSink(analyticsIdx) // async worker; closed on shutdown
			sinks = append(sinks, analyticsSink)
			fmt.Printf("inferplane: analytics index enabled → %s\n", apath)
		}
	}
	inst := instanceID()
	aud, err := audit.NewWriter(inst, raw.Audit.Buffer.Path, sinks)
	if err != nil {
		store.Close()
		return nil, fmt.Errorf("audit: %w", err)
	}
	aud.SetMetrics(m) // audit_write_failures / buffer_utilization

	// Optional DB-authoritative provider/model store (ADR-008). Absent → file is
	// authoritative and UI writes 405 (ADR-005, unchanged). Present → seed once
	// from the file (durable marker) so a file-config deployment loses nothing,
	// then the DB is authoritative for the reloadable topology.
	var pstore providerstore.Store
	if raw.ProviderStore != nil {
		// Only "sqlite" is implemented; reject typos / unimplemented backends
		// (e.g. "postgres") rather than silently using SQLite (P4 MINOR).
		if t := raw.ProviderStore.Type; t != "" && t != "sqlite" {
			store.Close()
			aud.Close()
			return nil, fmt.Errorf("provider store: unsupported type %q (only \"sqlite\" is implemented)", t)
		}
		pstore, err = providerstore.OpenSQLite(raw.ProviderStore.Path)
		if err != nil {
			store.Close()
			aud.Close()
			return nil, fmt.Errorf("provider store: %w", err)
		}
		if err := providerstore.SeedIfEmpty(context.Background(), pstore, raw); err != nil {
			pstore.Close()
			store.Close()
			aud.Close()
			return nil, fmt.Errorf("provider store seed: %w", err)
		}
	}

	// Effective config = raw file config, with providers/models overlaid from the
	// DB (when a store is authoritative) and only the EFFECTIVE providers' secret
	// refs resolved (ADR-008 gate G1). Without a store this is exactly config.Load.
	cfg, err := buildEffective(raw, pstore)
	if err != nil {
		closeAll(pstore, nil, store, aud)
		return nil, err
	}

	var pgstoreQ *pgstore.Store
	var pgstoreAgg *pgstore.Aggregator
	if cfg.Analytics.ModeB != nil {
		aggCfg, err := modeBAggregatorConfig(cfg.Analytics.ModeB)
		if err != nil {
			closeAll(pstore, nil, store, aud)
			return nil, err
		}
		pgstoreQ, err = pgstore.New(context.Background(), cfg.Analytics.ModeB.DSN)
		if err != nil {
			closeAll(pstore, nil, store, aud)
			return nil, fmt.Errorf("analytics mode_b postgres store: %w", err)
		}
		pgstoreAgg = pgstore.NewAggregator(pgstoreQ, aggCfg)
	}

	// Topology generation (providers + routes + pricing) — built by the
	// topology-only builder so the same path serves boot and hot reload
	// (ADR-006). Published behind an atomic holder the router reads.
	st, _, err := live.BuildState(cfg)
	if err != nil {
		closeAll(pstore, pgstoreQ, store, aud)
		return nil, err
	}
	holder := &live.Holder{}
	holder.Swap(st)
	r := router.New(holder)
	r.SetMetrics(m) // circuit_state

	// Governance pipeline: map config → governance/pricing config shapes (which
	// are decoupled from internal/config to avoid an import cycle), then build
	// the Governor so /v1/messages enforces rate/quota/budget + records cost.
	teamCfg := map[string]governance.ConfigTeam{}
	for name, tc := range cfg.Teams {
		teamCfg[name] = governance.ConfigTeam{
			RatePerMin:        tc.RateLimit.RequestsPerMinute,
			TokensPerMinute:   tc.RateLimit.TokensPerMinute,
			TokensPerDay:      tc.Quota.TokensPerDay,
			QuotaExceeded:     tc.Quota.OnExceeded,
			BudgetUSDPerMonth: tc.Budget.USDPerMonth,
			BudgetExceeded:    tc.Budget.OnExceeded,
		}
	}
	policies := governance.PoliciesFromConfig(teamCfg)
	// The pricing table lives in the live.State (built by live.BuildState) and
	// is passed into Settle per request from the resolved snapshot, so the
	// governor holds no pricing — only its persistent rate/budget counters.
	gov := governance.NewGovernor(policies, limiter.NewMemory(), budget.NewMemory(), m) // budget_spend / pricing_miss
	// D3 (ADR-016): a team record in the keystore takes precedence over the
	// config map above, and is checked fresh on every request — no restart, no
	// hot-reload needed for an admin-console budget/limit edit to take effect.
	// A lookup ERROR (not a miss) falls back to the config map rather than
	// blocking all traffic on a transient keystore error; store already does
	// one SQLite read per request on this exact hot path (Resolve), so this
	// adds one more point read, not a new failure class.
	gov.SetTeamLookup(func(team string) (governance.TeamPolicy, bool) {
		rec, ok, err := store.GetTeam(context.Background(), team)
		if err != nil {
			fmt.Fprintln(os.Stderr, "inferplane: team lookup:", err)
			return governance.TeamPolicy{}, false
		}
		if !ok {
			return governance.TeamPolicy{}, false
		}
		return governance.PolicyFromLimits(rec.RPM, rec.TPM, rec.TokensPerDay,
			rec.QuotaOnExceeded, rec.BudgetUSDMicros, rec.BudgetOnExceeded), true
	})
	// Per-team record lookup for NON-governance overrides (D6/ADR-019's
	// guardrail override; D7/ADR-020's region-lock reuses this same closure).
	// Same fresh-per-request posture as SetTeamLookup above; a lookup error
	// falls back to "no override" rather than blocking traffic. A DB record,
	// when present, wins WHOLESALE over config (ADR-016 priority) — but a
	// config-only team (no DB record) still gets its config-declared region
	// policy applied, since keystore.TeamRecord has no other source for it.
	teamPolicy := func(team string) (keystore.TeamRecord, bool) {
		rec, ok, err := store.GetTeam(context.Background(), team)
		if err != nil {
			fmt.Fprintln(os.Stderr, "inferplane: team policy lookup:", err)
			return keystore.TeamRecord{}, false
		}
		if ok {
			return rec, true
		}
		if tc, ok := cfg.Teams[team]; ok && len(tc.AllowedRegions) > 0 {
			return keystore.TeamRecord{AllowedRegions: tc.AllowedRegions}, true
		}
		return keystore.TeamRecord{}, false
	}

	// Budget-alert webhook (D5b, ADR-017): opt-in, off unless configured. The
	// webhook URL is never logged (it may embed a Slack/SNS capability token).
	var notifier *alert.Notifier
	if ba := cfg.BudgetAlerts; ba != nil {
		var timeout time.Duration
		if ba.Timeout != "" {
			timeout, _ = time.ParseDuration(ba.Timeout) // shape already validated at config load
		}
		notifier = alert.New(ba.WebhookURL, ba.Thresholds, timeout)
		gov.SetBudgetNotify(notifier.Observe)
		fmt.Println("inferplane: budget alerts enabled")
	}

	// Optional self-TLS for the data plane (design §2.3): non-K8s single-binary
	// deployments can terminate their own TLS; K8s terminates at ingress/mesh.
	// The pair must be fully specified or fully empty.
	if err := server.ValidateTLS(cfg.Server.TLS.CertFile, cfg.Server.TLS.KeyFile); err != nil {
		closeAll(pstore, pgstoreQ, store, aud)
		return nil, err
	}

	dataLn, err := net.Listen("tcp", cfg.Server.Listen)
	if err != nil {
		closeAll(pstore, pgstoreQ, store, aud)
		return nil, fmt.Errorf("data listen %q: %w", cfg.Server.Listen, err)
	}
	adminLn, err := net.Listen("tcp", cfg.Server.AdminListen)
	if err != nil {
		dataLn.Close()
		closeAll(pstore, pgstoreQ, store, aud)
		return nil, fmt.Errorf("admin listen %q: %w", cfg.Server.AdminListen, err)
	}

	// File audit-sink paths for the /admin/audit/verify endpoint (ADR-003 #2).
	var auditFileSinks []string
	for _, sk := range cfg.Audit.Sinks {
		if sk.Type == "file" {
			auditFileSinks = append(auditFileSinks, sk.Path)
		}
	}

	// Request-filter plugins (ADR-009): resolve the configured masking filter from
	// the registry (populated by blank imports). An unknown plugin name fails the
	// boot; enabling masking logs the cache/cost warning.
	masking, err := buildMasking(cfg)
	if err != nil {
		closeAll(pstore, pgstoreQ, store, aud)
		return nil, err
	}

	// Opt-in request/response body capture (D4, ADR-018). Boot-FATAL on open
	// failure — an operator who explicitly configured audit.log_bodies must
	// not silently run without it (unlike the analytics index, which degrades
	// to disabled on open failure: body logging is a compliance feature the
	// operator opted into on purpose).
	var bodyStore bodystore.Store
	var bodyRec *bodystore.Recorder
	var bodyMaxBytes int64
	if bl := cfg.Audit.LogBodies; bl != nil {
		masterKey, kerr := bodystore.ParseMasterKey(bl.Key)
		if kerr != nil {
			closeAll(pstore, pgstoreQ, store, aud)
			return nil, fmt.Errorf("body store: %w", kerr)
		}
		ttl, _ := time.ParseDuration(bl.TTL) // shape already validated at config load
		if bl.Type == "postgres" {
			pgBody, perr := bodystore.NewPostgres(context.Background(), bl.DSN)
			if perr != nil {
				closeAll(pstore, pgstoreQ, store, aud)
				return nil, fmt.Errorf("body store: %w", perr)
			}
			bodyStore = pgBody
		} else {
			sqliteBody, serr := bodystore.OpenSQLite(config.ResolveBodyPath(cfg))
			if serr != nil {
				closeAll(pstore, pgstoreQ, store, aud)
				return nil, fmt.Errorf("body store: %w", serr)
			}
			bodyStore = sqliteBody
		}
		bodyRec = bodystore.NewRecorder(bodyStore, masterKey, ttl, bl.MaxBodyBytes)
		bodyMaxBytes = bl.MaxBytes
		fmt.Println("inferplane: body logging enabled")
	}

	// OpenTelemetry tracing (ADR-011): opt-in. Init failure is NON-FATAL — tracing
	// is best-effort observability and must never block boot; the exporter
	// connects lazily so an unreachable collector at boot is fine.
	var otelDown func(context.Context) error
	if cfg.OTel != nil {
		sd, terr := tracing.Init(context.Background(), tracingConfig(cfg.OTel))
		if terr != nil {
			fmt.Fprintln(os.Stderr, "inferplane: otel init failed (tracing disabled):", terr)
		} else {
			otelDown = sd
			fmt.Println("inferplane: otel tracing enabled →", cfg.OTel.Endpoint)
		}
	}

	// Audit anchoring (ADR-012): opt-in. Init failure is NON-FATAL (best-effort
	// forensics, never blocks boot). Worker started in serve.
	var anchorer audit.Anchorer
	anchorEvery := 5 * time.Minute
	if a := raw.Audit.Anchor; a != nil {
		if a.Interval != "" {
			if d, derr := time.ParseDuration(a.Interval); derr == nil {
				anchorEvery = d
			}
		}
		an, aerr := s3anchor.New(context.Background(), s3anchor.Config{
			Bucket: a.Bucket, Prefix: a.Prefix, Region: a.Region, Endpoint: a.Endpoint, RetainDays: a.RetainDays,
		})
		if aerr != nil {
			fmt.Fprintln(os.Stderr, "inferplane: audit anchor init failed (anchoring disabled):", aerr)
		} else {
			anchorer = an
			fmt.Printf("inferplane: audit anchoring enabled → s3://%s/%s every %s\n", a.Bucket, a.Prefix, anchorEvery)
		}
	}

	g := &gateway{
		cfgPath:       cfgPath,
		cfg:           cfg,
		store:         store,
		aud:           aud,
		holder:        holder,
		router:        r,
		pstore:        pstore,
		pgstoreQ:      pgstoreQ,
		pgstoreAgg:    pgstoreAgg,
		analyticsIdx:  analyticsIdx,
		analyticsSink: analyticsSink,
		otelDown:      otelDown,
		notifier:      notifier,
		instance:      inst,
		anchorer:      anchorer,
		anchorEvery:   anchorEvery,
		metrics:       m,
		bodyStore:     bodyStore,
		bodyRec:       bodyRec,
		bodyMaxBytes:  bodyMaxBytes,
		dataLn:        dataLn,
		adminLn:       adminLn,
	}
	// The gateway implements configapi.Writer (build-once-swap-once). Pass it as
	// the write callback ONLY when a store is configured; nil → writes 405.
	var writer configapi.Writer
	if pstore != nil {
		writer = g
	}
	g.dataSrv = &http.Server{Handler: server.DataMux(r, store, aud, gov, m, masking, teamPolicy, bodyRec)}
	// Capability map the console reads on bootstrap (spec §4.4). Phase 0a:
	// analytics index not built yet; provider_store + guardrails reflect what
	// this assembly already knows. Later phases flip the rest on as they land.
	capabilities := func() configapi.Capabilities {
		ai := "off"
		if pgstoreQ != nil {
			ai = "B" // Mode B (shared Postgres store).
		} else if analyticsIdx != nil {
			ai = "A" // Mode A (local single-replica).
		}
		return configapi.Capabilities{
			AnalyticsIndex:      ai,
			ProviderStore:       writer != nil,
			Guardrails:          masking != nil,
			KeyGovernanceFields: true, // keystore always has budget/TPM/RPM/expiry/owner (Phase 2)
			TeamsRecords:        true, // keystore always supports TeamStore (D3, ADR-016)
			RegionPolicy:        true, // enforcement path always present (D7, ADR-020) — same rationale as TeamsRecords
			LogsBodies:          bodyRec != nil,
			BudgetAlerts:        notifier != nil,
		}
	}
	var alertFires func() []alert.Fire
	if notifier != nil {
		alertFires = notifier.Recent
	}
	var analyticsQ analyticsapi.Querier
	if pgstoreQ != nil {
		analyticsQ = pgstoreQ
	} else if analyticsIdx != nil {
		analyticsQ = analyticsIdx
	}
	// configTeams surfaces config-declared teams, carrying allowed_regions for
	// pre-fill, so /admin/teams can show them as "source":"config" until/unless
	// a DB record for the same name takes precedence (ADR-016).
	configTeams := func() []keystore.TeamRecord {
		recs := make([]keystore.TeamRecord, 0, len(cfg.Teams))
		for name, tc := range cfg.Teams {
			recs = append(recs, keystore.TeamRecord{Name: name, AllowedRegions: tc.AllowedRegions})
		}
		return recs
	}
	g.adminSrv = &http.Server{Handler: server.AdminMux(store, cfg.Server.AdminAuth.Tokens, oidcVerifier(cfg), oidcMapping(cfg), liveView(holder, pstore != nil), auditFileSinks, aud, m, writer, liveExport(holder), capabilities, analyticsQ, store, configTeams, alertFires, bodyRec, cfg.Probe.AllowedHosts...)}
	return g, nil
}

// buildEffective resolves the effective config from the raw file config: with a
// provider store it overlays the DB topology and resolves only those refs;
// without one it resolves the file providers (== config.Load). Shared by boot
// and reload so the topology source is consistent (ADR-008).
func buildEffective(raw *config.Config, pstore providerstore.Store) (*config.Config, error) {
	if pstore == nil {
		if err := config.ResolveProviders(raw); err != nil {
			return nil, err
		}
		return raw, nil
	}
	eff, err := providerstore.Overlay(raw, pstore)
	if err != nil {
		return nil, err
	}
	if err := config.ResolveProviders(eff); err != nil {
		return nil, err
	}
	return eff, nil
}

func modeBAggregatorConfig(mb *config.AnalyticsModeB) (pgstore.AggregatorConfig, error) {
	cfg := pgstore.AggregatorConfig{AggregatedAuditDir: mb.AggregatedAuditDir}
	if mb.PollInterval != "" {
		d, err := time.ParseDuration(mb.PollInterval)
		if err != nil {
			return cfg, fmt.Errorf("analytics mode_b poll_interval: %w", err)
		}
		cfg.PollInterval = d
	}
	if mb.LeaseTTL != "" {
		d, err := time.ParseDuration(mb.LeaseTTL)
		if err != nil {
			return cfg, fmt.Errorf("analytics mode_b lease_ttl: %w", err)
		}
		cfg.LeaseTTL = d
	}
	return cfg, nil
}

// buildMasking resolves the configured request-filter plugins into a
// filter.Masking (ADR-009). Each plugin name must be registered (blank-imported)
// — an unknown name fails the boot. v1 supports one masking filter; the last
// configured one wins. Enabling masking logs the cache/cost warning (never
// silent). Returns nil when no plugin is configured (masking off, zero overhead).
func buildMasking(cfg *config.Config) (*filter.Masking, error) {
	var masking *filter.Masking
	for _, pc := range cfg.Plugins {
		f, ok := filter.Get(pc.Name)
		if !ok {
			return nil, fmt.Errorf("config: unknown plugin %q (registered: %v)", pc.Name, filter.Names())
		}
		m := &filter.Masking{Filter: f}
		scope := "all teams"
		if len(pc.Teams) == 0 {
			m.Global = true
		} else {
			m.Teams = make(map[string]bool, len(pc.Teams))
			for _, t := range pc.Teams {
				m.Teams[t] = true
			}
			scope = fmt.Sprintf("teams %v", pc.Teams)
		}
		masking = m
		fmt.Fprintf(os.Stderr, "inferplane: plugin %q enabled (%s) — WARNING: masked traffic re-serializes the body, abandoning verbatim forwarding, so prompt-cache hits are lost (up to ~10x cost); this is opt-in by design (ADR-009)\n", pc.Name, scope)
	}
	return masking, nil
}

// tracingConfig maps the file config block to the tracing package's mirror type
// (tracing imports no config — ADR-011 gate).
func tracingConfig(o *config.OTelConfig) tracing.Config {
	return tracing.Config{
		Endpoint:    o.Endpoint,
		Protocol:    o.Protocol,
		Insecure:    o.Insecure,
		SampleRatio: o.SampleRatio,
		ServiceName: o.ServiceName,
	}
}

// closeAll closes the partially-opened resources on a newGateway error path
// (pstore and pgstoreQ may be nil).
func closeAll(pstore providerstore.Store, pgstoreQ *pgstore.Store, store keystore.Store, aud *audit.Writer) {
	if pstore != nil {
		pstore.Close()
	}
	if pgstoreQ != nil {
		pgstoreQ.Close()
	}
	store.Close()
	aud.Close()
}

// DataAddr is the bound data-plane address (host:port), valid once newGateway
// returns — usable with ":0" configs to discover the OS-assigned port.
func (g *gateway) DataAddr() string { return g.dataLn.Addr().String() }

// AdminAddr is the bound admin-plane address (host:port).
func (g *gateway) AdminAddr() string { return g.adminLn.Addr().String() }

// reload re-reads the config file and atomically swaps the topology generation
// (providers + routes + pricing) — it touches NO stateful component (governor
// counters, keystore, audit writer, circuit breaker all persist; ADR-006).
// Validate-then-swap: a config that fails to load/build leaves the current
// generation serving and returns the error (fail-safe rollback). Serialized by
// reloadMu so concurrent SIGHUP triggers AND UI writes never race (ADR-008
// gate C3 — reload and the write path funnel through the same mutex).
func (g *gateway) reload() error {
	g.reloadMu.Lock()
	defer g.reloadMu.Unlock()
	return g.reloadLocked()
}

// reloadLocked is the swap body; the caller MUST hold reloadMu. It exists so the
// UI-write path can validate, persist, and swap under ONE lock acquisition
// without a reentrant deadlock on reload() (ADR-008 gate C3). It rebuilds the
// EFFECTIVE config (file + DB overlay) so SIGHUP picks up DB writes too.
func (g *gateway) reloadLocked() error {
	raw, err := config.LoadRaw(g.cfgPath)
	if err != nil {
		return fmt.Errorf("reload: %w", err)
	}
	cfg, err := buildEffective(raw, g.pstore)
	if err != nil {
		return fmt.Errorf("reload: %w", err)
	}
	st, identities, err := live.BuildState(cfg)
	if err != nil {
		return fmt.Errorf("reload: %w", err)
	}
	g.holder.Swap(st)                   // one atomic publish — every reader flips together
	g.router.RetainBreakers(identities) // drop breaker state for removed/re-pointed providers
	return nil
}

// writeMutation is the build-once-swap-once core of the UI-write path (ADR-008
// gates C2 + C3). Under reloadMu it: (1) builds a CANDIDATE effective topology
// from the current store PLUS the pending mutation (in memory, not persisted)
// and validates it via live.BuildState — on failure nothing is persisted and
// ErrInvalidTopology is returned (→ 400); (2) persists the mutation; (3) swaps
// the ALREADY-VALIDATED generation. The state published is byte-for-byte the one
// validated, so the DB can never hold a row that fails to build.
func (g *gateway) writeMutation(ctx context.Context, persist func(context.Context) error, mutate func(provs map[string]providerstore.ProviderRow, models map[string]providerstore.ModelRoute)) error {
	g.reloadMu.Lock()
	defer g.reloadMu.Unlock()

	raw, err := config.LoadRaw(g.cfgPath)
	if err != nil {
		return fmt.Errorf("%w: %v", configapi.ErrInvalidTopology, err)
	}
	provList, err := g.pstore.ListProviders(ctx)
	if err != nil {
		return err
	}
	models, err := g.pstore.ListModels(ctx)
	if err != nil {
		return err
	}
	provs := make(map[string]providerstore.ProviderRow, len(provList))
	for _, p := range provList {
		provs[p.Name] = p
	}
	mutate(provs, models) // apply the pending change in memory

	provSlice := make([]providerstore.ProviderRow, 0, len(provs))
	for _, p := range provs {
		provSlice = append(provSlice, p)
	}
	eff := providerstore.OverlayFrom(raw, provSlice, models)
	if err := config.ValidateModelAliases(eff.Models); err != nil {
		fmt.Fprintln(os.Stderr, "inferplane: rejected UI write (aliases):", err)
		return configapi.ErrInvalidTopology
	}
	if err := config.ResolveProviders(eff); err != nil {
		// Log the detail server-side; the client gets a fixed, sanitized 400 so a
		// ref / build detail never leaks in the response (P4 M1).
		fmt.Fprintln(os.Stderr, "inferplane: rejected UI write (resolve):", err)
		return configapi.ErrInvalidTopology
	}
	st, identities, err := live.BuildState(eff)
	if err != nil {
		fmt.Fprintln(os.Stderr, "inferplane: rejected UI write (build):", err)
		return configapi.ErrInvalidTopology
	}

	// Validated — persist, then publish the validated generation.
	if err := persist(ctx); err != nil {
		return err // e.g. providerstore.ErrNotFound on DELETE → 404; nothing swapped
	}
	g.holder.Swap(st)
	g.router.RetainBreakers(identities)
	return nil
}

// WriteProvider / DeleteProvider / WriteModel / DeleteModel implement
// configapi.Writer via writeMutation (ADR-008 T5).
func (g *gateway) WriteProvider(ctx context.Context, row providerstore.ProviderRow) error {
	return g.writeMutation(ctx,
		func(ctx context.Context) error { return g.pstore.UpsertProvider(ctx, row) },
		func(provs map[string]providerstore.ProviderRow, _ map[string]providerstore.ModelRoute) {
			provs[row.Name] = row
		},
	)
}

func (g *gateway) DeleteProvider(ctx context.Context, name string) error {
	return g.writeMutation(ctx,
		func(ctx context.Context) error { return g.pstore.DeleteProvider(ctx, name) },
		func(provs map[string]providerstore.ProviderRow, _ map[string]providerstore.ModelRoute) {
			delete(provs, name)
		},
	)
}

func (g *gateway) WriteModel(ctx context.Context, name string, route providerstore.ModelRoute) error {
	return g.writeMutation(ctx,
		func(ctx context.Context) error { return g.pstore.SetModel(ctx, name, route) },
		func(_ map[string]providerstore.ProviderRow, models map[string]providerstore.ModelRoute) {
			models[name] = route
		},
	)
}

func (g *gateway) DeleteModel(ctx context.Context, name string) error {
	return g.writeMutation(ctx,
		func(ctx context.Context) error { return g.pstore.DeleteModel(ctx, name) },
		func(_ map[string]providerstore.ProviderRow, models map[string]providerstore.ModelRoute) {
			delete(models, name)
		},
	)
}

// anchorWorker periodically anchors the audit chain head to WORM storage
// (ADR-012). It anchors only when the count advanced beyond the last SUCCESSFUL
// anchor, so a failed anchor is retried (not skipped). On ctx cancel it makes a
// final anchor under a FRESH bounded timeout (the worker ctx is already
// canceled), so the last head is witnessed without hanging shutdown. Failures
// are logged + counted, never fatal (best-effort, off the request path).
func (g *gateway) anchorWorker(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	t := time.NewTicker(g.anchorEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return // the FINAL anchor is done by finalAnchor() AFTER the writer drains
		case <-t.C:
			g.anchorOnce(ctx)
		}
	}
}

// bodyPurgeWorker periodically deletes expired/over-cap body rows (D4,
// ADR-018): TTL first, then oldest-first size-cap eviction (Store.Purge's
// contract). Best-effort — failures are logged, never fatal.
func (g *gateway) bodyPurgeWorker(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	const bodyPurgeEvery = 5 * time.Minute
	t := time.NewTicker(bodyPurgeEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := g.bodyStore.Purge(ctx, time.Now(), g.bodyMaxBytes); err != nil {
				fmt.Fprintln(os.Stderr, "inferplane: body store purge failed (will retry):", err)
			}
		}
	}
}

// anchorOnce anchors the current head if the durable count advanced beyond the
// last SUCCESSFUL anchor (so a failed anchor is retried, never skipped). Failures
// are logged + counted, never propagated (best-effort).
func (g *gateway) anchorOnce(ctx context.Context) {
	hash, count := g.aud.HeadHash()
	if count <= g.anchorLast.Load() {
		return
	}
	p := audit.AnchorPoint{Instance: g.instance, HeadHash: hash, Count: count, TS: time.Now().UTC().Format(time.RFC3339Nano)}
	if err := g.anchorer.Anchor(ctx, p); err != nil {
		fmt.Fprintln(os.Stderr, "inferplane: audit anchor failed (will retry):", err)
		g.metrics.IncAnchorFailure()
		return // do NOT advance → retried on the next tick / final
	}
	g.anchorLast.Store(count)
}

// finalAnchor witnesses the FINAL chain head on shutdown. It is deferred FIRST in
// serve so it runs LAST — AFTER the audit writer has drained (aud.Close), so the
// last in-flight records are included (ADR-012 P4 MAJOR: the periodic worker's
// snapshot would otherwise miss records still in the queue). Bounded fresh ctx.
func (g *gateway) finalAnchor() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	g.anchorOnce(ctx)
}

// reloadWorker serializes reload triggers (SIGHUP) on a single goroutine and
// exits when ctx is canceled. A reload already in progress finishes before the
// worker observes cancellation (the trigger calls reload synchronously), so a
// shutdown never interrupts a half-applied swap. Reload failures are logged,
// never fatal (a bad SIGHUP must not take the gateway down).
func (g *gateway) reloadWorker(ctx context.Context, trigger <-chan os.Signal, done chan<- struct{}) {
	defer close(done)
	for {
		select {
		case <-ctx.Done():
			return
		case <-trigger:
			if err := g.reload(); err != nil {
				fmt.Fprintln(os.Stderr, "inferplane: config reload failed (keeping current generation):", err)
			} else {
				fmt.Println("inferplane: config reloaded")
			}
		}
	}
}

func (g *gateway) pgstoreAggregatorWorker(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	if err := g.pgstoreAgg.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "inferplane: analytics mode_b aggregator failed:", err)
	}
}

// serve runs both planes until ctx is canceled (graceful drain within
// drain_grace) or a server fails. It owns closing the keystore and audit
// writer; the in-flight handlers finish before audit drains (§5.4). SIGHUP
// triggers a hot config reload via a single serialized worker.
func (g *gateway) serve(ctx context.Context) error {
	// Registered FIRST so it runs LAST (LIFO) — after aud.Close() below drains the
	// audit queue — so the final WORM anchor includes the last in-flight records
	// (ADR-012 P4: the periodic worker would otherwise snapshot before drain).
	if g.anchorer != nil {
		defer g.finalAnchor()
	}
	defer g.store.Close()
	// Wait for in-flight budget-alert webhook deliveries (D5b, ADR-017) before
	// exit so a rolling deploy's last-window alerts aren't silently abandoned.
	// Registered here so it runs after the data plane has drained (below), by
	// which point any final Settle→Observe goroutines have been spawned.
	if g.notifier != nil {
		defer g.notifier.Close()
	}
	// Shutdown order (LIFO — registered here, run in reverse): aud.Close drains
	// the writer (enqueuing the last records into the analytics sink) → the sink
	// drains its worker (ingesting them) → the index closes. So these are
	// registered index-first, sink-next, writer-last.
	if g.analyticsIdx != nil {
		defer g.analyticsIdx.Close()
	}
	if g.analyticsSink != nil {
		defer g.analyticsSink.Close()
	}
	defer g.aud.Close()
	if g.pstore != nil {
		defer g.pstore.Close()
	}
	if g.pgstoreQ != nil {
		defer g.pgstoreQ.Close()
	}
	// Body-store shutdown order (D4, ADR-018): bodyStore.Close() is registered
	// FIRST so it runs LAST — bodyRec.Close() (registered second, runs first)
	// drains the encrypt+write queue before the underlying store connection
	// closes (same recorder-drains-before-store-closes shape as the analytics
	// sink/index pair above).
	if g.bodyStore != nil {
		defer g.bodyStore.Close()
	}
	if g.bodyRec != nil {
		defer g.bodyRec.Close()
	}
	if g.otelDown != nil {
		// Flush spans on teardown under a bounded timeout; errors are logged, never
		// fatal (ADR-011 — tracing is best-effort, off the critical path).
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := g.otelDown(ctx); err != nil {
				fmt.Fprintln(os.Stderr, "inferplane: otel shutdown:", err)
			}
		}()
	}

	// SIGHUP → hot reload, on one worker with a clean lifecycle: signal.Stop on
	// exit, and wait for the worker to drain before serve returns (no leak).
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	workerCtx, cancelWorker := context.WithCancel(ctx)
	workerDone := make(chan struct{})
	go g.reloadWorker(workerCtx, hup, workerDone)
	// Periodic audit anchoring (ADR-012): same clean-lifecycle pattern. On
	// workerCtx cancel it makes a final anchor under a fresh bounded timeout.
	var anchorDone chan struct{}
	if g.anchorer != nil {
		anchorDone = make(chan struct{})
		go g.anchorWorker(workerCtx, anchorDone)
	}
	var pgstoreDone chan struct{}
	if g.pgstoreAgg != nil {
		pgstoreDone = make(chan struct{})
		go g.pgstoreAggregatorWorker(workerCtx, pgstoreDone)
	}
	// Body-store TTL/size-cap purge (D4, ADR-018): same clean-lifecycle
	// pattern as the anchor worker. Best-effort — failures are logged, never
	// fatal (purge is housekeeping, not on the request path).
	var bodyPurgeDone chan struct{}
	if g.bodyStore != nil {
		bodyPurgeDone = make(chan struct{})
		go g.bodyPurgeWorker(workerCtx, bodyPurgeDone)
	}
	defer func() {
		cancelWorker()
		<-workerDone
		if anchorDone != nil {
			<-anchorDone
		}
		if pgstoreDone != nil {
			<-pgstoreDone
		}
		if bodyPurgeDone != nil {
			<-bodyPurgeDone
		}
	}()

	errc := make(chan error, 2)
	go func() {
		if g.cfg.Server.TLS.CertFile != "" {
			errc <- g.dataSrv.ServeTLS(g.dataLn, g.cfg.Server.TLS.CertFile, g.cfg.Server.TLS.KeyFile)
		} else {
			errc <- g.dataSrv.Serve(g.dataLn)
		}
	}()
	// Admin plane stays plaintext: /metrics, /healthz, /readyz are typically
	// cluster-internal (scraped by Prometheus, probed by the kubelet).
	go func() { errc <- g.adminSrv.Serve(g.adminLn) }()

	select {
	case <-ctx.Done():
		grace := 10 * time.Second
		if g.cfg.Server.DrainGrace != "" {
			if d, err := time.ParseDuration(g.cfg.Server.DrainGrace); err == nil {
				grace = d
			}
		}
		shutCtx, cancel := context.WithTimeout(context.Background(), grace)
		defer cancel()
		// Stop accepting new requests and let in-flight handlers finish so their
		// request_completed audit records get enqueued before we drain.
		_ = g.dataSrv.Shutdown(shutCtx)
		_ = g.adminSrv.Shutdown(shutCtx)
		return nil
	case err := <-errc:
		// One plane failed: drain the other gracefully too, so in-flight
		// handlers finish and their audit records are enqueued before the
		// deferred writer close (§5.4) — don't leave it to process teardown.
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = g.dataSrv.Shutdown(shutCtx)
		_ = g.adminSrv.Shutdown(shutCtx)
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// oidcVerifier builds the admin-plane ID-token verifier from config, or nil
// when the oidc block is absent (static break-glass only — back-compat).
// Construction is lazy and does no I/O: an unreachable IdP at boot must not
// block startup (ADR-004 break-glass invariant).
func oidcVerifier(cfg *config.Config) server.OIDCVerifier {
	o := cfg.Server.AdminAuth.OIDC
	if o == nil {
		return nil
	}
	return adminauth.NewVerifier(adminauth.VerifierConfig{
		Issuer:      o.Issuer,
		ClientID:    o.ClientID,
		GroupsClaim: o.GroupsClaim,
	})
}

// liveView derives the secret-free config view from the current generation,
// so /admin/config reflects hot reloads (ADR-006). live never imports
// configapi — the view is built here in the assembly layer.
func liveView(holder *live.Holder, writable bool) func() configapi.View {
	return func() configapi.View {
		st := holder.Load()
		v := configapi.ViewFrom(st.ProviderConfigs(), st.Models())
		v.Writable = writable // capability hint for the console (ADR-008); not secret-bearing
		return v
	}
}

// liveExport derives the secret-free, config-shaped Git-export doc from the
// current generation (ADR-008 §3). Like liveView it reads the holder per call so
// the export reflects the latest UI writes / reloads; ProviderConfig.APIKey is
// dropped at marshal time by its `json:"-"` tag.
func liveExport(holder *live.Holder) func() configapi.ExportDoc {
	return func() configapi.ExportDoc {
		st := holder.Load()
		return configapi.ExportDocFrom(st.ProviderConfigs(), st.Models())
	}
}

// oidcMapping converts the config mapping rules into the adminauth shape
// (decoupled types — same pattern as governance.ConfigTeam).
func oidcMapping(cfg *config.Config) adminauth.MappingConfig {
	o := cfg.Server.AdminAuth.OIDC
	if o == nil {
		return adminauth.MappingConfig{}
	}
	mc := adminauth.MappingConfig{AdminGroups: o.AdminGroups}
	for _, gm := range o.GroupMappings {
		mc.GroupMappings = append(mc.GroupMappings, adminauth.GroupMapping{Group: gm.Group, Teams: gm.Teams})
	}
	return mc
}

// instanceID names this gateway instance for the audit hash chain. Each process
// run gets a unique id (hostname + per-run nonce) so a restart starts a distinct
// per-instance chain segment (design §5.4) rather than reading as tampering.
func instanceID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "inferplane"
	}
	return host + "-" + ulid.New() // unique per process run → distinct audit chain segment
}

// buildSinks constructs audit sinks from config. "stdout" is best-effort;
// "file" sinks are required (they gate buffer_then_block durability §5.4).
func buildSinks(cfgs []config.AuditSink) ([]audit.Sink, error) {
	var sinks []audit.Sink
	for _, s := range cfgs {
		switch s.Type {
		case "stdout":
			sinks = append(sinks, audit.NewStdoutSink())
		case "file":
			fs, err := audit.NewFileSink(s.Path, true)
			if err != nil {
				return nil, fmt.Errorf("file sink %q: %w", s.Path, err)
			}
			sinks = append(sinks, fs)
		default:
			return nil, fmt.Errorf("unknown sink type %q", s.Type)
		}
	}
	return sinks, nil
}
