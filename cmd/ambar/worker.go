package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/datcal/ambar/internal/remote"
)

const workerUsage = `Usage:
  ambar worker [--server URL] [--token-file PATH] [--library PATH] [--blender PATH]
               [--concurrency N] [--workdir PATH] [--name NAME]

Takes derive jobs — thumbnails, previews, analysis, Blender conversions — from an ambar
server that has AMBAR_REMOTE_DERIVE=auto or only, does them on this machine, and sends
the results back. The server keeps the database; this machine only needs the library
mounted and an API token with the write scope (ambar token add <user> --scope write).

Each flag falls back to an environment variable:
  --server       AMBAR_WORKER_SERVER, then AMBAR_BASE_URL
  --token-file   AMBAR_WORKER_TOKEN holds the token itself; otherwise
                 <user config dir>/ambar/worker.token
  --library      AMBAR_LOCAL_LIBRARY_PATH (the same library, as this machine sees it)
  --blender      AMBAR_BLENDER_BIN
`

func runWorker(args []string) error {
	fs := flag.NewFlagSet("ambar worker", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, workerUsage) }

	defaultServer := os.Getenv("AMBAR_WORKER_SERVER")
	if defaultServer == "" {
		defaultServer = os.Getenv("AMBAR_BASE_URL")
	}
	configDir, _ := os.UserConfigDir()
	cacheDir, _ := os.UserCacheDir()

	server := fs.String("server", defaultServer, "the ambar server's base URL")
	tokenFile := fs.String("token-file", filepath.Join(configDir, "ambar", "worker.token"),
		"file holding the API token")
	library := fs.String("library", os.Getenv("AMBAR_LOCAL_LIBRARY_PATH"), "this machine's mount of the library")
	blender := fs.String("blender", os.Getenv("AMBAR_BLENDER_BIN"), "Blender executable, for FBX and .blend")
	concurrency := fs.Int("concurrency", max(1, runtime.NumCPU()/2), "jobs at once")
	workDir := fs.String("workdir", filepath.Join(cacheDir, "ambar-worker"),
		"scratch space; must be readable by Blender (not /tmp for a flatpak Blender)")
	name := fs.String("name", "", "this machine's name in the server's logs (default: hostname)")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return errors.New("`ambar worker` takes no positional arguments")
	}

	token := strings.TrimSpace(os.Getenv("AMBAR_WORKER_TOKEN"))
	if token == "" {
		raw, err := os.ReadFile(*tokenFile)
		if err != nil {
			return fmt.Errorf("no AMBAR_WORKER_TOKEN and no token file at %s: %w", *tokenFile, err)
		}
		token = strings.TrimSpace(string(raw))
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	return remote.RunWorker(ctx, remote.WorkerOptions{
		Server:      *server,
		Token:       token,
		LibraryRoot: *library,
		WorkDir:     *workDir,
		BlenderBin:  *blender,
		Concurrency: *concurrency,
		Name:        *name,
		Log:         newLogger(),
	})
}
