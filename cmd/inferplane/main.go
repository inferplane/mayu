// Command inferplane is the gateway binary. Subcommands: `serve` (run the
// gateway), `keys` (local virtual-key bootstrap CRUD), and `audit` (verify the
// tamper-evident log chain).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/inferplane/inferplane/internal/audit"
	"github.com/inferplane/inferplane/internal/budget"
	"github.com/inferplane/inferplane/internal/config"
	"github.com/inferplane/inferplane/internal/governance"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/limiter"
	"github.com/inferplane/inferplane/internal/pricing"
	"github.com/inferplane/inferplane/internal/router"
	"github.com/inferplane/inferplane/internal/server"
	"github.com/inferplane/inferplane/pkg/ulid"
	"github.com/inferplane/inferplane/providers"

	_ "github.com/inferplane/inferplane/providers/anthropic" // register "anthropic"
	_ "github.com/inferplane/inferplane/providers/bedrock"   // register "bedrock"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		cfgPath := "config.json"
		for i := 2; i < len(os.Args)-1; i++ {
			if os.Args[i] == "--config" {
				cfgPath = os.Args[i+1]
			}
		}
		if err := run(cfgPath); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "keys":
		if err := keysCmd(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "audit":
		os.Exit(auditCmd(os.Args[2:]))
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage:")
	fmt.Fprintln(os.Stderr, "  inferplane serve --config <path>")
	fmt.Fprintln(os.Stderr, "  inferplane keys create --team <t> --models <csv> --store <path>")
	fmt.Fprintln(os.Stderr, "  inferplane keys list --store <path>")
	fmt.Fprintln(os.Stderr, "  inferplane keys revoke --id <key_id> --store <path>")
	fmt.Fprintln(os.Stderr, "  inferplane audit verify --file <path>")
}

func run(cfgPath string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}

	// Virtual-key store: KeyAuth resolves client keys against it (§5.1).
	store, err := keystore.OpenSQLite(cfg.KeyStore.Path)
	if err != nil {
		return fmt.Errorf("keystore: %w", err)
	}
	defer store.Close()

	// Audit writer: build sinks from config, then the single-writer chain.
	sinks, err := buildSinks(cfg.Audit.Sinks)
	if err != nil {
		return fmt.Errorf("audit sinks: %w", err)
	}
	aud, err := audit.NewWriter(instanceID(), cfg.Audit.Buffer.Path, sinks)
	if err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	defer aud.Close()

	// model_api[providerName] = {upstreamModelID: api} — gathered from model
	// targets that name a non-empty api, so the bedrock factory can override
	// its default invoke/converse routing per upstream model.
	modelAPIByProvider := map[string]map[string]string{}
	for _, mc := range cfg.Models {
		for _, t := range mc.Targets {
			if t.API != "" {
				if modelAPIByProvider[t.Provider] == nil {
					modelAPIByProvider[t.Provider] = map[string]string{}
				}
				modelAPIByProvider[t.Provider][t.Model] = t.API
			}
		}
	}

	provs := map[string]providers.Provider{}
	for name, pc := range cfg.Providers {
		var settings map[string]string
		if pc.Type == "bedrock" {
			settings = map[string]string{
				"region":    pc.Region,
				"auth_mode": pc.Auth.Mode,
				"profile":   pc.Auth.Profile,
			}
			if m := modelAPIByProvider[name]; len(m) > 0 {
				b, _ := json.Marshal(m)
				settings["model_api"] = string(b)
			}
		}
		p, err := providers.New(providers.Config{Type: pc.Type, BaseURL: pc.BaseURL, APIKey: pc.APIKey, Settings: settings})
		if err != nil {
			return fmt.Errorf("provider %q: %w", name, err)
		}
		provs[name] = p
	}
	r := router.New(provs, cfg.Models)

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

	overrides := map[string]map[string]pricing.ConfigRate{}
	for provider, models := range cfg.Pricing.Overrides {
		overrides[provider] = map[string]pricing.ConfigRate{}
		for model, rc := range models {
			overrides[provider][model] = pricing.ConfigRate{
				InputPerMTok:        rc.InputPerMTok,
				OutputPerMTok:       rc.OutputPerMTok,
				CacheReadPerMTok:    rc.CacheReadPerMTok,
				CacheWrite5mPerMTok: rc.CacheWrite5mPerMTok,
				CacheWrite1hPerMTok: rc.CacheWrite1hPerMTok,
			}
		}
	}
	tbl := pricing.FromConfig(cfg.Pricing.OnMissing, overrides)
	gov := governance.NewGovernor(policies, limiter.NewMemory(), budget.NewMemory(), tbl)

	dataSrv := &http.Server{Addr: cfg.Server.Listen, Handler: server.DataMux(r, store, aud, gov)}
	adminSrv := &http.Server{Addr: cfg.Server.AdminListen, Handler: server.AdminMux(store, cfg.Server.AdminAuth.Tokens)}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errc := make(chan error, 2)
	go func() { errc <- dataSrv.ListenAndServe() }()
	go func() { errc <- adminSrv.ListenAndServe() }()
	fmt.Printf("inferplane serving data=%s admin=%s\n", cfg.Server.Listen, cfg.Server.AdminListen)

	select {
	case <-ctx.Done():
		grace := 10 * time.Second
		if cfg.Server.DrainGrace != "" {
			if d, err := time.ParseDuration(cfg.Server.DrainGrace); err == nil {
				grace = d
			}
		}
		shutCtx, cancel := context.WithTimeout(context.Background(), grace)
		defer cancel()
		// Stop accepting new requests and let in-flight handlers finish so their
		// request_completed audit records get enqueued before we drain.
		_ = dataSrv.Shutdown(shutCtx)
		_ = adminSrv.Shutdown(shutCtx)
		return nil
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
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
