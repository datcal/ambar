package db

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// openTestDB returns a migrated database in a temp directory. Shared with
// fts5_test.go.
func openTestDB(t *testing.T) *DB {
	t.Helper()

	// An on-disk file, not :memory: — WAL, the busy timeout and the two-pool
	// arrangement only behave realistically against a real file.
	d, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})
	if _, err := d.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return d
}

// TestPragmasAreInEffect reads the pragmas back rather than trusting the DSN.
// A typo in a _pragma= parameter is silently ignored by the driver, so the DSN
// is not evidence of anything.
func TestPragmasAreInEffect(t *testing.T) {
	d := openTestDB(t)

	writerWant := map[string]string{
		"journal_mode": "wal",
		"busy_timeout": "5000",
		"foreign_keys": "1",
		"synchronous":  "1", // NORMAL
	}
	for pragma, want := range writerWant {
		var got string
		if err := d.Writer.QueryRow(`PRAGMA ` + pragma).Scan(&got); err != nil {
			t.Errorf("writer PRAGMA %s: %v", pragma, err)
			continue
		}
		if !strings.EqualFold(got, want) {
			t.Errorf("writer PRAGMA %s = %q, want %q", pragma, got, want)
		}
	}

	// The read pool needs the busy timeout and foreign keys too; journal_mode is
	// a property of the file, so it is already WAL here.
	readerWant := map[string]string{
		"busy_timeout": "5000",
		"foreign_keys": "1",
		"query_only":   "1",
	}
	for pragma, want := range readerWant {
		var got string
		if err := d.Reader.QueryRow(`PRAGMA ` + pragma).Scan(&got); err != nil {
			t.Errorf("reader PRAGMA %s: %v", pragma, err)
			continue
		}
		if !strings.EqualFold(got, want) {
			t.Errorf("reader PRAGMA %s = %q, want %q", pragma, got, want)
		}
	}
}

// TestWriterIsSingleConnection pins the §4 rule that writes serialize through
// one connection.
func TestWriterIsSingleConnection(t *testing.T) {
	d := openTestDB(t)
	if got := d.Writer.Stats().MaxOpenConnections; got != 1 {
		t.Errorf("writer MaxOpenConnections = %d, want 1 (§4: serialize writes)", got)
	}
}

// TestReadPoolRejectsWrites checks that query_only turns an accidental write
// through the read pool into an immediate error, instead of a lock-contention
// mystery months from now.
func TestReadPoolRejectsWrites(t *testing.T) {
	d := openTestDB(t)

	_, err := d.Reader.ExecContext(context.Background(), `
		INSERT INTO users (username, password_hash, role, created_at, updated_at)
		VALUES ('nope', 'x', 'user', 0, 0)`)
	if err == nil {
		t.Fatal("a write through the read pool succeeded; query_only is not in effect")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "readonly") {
		t.Errorf("write through read pool failed with an unexpected error: %v", err)
	}
}

func TestMigrationsAreIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")

	d, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()

	first, err := d.Migrate(ctx)
	if err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	if len(first) == 0 {
		t.Fatal("first migrate applied nothing")
	}

	second, err := d.Migrate(ctx)
	if err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if len(second) != 0 {
		t.Errorf("second migrate applied %v, want nothing", second)
	}

	version, err := d.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("schema version: %v", err)
	}
	if want := first[len(first)-1]; version != want {
		t.Errorf("SchemaVersion = %q, want %q", version, want)
	}
}

// TestMigrationsCreateOnlyShippedTables guards the scope policy: a milestone
// creates only the tables it populates, because a column with no writer is a
// column with no meaning.
//
// Asserted as "these exist" plus "these do not" rather than as an exact list,
// because FTS5 also creates shadow tables (assets_fts_data, assets_fts_idx, ...)
// whose names are an implementation detail of the SQLite version.
func TestMigrationsCreateOnlyShippedTables(t *testing.T) {
	d := openTestDB(t)

	exists := func(name string) bool {
		t.Helper()
		var n int
		if err := d.Reader.QueryRow(
			`SELECT count(*) FROM sqlite_master WHERE type IN ('table','view') AND name = ?`,
			name).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n > 0
	}

	// M0 (0001_init), M1 (0002_library), M2 (0003_derive) and M3 (0004_tags).
	for _, name := range []string{
		"schema_migrations", "users", "sessions", "audit_log",
		"packs", "assets", "assets_fts",
		"jobs", "asset_groups",
		"tags", "tag_closure", "tag_aliases", "asset_tags", "pack_tags",
		"saved_searches", "licenses", "api_tokens",
		"projects", "project_uses",
	} {
		if !exists(name) {
			t.Errorf("table %q is missing", name)
		}
	}
	// Every §4 table now ships. New future-milestone tables, if any are sketched
	// later, would be asserted absent here.
}

