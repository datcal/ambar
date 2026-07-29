package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"golang.org/x/term"

	"github.com/datcal/ambar/internal/audit"
	"github.com/datcal/ambar/internal/auth"
	"github.com/datcal/ambar/internal/config"
	"github.com/datcal/ambar/internal/db"
)

const userUsage = `Usage:
  ambar user add <username> [--role user] [--password-stdin]
  ambar user list
`

func runUser(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, userUsage)
		return errors.New("`ambar user` needs a subcommand")
	}
	switch args[0] {
	case "add":
		return runUserAdd(args[1:])
	case "list":
		return runUserList(args[1:])
	default:
		fmt.Fprint(os.Stderr, userUsage)
		return fmt.Errorf("unknown subcommand `ambar user %s`", args[0])
	}
}

// openDatabase loads the configuration and opens a migrated database.
//
// The CLI shares config.Load with the server on purpose: one code path means the
// CLI cannot succeed against a configuration the server would reject. In the
// intended deployment both run in the same container with the same environment
// (§17 documents `docker exec ambar /ambar backup`).
func openDatabase(ctx context.Context) (*config.Config, *db.DB, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, fmt.Errorf("configuration is not usable:\n%w", err)
	}
	database, err := db.Open(cfg.DatabasePath())
	if err != nil {
		return nil, nil, err
	}
	if _, err := database.Migrate(ctx); err != nil {
		database.Close()
		return nil, nil, err
	}
	return cfg, database, nil
}

// parseFlags parses a flag set that may have flags on either side of its
// positional arguments, and returns the positionals.
//
// The stdlib flag package stops at the first non-flag argument, so a plain
// fs.Parse would read `user add alice --password-stdin` as two usernames. That is
// the form the README and docker-compose.yml both document, so it has to work.
// Re-parsing what is left after each positional handles flags in any position.
func parseFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for rest := args; ; {
		if err := fs.Parse(rest); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return positional, nil
		}
		positional = append(positional, fs.Arg(0))
		rest = fs.Args()[1:]
	}
}

func runUserAdd(args []string) error {
	fs := flag.NewFlagSet("ambar user add", flag.ContinueOnError)
	role := fs.String("role", auth.RoleUser, "role for the new user (only \"user\" exists)")
	passwordStdin := fs.Bool("password-stdin", false,
		"read the password from the first line of stdin instead of prompting")

	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		fmt.Fprint(os.Stderr, userUsage)
		return errors.New("`ambar user add` takes exactly one username")
	}

	username := auth.NormalizeUsername(positional[0])
	// Validate before asking for a password: being told the username is invalid
	// after typing a password twice is needless.
	if err := auth.ValidateUsername(username); err != nil {
		return err
	}

	ctx := context.Background()
	_, database, err := openDatabase(ctx)
	if err != nil {
		return err
	}
	defer database.Close() //nolint:errcheck // nothing useful to do on close failure here

	users := auth.NewUserStore(database)

	// A friendlier error than the UNIQUE constraint, though Create still relies
	// on the constraint as the real guard.
	if _, err := users.ByUsername(ctx, username); err == nil {
		return fmt.Errorf("user %q already exists", username)
	} else if !errors.Is(err, auth.ErrUserNotFound) {
		return err
	}

	password, err := readNewPassword(*passwordStdin)
	if err != nil {
		return err
	}

	user, err := users.Create(ctx, username, password, *role)
	if err != nil {
		return err
	}

	audit.New(database, newLogger()).Record(ctx, audit.Entry{
		UserID: &user.ID, Action: audit.ActionUserCreated,
		Entity: "user", EntityID: user.Username,
		Detail: map[string]any{"role": user.Role, "via": "cli"},
	})

	fmt.Printf("created user %q (id %d, role %s)\n", user.Username, user.ID, user.Role)
	return nil
}

// readNewPassword collects and confirms a password.
func readNewPassword(fromStdin bool) (string, error) {
	if fromStdin {
		// No confirmation prompt: a script that pipes the wrong thing twice is
		// no safer, and this path exists for `docker exec -i`.
		reader := bufio.NewReader(os.Stdin)
		line, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", fmt.Errorf("read password from stdin: %w", err)
		}
		password := strings.TrimRight(line, "\r\n")
		if err := checkPasswordLength(password); err != nil {
			return "", err
		}
		return password, nil
	}

	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return "", errors.New("stdin is not a terminal, so the password cannot be prompted for; " +
			"use --password-stdin (for example: printf '%s' \"$PASSWORD\" | ambar user add alice --password-stdin)")
	}

	fmt.Printf("Password (at least %d characters, not echoed): ", auth.MinPasswordLength)
	first, err := term.ReadPassword(fd)
	fmt.Println()
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}

	fmt.Print("Repeat password: ")
	second, err := term.ReadPassword(fd)
	fmt.Println()
	if err != nil {
		return "", fmt.Errorf("read password confirmation: %w", err)
	}

	if string(first) != string(second) {
		return "", errors.New("the two passwords do not match")
	}
	password := string(first)
	if err := checkPasswordLength(password); err != nil {
		return "", err
	}
	return password, nil
}

func checkPasswordLength(password string) error {
	if len([]rune(password)) < auth.MinPasswordLength {
		return fmt.Errorf("password must be at least %d characters", auth.MinPasswordLength)
	}
	return nil
}

func runUserList(args []string) error {
	fs := flag.NewFlagSet("ambar user list", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx := context.Background()
	_, database, err := openDatabase(ctx)
	if err != nil {
		return err
	}
	defer database.Close()

	users, err := auth.NewUserStore(database).List(ctx)
	if err != nil {
		return err
	}
	if len(users) == 0 {
		fmt.Println("no users yet — create one with `ambar user add <username>`")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 8, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tUSERNAME\tROLE\tCREATED\tLAST LOGIN")
	for _, u := range users {
		last := "never"
		if u.LastLoginAt != nil {
			last = u.LastLoginAt.Format(time.RFC3339)
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\n",
			u.ID, u.Username, u.Role, u.CreatedAt.Format(time.RFC3339), last)
	}
	return w.Flush()
}
