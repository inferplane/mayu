package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/inferplane/inferplane/internal/keystore"
)

// keysCmd implements the local virtual-key bootstrap CLI (server-not-running):
// `inferplane keys create|list|revoke`, writing directly to the SQLite store.
func keysCmd(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: inferplane keys create|list|revoke ...")
	}
	switch args[0] {
	case "create":
		return keysCreate(args[1:])
	case "list":
		return keysList(args[1:])
	case "revoke":
		return keysRevoke(args[1:])
	default:
		return fmt.Errorf("unknown keys subcommand %q", args[0])
	}
}

func keysCreate(args []string) error {
	fs := flag.NewFlagSet("keys create", flag.ContinueOnError)
	team := fs.String("team", "", "team name (required)")
	models := fs.String("models", "*", "comma-separated allowed models, or * for all")
	store := fs.String("store", "", "path to the SQLite key store (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *team == "" || *store == "" {
		return fmt.Errorf("keys create: --team and --store are required")
	}
	s, err := keystore.OpenSQLite(*store)
	if err != nil {
		return err
	}
	defer s.Close()
	plaintext, p, err := s.Create(context.Background(), *team, splitCSV(*models))
	if err != nil {
		return err
	}
	// Plaintext is shown ONCE — never stored, never recoverable.
	fmt.Println(plaintext)
	fmt.Printf("key_id: %s\n", p.KeyID)
	// Local bootstrap audit note (§5.5): the full audit writer is the runtime path.
	fmt.Fprintf(os.Stderr, "audit: key %s created for team %s\n", p.KeyID, *team)
	return nil
}

func keysList(args []string) error {
	fs := flag.NewFlagSet("keys list", flag.ContinueOnError)
	store := fs.String("store", "", "path to the SQLite key store (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *store == "" {
		return fmt.Errorf("keys list: --store is required")
	}
	s, err := keystore.OpenSQLite(*store)
	if err != nil {
		return err
	}
	defer s.Close()
	ps, err := s.List(context.Background())
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "KEY_ID\tTEAM\tMODELS")
	for _, p := range ps {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", p.KeyID, p.Team, strings.Join(p.AllowedModels, ","))
	}
	return tw.Flush()
}

func keysRevoke(args []string) error {
	fs := flag.NewFlagSet("keys revoke", flag.ContinueOnError)
	id := fs.String("id", "", "key_id to revoke (required)")
	store := fs.String("store", "", "path to the SQLite key store (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == "" || *store == "" {
		return fmt.Errorf("keys revoke: --id and --store are required")
	}
	s, err := keystore.OpenSQLite(*store)
	if err != nil {
		return err
	}
	defer s.Close()
	if err := s.Revoke(context.Background(), *id); err != nil {
		return err
	}
	fmt.Printf("revoked %s\n", *id)
	return nil
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
