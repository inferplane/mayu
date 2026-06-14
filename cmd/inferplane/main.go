// Command inferplane is the gateway binary. Subcommands: `serve` (run the
// gateway), `keys` (local virtual-key bootstrap CRUD), and `audit` (verify the
// tamper-evident log chain).
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	_ "github.com/inferplane/inferplane/providers/anthropic"    // register "anthropic"
	_ "github.com/inferplane/inferplane/providers/bedrock"      // register "bedrock"
	_ "github.com/inferplane/inferplane/providers/openaicompat" // register "openai_compatible"
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
	case "report":
		os.Exit(reportCmd(os.Args[2:]))
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
	fmt.Fprintln(os.Stderr, "  inferplane report --file <path> [--since <RFC3339>] [--until <RFC3339>] [--by team|team,model]")
}

// run assembles the gateway (gateway.go) and serves until SIGINT/SIGTERM.
func run(cfgPath string) error {
	g, err := newGateway(cfgPath)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	fmt.Printf("inferplane serving data=%s admin=%s\n", g.DataAddr(), g.AdminAddr())
	return g.serve(ctx)
}
