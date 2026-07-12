package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/inferplane/inferplane/internal/bodystore"
	"github.com/inferplane/inferplane/internal/config"
)

// bodiesCmd implements `inferplane bodies rewrap-key`, the ADR-018 deferred
// key-rotation item: re-wraps every row's data key from an old master key to
// a new one, touching only the wrapped_key_* columns -- request/response
// ciphertext is never read or rewritten. Returns the process exit code.
func bodiesCmd(args []string) int {
	if len(args) < 1 || args[0] != "rewrap-key" {
		fmt.Fprintln(os.Stderr, "usage: inferplane bodies rewrap-key --store <path> --old-key-env <VAR>|--old-key-file <path> --new-key-env <VAR>|--new-key-file <path>")
		return 2
	}
	return bodiesRewrapKey(args[1:])
}

func bodiesRewrapKey(args []string) int {
	fs := flag.NewFlagSet("bodies rewrap-key", flag.ContinueOnError)
	storePath := fs.String("store", "", "path to the SQLite body store")
	pgDSNEnv := fs.String("postgres-dsn-env", "", "env var holding the postgres DSN")
	oldKeyEnv := fs.String("old-key-env", "", "env var holding the old 64-hex-char master key")
	oldKeyFile := fs.String("old-key-file", "", "file holding the old 64-hex-char master key")
	newKeyEnv := fs.String("new-key-env", "", "env var holding the new 64-hex-char master key")
	newKeyFile := fs.String("new-key-file", "", "file holding the new 64-hex-char master key")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// exactly one of --store / --postgres-dsn-env
	haveStore, havePG := *storePath != "", *pgDSNEnv != ""
	if haveStore == havePG {
		fmt.Fprintln(os.Stderr, "bodies rewrap-key: exactly one of --store or --postgres-dsn-env is required")
		return 2
	}
	oldRef, err := oneOfRef("old-key", *oldKeyEnv, *oldKeyFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bodies rewrap-key:", err)
		return 2
	}
	newRef, err := oneOfRef("new-key", *newKeyEnv, *newKeyFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bodies rewrap-key:", err)
		return 2
	}

	oldMaster, err := resolveMasterKey(oldRef)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bodies rewrap-key: old key:", err)
		return 2
	}
	newMaster, err := resolveMasterKey(newRef)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bodies rewrap-key: new key:", err)
		return 2
	}

	var store bodystore.Store
	if haveStore {
		store, err = bodystore.OpenSQLite(*storePath)
	} else {
		dsnRef := &config.SecretRef{Env: *pgDSNEnv}
		if err := config.ValidateSecretRef(dsnRef); err != nil {
			fmt.Fprintln(os.Stderr, "bodies rewrap-key: postgres dsn ref:", err)
			return 2
		}
		dsn, rerr := config.ResolveSecretRef(dsnRef)
		if rerr != nil || dsn == "" {
			fmt.Fprintln(os.Stderr, "bodies rewrap-key: postgres dsn is empty or unresolved")
			return 2
		}
		store, err = bodystore.NewPostgres(context.Background(), dsn)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "bodies rewrap-key: open store:", err)
		return 1
	}
	defer store.Close()

	ctx := context.Background()
	rows, err := store.ListWrappedKeys(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bodies rewrap-key: list rows:", err)
		return 1
	}

	var rewrapped, skipped, raced int
	for _, row := range rows {
		newNonce, newCT, rerr := bodystore.RewrapKey(oldMaster, newMaster, row.Nonce, row.CT)
		if rerr != nil {
			skipped++
			continue
		}
		matched, uerr := store.UpdateWrappedKey(ctx, row.Ref, row.Nonce, row.CT, newNonce, newCT)
		if uerr != nil {
			fmt.Fprintln(os.Stderr, "bodies rewrap-key: update", row.Ref, ":", uerr)
			skipped++
			continue
		}
		if matched {
			rewrapped++
		} else {
			raced++
		}
	}

	fmt.Printf("rewrapped=%d skipped=%d raced=%d\n", rewrapped, skipped, raced)
	if rewrapped == 0 && skipped > 0 {
		return 1
	}
	return 0
}

// oneOfRef builds a config.SecretRef from exactly one of an env-var name or a
// file path (mirrors the shape of every other secret-ref CLI flag pair in
// this repo -- never accepts the raw secret value itself).
func oneOfRef(name, env, file string) (*config.SecretRef, error) {
	if (env == "") == (file == "") {
		return nil, fmt.Errorf("exactly one of --%s-env or --%s-file is required", name, name)
	}
	ref := &config.SecretRef{Env: env, File: file}
	if err := config.ValidateSecretRef(ref); err != nil {
		return nil, err
	}
	return ref, nil
}

func resolveMasterKey(ref *config.SecretRef) ([32]byte, error) {
	hexKey, err := config.ResolveSecretRef(ref)
	if err != nil {
		return [32]byte{}, err
	}
	return bodystore.ParseMasterKey(hexKey)
}
