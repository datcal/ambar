package main

import (
	"flag"
	"strings"
	"testing"
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
