package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Migrations are forward-only and numbered. There are deliberately no down
// migrations: the database is a rebuildable index (§3), so the recovery path
// for a bad migration is to fix it forward or to run `ambar rebuild-index`,
// never to unwind schema changes against live data.
const migrationDir = "migrations"

// Migrate applies every pending migration and returns the versions it applied.
// Running it again applies nothing.
func (d *DB) Migrate(ctx context.Context) ([]string, error) {
	if _, err := d.Writer.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    TEXT    NOT NULL PRIMARY KEY,
			applied_at INTEGER NOT NULL
		) STRICT`); err != nil {
		return nil, fmt.Errorf("create schema_migrations: %w", err)
	}

	applied, err := d.appliedVersions(ctx)
	if err != nil {
		return nil, err
	}

	files, err := migrationFiles()
	if err != nil {
		return nil, err
	}

	var ran []string
	for _, name := range files {
		version := strings.TrimSuffix(name, ".sql")
		if applied[version] {
			continue
		}
		body, err := migrationFS.ReadFile(migrationDir + "/" + name)
		if err != nil {
			return ran, fmt.Errorf("read migration %s: %w", name, err)
		}
		if err := d.applyOne(ctx, version, string(body)); err != nil {
			return ran, err
		}
		ran = append(ran, version)
	}
	return ran, nil
}

// applyOne runs a single migration and records it, both inside one
// transaction, so a failure halfway through leaves no partial schema.
func (d *DB) applyOne(ctx context.Context, version, body string) error {
	tx, err := d.Writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration %s: %w", version, err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed

	// modernc.org/sqlite executes a multi-statement string one statement at a
	// time, so a whole .sql file can be passed straight through. It parses
	// CREATE TRIGGER ... BEGIN ... END correctly, which naive splitting on ";"
	// would not.
	if _, err := tx.ExecContext(ctx, body); err != nil {
		return fmt.Errorf("apply migration %s: %w", version, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
		version, time.Now().Unix(),
	); err != nil {
		return fmt.Errorf("record migration %s: %w", version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration %s: %w", version, err)
	}
	return nil
}

func (d *DB) appliedVersions(ctx context.Context) (map[string]bool, error) {
	rows, err := d.Writer.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer rows.Close()

	applied := map[string]bool{}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		applied[v] = true
	}
	return applied, rows.Err()
}

// migrationFiles returns the embedded migration filenames in lexical order,
// which is why they are zero-padded.
func migrationFiles() ([]string, error) {
	entries, err := fs.ReadDir(migrationFS, migrationDir)
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return nil, fmt.Errorf("no migrations embedded from %s", migrationDir)
	}
	return names, nil
}

// SchemaVersion returns the most recently applied migration, or "" if none.
// Reported by the health endpoint so a container running an old image is
// visible.
func (d *DB) SchemaVersion(ctx context.Context) (string, error) {
	var v string
	err := d.Reader.QueryRowContext(ctx,
		`SELECT version FROM schema_migrations ORDER BY version DESC LIMIT 1`).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}
