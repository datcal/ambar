// Command ambar is the single binary: HTTP server and CLI in one.
//
// Subcommands are dispatched by hand with the stdlib flag package. There is no
// CLI framework because there are three subcommands, and §2's "prefer stdlib"
// applies here as much as to the HTTP layer.
package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"strings"
)

// Stamped by the Makefile via -ldflags. "dev" means somebody ran `go build`
// directly, which is fine but worth being able to see in the health endpoint.
var (
	version = "dev"
	commit  = "unknown"
)

const usage = `ambar — self-hosted game asset library

Usage:
  ambar serve                       run the HTTP server
  ambar scan [--dry-run]            index the library
  ambar user add <username>         create a user (there is no self-registration)
  ambar user list                   list users
  ambar version                     print version information

Configuration is entirely by environment variable; see .env.example.
`

func main() {
	// Exit code handling lives in run() so deferred cleanup actually runs.
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, errUsage) {
			fmt.Fprint(os.Stderr, usage)
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "ambar: %v\n", err)
		os.Exit(1)
	}
}

var errUsage = errors.New("usage")

func run(args []string) error {
	if len(args) == 0 {
		return errUsage
	}

	switch args[0] {
	case "serve":
		return runServe(args[1:])
	case "scan":
		return runScan(args[1:])
	case "user":
		return runUser(args[1:])
	case "version", "--version", "-version":
		fmt.Printf("ambar %s (commit %s, %s, %s/%s)\n",
			version, commit, runtime.Version(), runtime.GOOS, runtime.GOARCH)
		return nil
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	default:
		return fmt.Errorf("unknown command %q\n\n%s", args[0], strings.TrimSuffix(usage, "\n"))
	}
}

// newLogger builds the structured logger (§12). Text by default because a human
// reads `docker logs`; JSON when AMBAR_LOG_FORMAT=json for a log shipper.
func newLogger() *slog.Logger {
	level := slog.LevelInfo
	if v := strings.ToLower(os.Getenv("AMBAR_LOG_LEVEL")); v != "" {
		switch v {
		case "debug":
			level = slog.LevelDebug
		case "info":
			level = slog.LevelInfo
		case "warn", "warning":
			level = slog.LevelWarn
		case "error":
			level = slog.LevelError
		}
	}
	opts := &slog.HandlerOptions{Level: level}

	var handler slog.Handler = slog.NewTextHandler(os.Stderr, opts)
	if strings.EqualFold(os.Getenv("AMBAR_LOG_FORMAT"), "json") {
		handler = slog.NewJSONHandler(os.Stderr, opts)
	}
	return slog.New(handler)
}