// TestForeignKeysEnforced proves foreign_keys=ON is doing something, since a
// pragma that silently failed to apply looks identical to one that worked.
func TestForeignKeysEnforced(t *testing.T) {
	d := openTestDB(t)

	_, err := d.Writer.ExecContext(context.Background(), `
		INSERT INTO sessions (user_id, token_hash, created_at, expires_at, idle_expires_at)
		VALUES (99999, X'00', 0, 0, 0)`)
	if err == nil {
		t.Fatal("inserted a session for a nonexistent user; foreign keys are not enforced")
	}
	if !strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestCascadeDeleteRemovesSessions confirms the ON DELETE CASCADE in the schema,
// so removing a user cannot leave usable sessions behind.
func TestCascadeDeleteRemovesSessions(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	if _, err := d.Writer.ExecContext(ctx, `
		INSERT INTO users (id, username, password_hash, role, created_at, updated_at)
		VALUES (1, 'alice', 'x', 'user', 0, 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Writer.ExecContext(ctx, `
		INSERT INTO sessions (user_id, token_hash, created_at, expires_at, idle_expires_at)
		VALUES (1, X'01', 0, 0, 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Writer.ExecContext(ctx, `DELETE FROM users WHERE id = 1`); err != nil {
		t.Fatal(err)
	}

	var n int
	if err := d.Reader.QueryRowContext(ctx, `SELECT count(*) FROM sessions`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d sessions survived the user being deleted, want 0", n)
	}
}

// TestAuditLogSurvivesUserDeletion is the opposite case, and deliberate: §11
// wants an audit trail, so deleting a user must not erase what they did.
func TestAuditLogSurvivesUserDeletion(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	if _, err := d.Writer.ExecContext(ctx, `
		INSERT INTO users (id, username, password_hash, role, created_at, updated_at)
		VALUES (1, 'alice', 'x', 'user', 0, 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Writer.ExecContext(ctx, `
		INSERT INTO audit_log (user_id, action, at) VALUES (1, 'login.succeeded', 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Writer.ExecContext(ctx, `DELETE FROM users WHERE id = 1`); err != nil {
		t.Fatal(err)
	}

	var (
		n      int
		userID *int64
	)
	if err := d.Reader.QueryRowContext(ctx, `SELECT count(*) FROM audit_log`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("%d audit rows survived, want 1", n)
	}
	if err := d.Reader.QueryRowContext(ctx, `SELECT user_id FROM audit_log`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if userID != nil {
		t.Errorf("audit_log.user_id = %v after the user was deleted, want NULL", *userID)
	}
}

// TestConcurrentWritesSerialize checks the arrangement that makes SQLITE_BUSY a
// non-event: many goroutines writing through the single-connection writer pool.
func TestConcurrentWritesSerialize(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	if _, err := d.Writer.ExecContext(ctx, `
		INSERT INTO users (id, username, password_hash, role, created_at, updated_at)
		VALUES (1, 'alice', 'x', 'user', 0, 0)`); err != nil {
		t.Fatal(err)
	}

	const goroutines = 8
	const perGoroutine = 20

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				_, err := d.Writer.ExecContext(ctx,
					`INSERT INTO audit_log (user_id, action, entity_id, at) VALUES (1, 'test', ?, 0)`,
					g*perGoroutine+i)
				if err != nil {
					mu.Lock()
					errs = append(errs, err)
					mu.Unlock()
					return
				}
			}
		}(g)
	}
	wg.Wait()

	for _, err := range errs {
		t.Errorf("concurrent write failed: %v", err)
	}

	var n int
	if err := d.Reader.QueryRowContext(ctx, `SELECT count(*) FROM audit_log`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if want := goroutines * perGoroutine; n != want {
		t.Errorf("wrote %d rows, want %d", n, want)
	}
}

// TestStrictTablesRejectWrongTypes confirms the STRICT keyword is doing its job
// on the tables that hold credentials.
func TestStrictTablesRejectWrongTypes(t *testing.T) {
	d := openTestDB(t)

	_, err := d.Writer.ExecContext(context.Background(), `
		INSERT INTO users (username, password_hash, role, created_at, updated_at)
		VALUES ('alice', 'x', 'user', 'not-a-number', 0)`)
	if err == nil {
		t.Fatal("a TEXT value was accepted for an INTEGER column; the table is not STRICT")
	}
}

func TestOpenCreatesMissingDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "deeper", "ambar.db")
	d, err := Open(path)
	if err != nil {
		t.Fatalf("open into a missing directory: %v", err)
	}
	defer d.Close()

	if _, err := d.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
}
