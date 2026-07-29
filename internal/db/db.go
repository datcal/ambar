// Package db owns the SQLite connection and the migration runner.
//
// The discipline in spec §4 is implemented here and nowhere else: WAL,
// busy_timeout, foreign keys, and one serialized writer connection alongside a
// separate read pool.
//
// Remember what this database is: a rebuildable index. The filesystem is the
// source of truth (§3), so losing this file must never lose metadata.
package db

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	_ "modernc.org/sqlite" // pure Go, no CGO (invariant 6); FTS5 verified in fts5_test.go
)

// driverName is modernc.org/sqlite's registered name.
const driverName = "sqlite"

// DB holds the two connection pools from §4.
//
// Every INSERT, UPDATE, DELETE and DDL statement goes through Writer, which is
// limited to a single connection so writes serialize in Go rather than
// colliding in SQLite and burning the busy timeout. Reads go through Reader.
type DB struct {
	Writer *sql.DB
	Reader *sql.DB
	Path   string
}

// Open connects to the SQLite file at path, creating it if necessary, and
// applies the §4 pragmas.
//
// It refuses to continue if WAL cannot be enabled. §4 warns that WAL corrupts
// on network filesystems; a database on an SMB or NFS mount typically fails to
// enter WAL mode, and refusing to start is much better than corrupting the
// index later.
func Open(path string) (*DB, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create database directory %s: %w", dir, err)
		}
	}

	writer, err := sql.Open(driverName, writerDSN(path))
	if err != nil {
		return nil, fmt.Errorf("open writer: %w", err)
	}
	// §4: serialize writes through a single connection.
	writer.SetMaxOpenConns(1)
	writer.SetMaxIdleConns(1)
	writer.SetConnMaxLifetime(0)

	if err := writer.Ping(); err != nil {
		writer.Close()
		return nil, fmt.Errorf("connect to %s: %w", path, err)
	}

	var journal string
	if err := writer.QueryRow(`PRAGMA journal_mode`).Scan(&journal); err != nil {
		writer.Close()
		return nil, fmt.Errorf("read journal_mode: %w", err)
	}
	if !strings.EqualFold(journal, "wal") {
		writer.Close()
		return nil, fmt.Errorf(
			"could not enable WAL on %s (journal_mode is %q): the database must sit on a "+
				"real local volume, not a remote SMB/NFS share — see spec §4", path, journal)
	}

	// The writer creates the file and sets the journal mode before any reader
	// connects, so readers never race an empty non-WAL database into existence.
	reader, err := sql.Open(driverName, readerDSN(path))
	if err != nil {
		writer.Close()
		return nil, fmt.Errorf("open reader: %w", err)
	}
	reader.SetMaxOpenConns(max(4, runtime.NumCPU()))
	reader.SetMaxIdleConns(2)
	reader.SetConnMaxLifetime(0)

	if err := reader.Ping(); err != nil {
		writer.Close()
		reader.Close()
		return nil, fmt.Errorf("connect read pool to %s: %w", path, err)
	}

	return &DB{Writer: writer, Reader: reader, Path: path}, nil
}

// Close shuts both pools down. WAL checkpointing on close is SQLite's job.
func (d *DB) Close() error {
	var errs []string
	if d.Reader != nil {
		if err := d.Reader.Close(); err != nil {
			errs = append(errs, "reader: "+err.Error())
		}
	}
	if d.Writer != nil {
		if err := d.Writer.Close(); err != nil {
			errs = append(errs, "writer: "+err.Error())
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("close database: %s", strings.Join(errs, "; "))
	}
	return nil
}

// Ping checks both pools, which is what the health endpoint reports (§12).
func (d *DB) Ping(ctx context.Context) error {
	if err := d.Writer.PingContext(ctx); err != nil {
		return fmt.Errorf("writer: %w", err)
	}
	if err := d.Reader.PingContext(ctx); err != nil {
		return fmt.Errorf("reader: %w", err)
	}
	return nil
}

// writerDSN builds the connection string for the single write connection.
//
// modernc.org/sqlite executes each _pragma= parameter on connect, in order.
// _txlock=immediate makes BEGIN take the write lock straight away instead of
// upgrading mid-transaction, which is what produces spurious SQLITE_BUSY under
// concurrency even with a single writer.
func writerDSN(path string) string {
	return "file:" + url.PathEscape(path) +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(ON)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_txlock=immediate"
}

// readerDSN builds the connection string for the read pool.
//
// query_only makes an accidental write through the read pool an immediate
// error rather than a lock contention mystery — the §4 single-writer rule
// enforced by the database instead of by convention.
func readerDSN(path string) string {
	return "file:" + url.PathEscape(path) +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(ON)" +
		"&_pragma=query_only(true)"
}
