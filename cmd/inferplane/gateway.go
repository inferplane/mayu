package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/inferplane/inferplane/internal/adminauth"
	"github.com/inferplane/inferplane/internal/audit"
	"github.com/inferplane/inferplane/internal/budget"
	"github.com/inferplane/inferplane/internal/config"
	"github.com/inferplane/inferplane/internal/governance"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/limiter"
	"github.com/inferplane/inferplane/internal/live"
	"github.com/inferplane/inferplane/internal/metrics"
	"github.com/inferplane/inferplane/internal/router"
	"github.com/inferplane/inferplane/internal/server"
	"github.com/inferplane/inferplane/internal/server/configapi"
	"github.com/inferplane/inferplane/pkg/ulid"
)

// gateway is the fully wired serve assembly with its listeners already bound.
// Binding in newGateway (rather than inside serve) makes ":0" configs testable:
// the OS-chosen ports are discoverable via DataAddr/AdminAddr before traffic.
type gateway struct {
	cfg      *config.Config
	store    keystore.Store
	aud      *audit.Writer
	holder   *live.Holder
	router   *router.Router
	dataLn   net.Listener
	adminLn  net.Listener
	dataSrv  *http.Server
	adminSrv *http.Server
}

// newGateway loads config and assembles the full serve wiring — metrics,
// keystore, audit writer, providers, router, governor, both muxes — then binds
// the data and admin listeners. On error every partially opened resource is
// closed. The caller must call serve (which owns shutdown + resource closing).
func newGateway(cfgPath string) (*gateway, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, err
	}

	// Prometheus metrics sink: owned by main, threaded into the audit writer,
	// router, governor, and ingress handlers, and exposed on the admin /metrics.
	m := metrics.New()

	// Virtual-key store: KeyAuth resolves client keys against it (§5.1).
	store, err := keystore.OpenSQLite(cfg.KeyStore.Path)
	if err != nil {
		return nil, fmt.Errorf("keystore: %w", err)
	}

	// Audit writer: build sinks from config, then the single-writer chain.
	sinks, err := buildSinks(cfg.Audit.Sinks)
	if err != nil {
		store.Close()
		return nil, fmt.Errorf("audit sinks: %w", err)
	}
	aud, err := audit.NewWriter(instanceID(), cfg.Audit.Buffer.Path, sinks)
	if err != nil {
		store.Close()
		return nil, fmt.Errorf("audit: %w", err)
	}
	aud.SetMetrics(m) // audit_write_failures / buffer_utilization

	// Topology generation (providers + routes + pricing) — built by the
	// topology-only builder so the same path serves boot and hot reload
	// (ADR-006). Published behind an atomic holder the router reads.
	st, _, err := live.BuildState(cfg)
	if err != nil {
		store.Close()
		aud.Close()
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

	// Optional self-TLS for the data plane (design §2.3): non-K8s single-binary
	// deployments can terminate their own TLS; K8s terminates at ingress/mesh.
	// The pair must be fully specified or fully empty.
	if err := server.ValidateTLS(cfg.Server.TLS.CertFile, cfg.Server.TLS.KeyFile); err != nil {
		store.Close()
		aud.Close()
		return nil, err
	}

	dataLn, err := net.Listen("tcp", cfg.Server.Listen)
	if err != nil {
		store.Close()
		aud.Close()
		return nil, fmt.Errorf("data listen %q: %w", cfg.Server.Listen, err)
	}
	adminLn, err := net.Listen("tcp", cfg.Server.AdminListen)
	if err != nil {
		dataLn.Close()
		store.Close()
		aud.Close()
		return nil, fmt.Errorf("admin listen %q: %w", cfg.Server.AdminListen, err)
	}

	return &gateway{
		cfg:      cfg,
		store:    store,
		aud:      aud,
		holder:   holder,
		router:   r,
		dataLn:   dataLn,
		adminLn:  adminLn,
		dataSrv:  &http.Server{Handler: server.DataMux(r, store, aud, gov, m)},
		adminSrv: &http.Server{Handler: server.AdminMux(store, cfg.Server.AdminAuth.Tokens, oidcVerifier(cfg), oidcMapping(cfg), configapi.ViewFrom(cfg.Providers, cfg.Models), aud, m)},
	}, nil
}

// DataAddr is the bound data-plane address (host:port), valid once newGateway
// returns — usable with ":0" configs to discover the OS-assigned port.
func (g *gateway) DataAddr() string { return g.dataLn.Addr().String() }

// AdminAddr is the bound admin-plane address (host:port).
func (g *gateway) AdminAddr() string { return g.adminLn.Addr().String() }

// serve runs both planes until ctx is canceled (graceful drain within
// drain_grace) or a server fails. It owns closing the keystore and audit
// writer; the in-flight handlers finish before audit drains (§5.4).
func (g *gateway) serve(ctx context.Context) error {
	defer g.store.Close()
	defer g.aud.Close()

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
