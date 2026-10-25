package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/datcal/ambar/internal/auth"
)

const tokenUsage = `Usage:
  ambar token add <username> --name NAME [--scope read|write|admin]

Creates an API token and prints it once, on stdout. The tokens page in the UI does the
same; this exists for a machine that has no browser session to create one from — a
remote derive worker, set up over SSH.
`

func runToken(args []string) error {
	if len(args) == 0 || args[0] != "add" {
		fmt.Fprint(os.Stderr, tokenUsage)
		return errors.New("`ambar token` needs the add subcommand")
	}

	fs := flag.NewFlagSet("ambar token add", flag.ContinueOnError)
	name := fs.String("name", "", "what the token is for, as the tokens page lists it")
	scope := fs.String("scope", auth.ScopeRead, "read, write or admin")
	positional, err := parseFlags(fs, args[1:])
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		fmt.Fprint(os.Stderr, tokenUsage)
		return errors.New("`ambar token add` takes exactly one username")
	}
	switch *scope {
	case auth.ScopeRead, auth.ScopeWrite, auth.ScopeAdmin:
	default:
		return fmt.Errorf("unknown scope %q; use read, write or admin", *scope)
	}

	ctx := context.Background()
	_, database, err := openDatabase(ctx)
	if err != nil {
		return err
	}
	defer database.Close() //nolint:errcheck

	user, err := auth.NewUserStore(database).ByUsername(ctx, positional[0])
	if err != nil {
		return fmt.Errorf("user %q: %w", positional[0], err)
	}
	plaintext, t, err := auth.NewTokenStore(database).Create(ctx, user.ID, *name, []string{*scope}, nil)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "created token %q for %s with scopes %v; it is shown once:\n",
		t.Name, user.Username, t.Scopes)
	fmt.Println(plaintext)
	return nil
}
