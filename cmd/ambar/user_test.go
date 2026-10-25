package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/datcal/ambar/internal/auth"
)

// TestParseFlagsAcceptsFlagsAfterPositionals is a regression test.
//
// A plain flag.FlagSet.Parse stops at the first non-flag argument, so
// `user add alice --password-stdin` read "--password-stdin" as a second
// username and refused to create the user. That is the exact form the README and
// docker-compose.yml document for `docker exec -i`, so it silently broke the
// documented first-run path.
func TestParseFlagsAcceptsFlagsAfterPositionals(t *testing.T) {
	tests := []struct {
		name           string
		args           []string
		wantPositional []string
		wantRole       string
		wantStdin      bool
	}{
		{
			name:           "bare username",
			args:           []string{"alice"},
			wantPositional: []string{"alice"},
			wantRole:       "user",
		},
		{
			// The case that was broken.
			name:           "flag after the username",
			args:           []string{"alice", "--password-stdin"},
			wantPositional: []string{"alice"},
			wantRole:       "user",
			wantStdin:      true,
		},
		{
			name:           "single-dash flag after the username",
			args:           []string{"alice", "-password-stdin"},
			wantPositional: []string{"alice"},
			wantRole:       "user",
			wantStdin:      true,
		},
		{
			name:           "flag before the username",
			args:           []string{"--password-stdin", "alice"},
			wantPositional: []string{"alice"},
			wantRole:       "user",
			wantStdin:      true,
		},
		{
			name:           "valued flag after the username, space separated",
			args:           []string{"alice", "--role", "user"},
			wantPositional: []string{"alice"},
			wantRole:       "user",
		},
		{
			name:           "valued flag after the username, equals separated",
			args:           []string{"alice", "--role=user"},
			wantPositional: []string{"alice"},
			wantRole:       "user",
		},
		{
			name:           "flags on both sides",
			args:           []string{"--role=user", "alice", "--password-stdin"},
			wantPositional: []string{"alice"},
			wantRole:       "user",
			wantStdin:      true,
		},
		{
			name:           "no arguments at all",
			args:           nil,
			wantPositional: nil,
			wantRole:       "user",
		},
		{
			// Must be detectable as wrong, not silently take the first.
			name:           "two usernames",
			args:           []string{"alice", "bob"},
			wantPositional: []string{"alice", "bob"},
			wantRole:       "user",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			fs.SetOutput(&strings.Builder{})
			role := fs.String("role", "user", "")
			stdin := fs.Bool("password-stdin", false, "")

			positional, err := parseFlags(fs, tc.args)
			if err != nil {
				t.Fatalf("parseFlags(%v): %v", tc.args, err)
			}

			if strings.Join(positional, ",") != strings.Join(tc.wantPositional, ",") {
				t.Errorf("positional = %v, want %v", positional, tc.wantPositional)
			}
			if *role != tc.wantRole {
				t.Errorf("role = %q, want %q", *role, tc.wantRole)
			}
			if *stdin != tc.wantStdin {
				t.Errorf("password-stdin = %v, want %v", *stdin, tc.wantStdin)
			}
		})
	}
}

func TestParseFlagsRejectsUnknownFlags(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(&strings.Builder{})
	fs.Bool("password-stdin", false, "")

	if _, err := parseFlags(fs, []string{"alice", "--nonsense"}); err == nil {
		t.Error("an unknown flag was accepted")
	}
}

func TestCheckPasswordLength(t *testing.T) {
	tests := []struct {
		password string
		valid    bool
	}{
		{"", false},
		{"short", false},
		{strings.Repeat("x", 11), false},
		{strings.Repeat("x", 12), true},
		{"a-long-enough-password", true},
		// Counted in runes, not bytes, so a short multi-byte password is still
		// short rather than accidentally passing on byte length.
		{strings.Repeat("é", 11), false},
		{strings.Repeat("é", 12), true},
	}
	for _, tc := range tests {
		err := checkPasswordLength(tc.password)
		if tc.valid && err != nil {
			t.Errorf("password of %d runes was rejected: %v", len([]rune(tc.password)), err)
		}
		if !tc.valid && err == nil {
			t.Errorf("password of %d runes was accepted", len([]rune(tc.password)))
		}
	}
}

// TestRunRejectsUnknownCommands keeps the dispatch honest.
func TestRunRejectsUnknownCommands(t *testing.T) {
	if err := run(nil); err == nil {
		t.Error("no arguments should be a usage error")
	}
	if err := run([]string{"definitely-not-a-command"}); err == nil {
		t.Error("an unknown command was accepted")
	}
	// version and help must not need a database or a valid configuration.
	if err := run([]string{"version"}); err != nil {
		t.Errorf("version failed: %v", err)
	}
	if err := run([]string{"help"}); err != nil {
		t.Errorf("help failed: %v", err)
	}
}

// TestUserPasswdNeedsOneUsername: argument mistakes are caught before the database
// is opened, so they do not depend on a configuration.
func TestUserPasswdNeedsOneUsername(t *testing.T) {
	for _, args := range [][]string{{"passwd"}, {"passwd", "alice", "bob"}} {
		err := runUser(args)
		if err == nil || !strings.Contains(err.Error(), "exactly one username") {
			t.Errorf("runUser(%q) = %v, want an \"exactly one username\" error", args, err)
		}
	}
}

// TestUserPasswdResetsAndSignsOut runs the command against a real database: the
// password is replaced and every session of that user ends, since a reset usually
// means the old password is lost or known to someone else.
func TestUserPasswdResetsAndSignsOut(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"library", "data"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("AMBAR_LIBRARY_ROOT", filepath.Join(root, "library"))
	t.Setenv("AMBAR_DATA_ROOT", filepath.Join(root, "data"))

	ctx := context.Background()
	_, database, err := openDatabase(ctx)
	if err != nil {
		t.Fatal(err)
	}
	alice, err := auth.NewUserStore(database).Create(ctx, "alice", "the-forgotten-password", auth.RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := auth.NewSessionStore(database).Create(ctx, alice.ID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	database.Close()

	// A name that does not exist is refused before any password is read.
	if err := runUser([]string{"passwd", "nobody"}); err == nil || !strings.Contains(err.Error(), "no user") {
		t.Errorf("passwd for a missing user = %v, want a \"no user\" error", err)
	}

	withStdin(t, "a-freshly-reset-password\n")
	if err := runUser([]string{"passwd", "Alice", "--password-stdin"}); err != nil {
		t.Fatalf("passwd: %v", err)
	}

	_, database, err = openDatabase(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	u, err := auth.NewUserStore(database).ByID(ctx, alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := auth.VerifyPassword(u.PasswordHash, "a-freshly-reset-password"); err != nil || !ok {
		t.Errorf("the reset password does not verify (ok=%v, err=%v)", ok, err)
	}
	if _, _, err := auth.NewSessionStore(database).Lookup(ctx, token); !errors.Is(err, auth.ErrNoSession) {
		t.Errorf("alice's session survived the reset: %v", err)
	}
}

// withStdin makes os.Stdin read input for the rest of the test.
func withStdin(t *testing.T, input string) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(input); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = f
	t.Cleanup(func() {
		os.Stdin = old
		f.Close()
	})
}
