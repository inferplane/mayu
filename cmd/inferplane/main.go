// Command inferplane is the gateway binary. M2 implements the `serve`
// subcommand; `keys` and `audit` arrive in M3.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/inferplane/inferplane/internal/config"
	"github.com/inferplane/inferplane/internal/router"
	"github.com/inferplane/inferplane/internal/server"
	"github.com/inferplane/inferplane/providers"

	_ "github.com/inferplane/inferplane/providers/anthropic" // register "anthropic"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] != "serve" {
		fmt.Fprintln(os.Stderr, "usage: inferplane serve --config <path>")
		os.Exit(2)
	}
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
}

func run(cfgPath string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	devKey := os.Getenv("INFERPLANE_DEV_KEY")
	if devKey == "" {
		return errors.New("INFERPLANE_DEV_KEY must be set (M2 temporary auth)")
	}

	provs := map[string]providers.Provider{}
	for name, pc := range cfg.Providers {
		p, err := providers.New(providers.Config{Type: pc.Type, BaseURL: pc.BaseURL, APIKey: pc.APIKey})
		if err != nil {
			return fmt.Errorf("provider %q: %w", name, err)
		}
		provs[name] = p
	}
	r := router.New(provs, cfg.Models)

	dataSrv := &http.Server{Addr: cfg.Server.Listen, Handler: server.DataMux(r, devKey)}
	adminSrv := &http.Server{Addr: cfg.Server.AdminListen, Handler: server.AdminMux()}

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
