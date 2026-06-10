// Command inferplane is the gateway binary. Subcommands: `serve` (run the
// gateway), `keys` (local virtual-key bootstrap CRUD), and `audit` (verify the
// tamper-evident log chain).
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/inferplane/inferplane/internal/audit"
	"github.com/inferplane/inferplane/internal/config"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/router"
	"github.com/inferplane/inferplane/internal/server"
	"github.com/inferplane/inferplane/providers"

	_ "github.com/inferplane/inferplane/providers/anthropic" // register "anthropic"
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

	provs := map[string]providers.Provider{}
	for name, pc := range cfg.Providers {
		p, err := providers.New(providers.Config{Type: pc.Type, BaseURL: pc.BaseURL, APIKey: pc.APIKey})
		if err != nil {
			return fmt.Errorf("provider %q: %w", name, err)
		}
		provs[name] = p
	}
	r := router.New(provs, cfg.Models)

	dataSrv := &http.Server{Addr: cfg.Server.Listen, Handler: server.DataMux(r, store, aud)}
	adminSrv := &http.Server{Addr: cfg.Server.AdminListen, Handler: server.AdminMux(store, cfg.Server.AdminAuth.Tokens)}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errc := make(chan error, 2)
	go func() { errc <- dataSrv.ListenAndServe() }()
	go func() { errc <- adminSrv.ListenAndServe() }()
	fmt.Printf("inferplane serving data=%s admin=%s\n", cfg.Server.Listen, cfg.Server.AdminListen)

	select {
	case <-ctx.Done():
		_ = dataSrv.Close()
		_ = adminSrv.Close()
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

// instanceID names this gateway instance for the audit hash chain (per-instance
// chains). Hostname falls back to a fixed name when unavailable.
func instanceID() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "inferplane"
}
