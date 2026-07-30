package main

import (
	"context"
	"fmt"
	"os"

	"github.com/datcal/ambar/internal/sidecar"
)

const sidecarUsage = `Usage:
  ambar sidecar sync      write a .ambar.json for every pack from the index
  ambar sidecar import    import metadata from sidecars where the index lacks it

Sidecars (.ambar.json) are what make the database disposable (§3): they carry a
pack's provenance and manual tags beside the files, so a rebuilt index — or a
folder copied to another machine — recovers its metadata. On a read-only library
they live in $AMBAR_DATA_ROOT/sidecars/ instead.
`

func runSidecar(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, sidecarUsage)
		return errUsage
	}

	log := newLogger()
	cfg, database, err := openDatabase(context.Background())
	if err != nil {
		return err
	}
	defer database.Close() //nolint:errcheck

	mgr := sidecar.New(database, sidecar.Options{
		LibraryRoot: cfg.LibraryRoot,
		DataRoot:    cfg.DataRoot,
		Readonly:    cfg.LibraryReadonly,
		Log:         log,
	})
	ctx := context.Background()

	switch args[0] {
	case "sync":
		if cfg.LibraryReadonly {
			fmt.Println("read-only library: writing sidecars to the data-root mirror")
		}
		n, err := mgr.SyncAll(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("wrote %d sidecar(s)\n", n)
		return nil
	case "import":
		n, err := mgr.ImportAll(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("imported metadata for %d pack(s) from sidecars\n", n)
		return nil
	default:
		fmt.Fprint(os.Stderr, sidecarUsage)
		return fmt.Errorf("unknown sidecar subcommand %q", args[0])
	}
}
