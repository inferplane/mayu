package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/inferplane/inferplane/internal/audit"
)

// auditCmd implements `inferplane audit verify --file <path>`, checking the
// tamper-evident hash chain. Returns the process exit code.
func auditCmd(args []string) int {
	if len(args) < 1 || args[0] != "verify" {
		fmt.Fprintln(os.Stderr, "usage: inferplane audit verify --file <path>")
		return 2
	}
	fs := flag.NewFlagSet("audit verify", flag.ContinueOnError)
	file := fs.String("file", "", "path to the JSONL audit log (required)")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if *file == "" {
		fmt.Fprintln(os.Stderr, "audit verify: --file is required")
		return 2
	}
	f, err := os.Open(*file)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	defer f.Close()
	res, err := audit.Verify(f)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if res.OK {
		fmt.Printf("chain OK (%d records)\n", res.Records)
		return 0
	}
	fmt.Printf("chain BROKEN at record %d: %s\n", res.BrokenAt, res.Reason)
	return 1
}
