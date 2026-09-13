// Command mayu is the data-plane binary. Subcommands: `serve` (run the
// gateway), `keys` (local virtual-key bootstrap CRUD), `audit` (verify the
// tamper-evident log chain), and `login`/`token`/`logout` (ADR-028 — OIDC
// login for humans, trading an IdP session for an automatically-renewing
// short-lived virtual key instead of a hand-copied long-lived one).
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	_ "time/tzdata" // embed the IANA tz database: distroless/static ships no /usr/share/zoneinfo, so a named-zone LoadLocation would fail only in production

	_ "github.com/inferplane/inferplane/plugins/piimask"            // register "pii-mask" filter (ADR-009)
	_ "github.com/inferplane/inferplane/providers/anthropic"        // register "anthropic"
	_ "github.com/inferplane/inferplane/providers/bedrock"          // register "bedrock"
	_ "github.com/inferplane/inferplane/providers/bedrockresponses" // register "bedrock_responses"
	_ "github.com/inferplane/inferplane/providers/openaicompat"     // register "openai_compatible"
	_ "github.com/inferplane/inferplane/providers/openairesponses"  // register "openai_responses"
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
	case "bodies":
		os.Exit(bodiesCmd(os.Args[2:]))
	case "report":
		os.Exit(reportCmd(os.Args[2:]))
	case "pricing":
		os.Exit(pricingCmd(os.Args[2:]))
	case "login":
		if err := loginCmd(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "token":
		if err := tokenCmd(os.Args[2:]); err != nil {
			os.Exit(1) // tokenCmd already wrote its own stderr hint
		}
	case "logout":
		if err := logoutCmd(os.Args[2:]); err != nil {
			os.Exit(1) // logoutCmd already wrote its own stderr warning
		}
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage:")
	fmt.Fprintln(os.Stderr, "  mayu serve --config <path>")
	fmt.Fprintln(os.Stderr, "  mayu keys create --team <t> --models <csv> --store <path>")
	fmt.Fprintln(os.Stderr, "  mayu keys list --store <path>")
	fmt.Fprintln(os.Stderr, "  mayu keys revoke --id <key_id> --store <path>")
	fmt.Fprintln(os.Stderr, "  mayu audit verify --file <path>")
	fmt.Fprintln(os.Stderr, "  mayu bodies rewrap-key --store <path> --old-key-env <VAR>|--old-key-file <path> --new-key-env <VAR>|--new-key-file <path>")
	fmt.Fprintln(os.Stderr, "  mayu report --file <path> [--since <RFC3339>] [--until <RFC3339>] [--by team|team,model]")
	fmt.Fprintln(os.Stderr, "  mayu pricing check --config <path>")
	fmt.Fprintln(os.Stderr, "  mayu pricing sync --config <path> [--out <path>]")
	fmt.Fprintln(os.Stderr, "  mayu login --gateway <url> [--team <t>] [--issuer <url> --client-id <id>] [--id-token-command <cmd>]")
	fmt.Fprintln(os.Stderr, "  mayu token [--export] [--raw]")
	fmt.Fprintln(os.Stderr, "  mayu logout")
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
