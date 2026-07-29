package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// setMinimalEnv points the two roots at usable temp directories and clears
// everything else, so each test starts from documented defaults rather than
// from whatever the developer's shell happens to export.
func setMinimalEnv(t *testing.T) (libraryRoot, dataRoot string) {
	t.Helper()

	libraryRoot = filepath.Join(t.TempDir(), "library")
	dataRoot = filepath.Join(t.TempDir(), "data")
	for _, dir := range []string{libraryRoot, dataRoot} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	for _, key := range []string{
		"AMBAR_LIBRARY_READONLY", "AMBAR_BIND", "AMBAR_BASE_URL", "AMBAR_TRUSTED_PROXIES",
		"AMBAR_REAL_IP_HEADER", "AMBAR_WORKERS", "AMBAR_MAX_UPLOAD_SIZE",
		"AMBAR_MAX_ARCHIVE_UNCOMPRESSED", "AMBAR_INBOX_POLL_INTERVAL", "AMBAR_BACKUP_INTERVAL",
		"AMBAR_BACKUP_DIR", "AMBAR_BACKUP_KEEP", "AMBAR_TRASH_DIR", "AMBAR_TRASH_RETENTION",
		"AMBAR_DEDUPE_LINK_MODE", "AMBAR_ASEPRITE_BIN", "AMBAR_BLENDER_BIN",
		"AMBAR_SESSION_SECRET", "AMBAR_COOKIE_SECURE",
	} {
		// t.Setenv registers the restore of the original value; os.Unsetenv then
		// removes it entirely. Unset rather than empty matters, because for
		// AMBAR_BACKUP_INTERVAL and AMBAR_TRASH_RETENTION an explicitly empty
		// value means "disabled" and is not the same as absent.
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("AMBAR_LIBRARY_ROOT", libraryRoot)
	t.Setenv("AMBAR_DATA_ROOT", dataRoot)
	return libraryRoot, dataRoot
}

// TestLoadDefaults pins the documented defaults from §13. If one of these
// changes, .env.example and the README have to change with it.
func TestLoadDefaults(t *testing.T) {
	libraryRoot, dataRoot := setMinimalEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.LibraryRoot != libraryRoot {
		t.Errorf("LibraryRoot = %q, want %q", cfg.LibraryRoot, libraryRoot)
	}
	if cfg.Bind != "0.0.0.0:8080" {
		t.Errorf("Bind = %q, want 0.0.0.0:8080", cfg.Bind)
	}
	if cfg.LibraryReadonly {
		t.Error("LibraryReadonly is true by default; §17 says default to read-write")
	}
	if cfg.Workers != 2 {
		t.Errorf("Workers = %d, want 2", cfg.Workers)
	}
	if cfg.MaxUploadSize != 104857600 {
		t.Errorf("MaxUploadSize = %d, want 104857600", cfg.MaxUploadSize)
	}
	if cfg.MaxArchiveUncompressed != 21474836480 {
		t.Errorf("MaxArchiveUncompressed = %d, want 21474836480", cfg.MaxArchiveUncompressed)
	}
	if cfg.InboxPollInterval != 30*time.Second {
		t.Errorf("InboxPollInterval = %s, want 30s", cfg.InboxPollInterval)
	}
	if cfg.BackupInterval != time.Hour {
		t.Errorf("BackupInterval = %s, want 1h", cfg.BackupInterval)
	}
	if cfg.BackupKeep != 48 {
		t.Errorf("BackupKeep = %d, want 48", cfg.BackupKeep)
	}
	if cfg.DedupeLinkMode != "reflink" {
		t.Errorf("DedupeLinkMode = %q, want reflink", cfg.DedupeLinkMode)
	}
	// §13: empty means never auto-purge, which is also the default.
	if cfg.TrashRetention != 0 {
		t.Errorf("TrashRetention = %s, want 0 (never auto-purge)", cfg.TrashRetention)
	}
	// Derived defaults.
	if want := filepath.Join(dataRoot, "backups"); cfg.BackupDir != want {
		t.Errorf("BackupDir = %q, want %q", cfg.BackupDir, want)
	}
	if want := filepath.Join(libraryRoot, "_trash"); cfg.TrashDir != want {
		t.Errorf("TrashDir = %q, want %q", cfg.TrashDir, want)
	}
	if want := filepath.Join(dataRoot, "ambar.db"); cfg.DatabasePath() != want {
		t.Errorf("DatabasePath = %q, want %q", cfg.DatabasePath(), want)
	}
	// §13: empty AMBAR_TRUSTED_PROXIES means ignore forwarded headers entirely.
	if len(cfg.TrustedProxies) != 0 {
		t.Errorf("TrustedProxies = %v, want empty", cfg.TrustedProxies)
	}
}

// TestLoadRejectsBadValues covers every validation branch. A misconfigured
// deployment has to fail at startup, not at the milestone that reads the value.
func TestLoadRejectsBadValues(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		value   string
		wantMsg string
	}{
		{"non-boolean readonly", "AMBAR_LIBRARY_READONLY", "yes-please", "must be a boolean"},
		{"bind without port", "AMBAR_BIND", "0.0.0.0", "host:port"},
		{"base url without scheme", "AMBAR_BASE_URL", "nas:8973", "must start with http"},
		{"base url without host", "AMBAR_BASE_URL", "http://", "has no host"},
		{"bad proxy cidr", "AMBAR_TRUSTED_PROXIES", "10.0.0.0/999", "not a CIDR"},
		{"non-numeric workers", "AMBAR_WORKERS", "many", "must be an integer"},
		{"zero workers", "AMBAR_WORKERS", "0", "at least 1"},
		{"negative workers", "AMBAR_WORKERS", "-1", "at least 1"},
		{"zero upload size", "AMBAR_MAX_UPLOAD_SIZE", "0", "must be positive"},
		{"zero archive cap", "AMBAR_MAX_ARCHIVE_UNCOMPRESSED", "0", "must be positive"},
		{"bad duration", "AMBAR_INBOX_POLL_INTERVAL", "30 seconds", "must be a duration"},
		{"poll too fast", "AMBAR_INBOX_POLL_INTERVAL", "10ms", "at least 1s"},
		{"zero backup keep", "AMBAR_BACKUP_KEEP", "0", "at least 1"},
		{"unknown link mode", "AMBAR_DEDUPE_LINK_MODE", "symlink", "reflink, hardlink or off"},
		{"unknown cookie secure", "AMBAR_COOKIE_SECURE", "maybe", "auto, true or false"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setMinimalEnv(t)
			t.Setenv(tc.key, tc.value)

			_, err := Load()
			if err == nil {
				t.Fatalf("%s=%q was accepted, want an error", tc.key, tc.value)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error %q does not mention %q", err, tc.wantMsg)
			}
			// The message has to name the variable, or the operator cannot act
			// on it.
			if !strings.Contains(err.Error(), tc.key) {
				t.Errorf("error %q does not name %s", err, tc.key)
			}
		})
	}
}

// TestLoadReportsEveryProblemAtOnce: fixing a .env one error per restart is
// miserable, so errors are collected.
func TestLoadReportsEveryProblemAtOnce(t *testing.T) {
	setMinimalEnv(t)
	t.Setenv("AMBAR_WORKERS", "0")
	t.Setenv("AMBAR_BACKUP_KEEP", "0")
	t.Setenv("AMBAR_DEDUPE_LINK_MODE", "symlink")

	_, err := Load()
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, key := range []string{"AMBAR_WORKERS", "AMBAR_BACKUP_KEEP", "AMBAR_DEDUPE_LINK_MODE"} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("combined error does not mention %s:\n%v", key, err)
		}
	}
}

// TestMissingRootsFailLoudly is §17's "fail loudly at startup rather than
// limping along".
func TestMissingRootsFailLoudly(t *testing.T) {
	t.Run("data root absent", func(t *testing.T) {
		setMinimalEnv(t)
		t.Setenv("AMBAR_DATA_ROOT", filepath.Join(t.TempDir(), "nope"))

		_, err := Load()
		if err == nil {
			t.Fatal("a missing data root was accepted")
		}
		if !strings.Contains(err.Error(), "does not exist") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("library root absent", func(t *testing.T) {
		setMinimalEnv(t)
		t.Setenv("AMBAR_LIBRARY_ROOT", filepath.Join(t.TempDir(), "nope"))

		_, err := Load()
		if err == nil {
			t.Fatal("a missing library root was accepted")
		}
		if !strings.Contains(err.Error(), "AMBAR_LIBRARY_ROOT") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("data root is a file", func(t *testing.T) {
		setMinimalEnv(t)
		file := filepath.Join(t.TempDir(), "afile")
		if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("AMBAR_DATA_ROOT", file)

		_, err := Load()
		if err == nil || !strings.Contains(err.Error(), "not a directory") {
			t.Errorf("expected a not-a-directory error, got %v", err)
		}
	})
}

// TestUnwritableDataRootFails is the uid/gid mismatch from §17 — the failure
// this probe exists to catch.
func TestUnwritableDataRootFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root, which can write to a 0500 directory")
	}
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits")
	}

	setMinimalEnv(t)
	readOnly := filepath.Join(t.TempDir(), "readonly")
	if err := os.Mkdir(readOnly, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AMBAR_DATA_ROOT", readOnly)

	_, err := Load()
	if err == nil {
		t.Fatal("an unwritable data root was accepted")
	}
	if !strings.Contains(err.Error(), "not writable") {
		t.Errorf("error should say the directory is not writable: %v", err)
	}
	// The message must point at the cause, since this is the single most likely
	// first-run failure on a Synology.
	if !strings.Contains(err.Error(), "§17") {
		t.Errorf("error should reference the §17 uid/gid note: %v", err)
	}
}

// TestReadonlyLibraryNeedsOnlyRead: §3's escape hatch has to actually work on a
// read-only mount.
func TestReadonlyLibraryNeedsOnlyRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root")
	}

	setMinimalEnv(t)
	readOnly := filepath.Join(t.TempDir(), "library-ro")
	if err := os.Mkdir(readOnly, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AMBAR_LIBRARY_ROOT", readOnly)

	// Read-write mode must reject it...
	if _, err := Load(); err == nil {
		t.Error("an unwritable library was accepted in read-write mode")
	}

	// ...and read-only mode must accept it.
	t.Setenv("AMBAR_LIBRARY_READONLY", "true")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("read-only mode rejected a readable library: %v", err)
	}
	if !cfg.LibraryReadonly {
		t.Error("LibraryReadonly did not take effect")
	}
}

// TestCookieSecureResolution documents the deviation from §11 in a test, so a
// future change to it is deliberate.
func TestCookieSecureResolution(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		mode    string
		want    bool
	}{
		{"auto with https", "https://nas.tailnet.ts.net", "", true},
		{"auto with http", "http://nas:8973", "", false},
		{"forced on over http", "http://nas:8973", "true", true},
		{"forced off over https", "https://nas.tailnet.ts.net", "false", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setMinimalEnv(t)
			t.Setenv("AMBAR_BASE_URL", tc.baseURL)
			if tc.mode != "" {
				t.Setenv("AMBAR_COOKIE_SECURE", tc.mode)
			}

			cfg, err := Load()
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if cfg.CookieSecure != tc.want {
				t.Errorf("CookieSecure = %v, want %v (base %s, mode %q)",
					cfg.CookieSecure, tc.want, tc.baseURL, tc.mode)
			}
		})
	}
}

func TestTrustedProxyParsing(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  []string
	}{
		{"empty", "", nil},
		{"single cidr", "10.0.0.0/8", []string{"10.0.0.0/8"}},
		{"multiple with spaces", "10.0.0.0/8, 172.16.0.0/12", []string{"10.0.0.0/8", "172.16.0.0/12"}},
		// A bare address is the obvious mistake to forgive.
		{"bare ipv4", "192.168.1.5", []string{"192.168.1.5/32"}},
		{"bare ipv6", "2001:db8::1", []string{"2001:db8::1/128"}},
		{"ipv6 cidr", "2001:db8::/32", []string{"2001:db8::/32"}},
		// Unmasked host bits are normalised rather than rejected.
		{"unmasked cidr", "10.1.2.3/8", []string{"10.0.0.0/8"}},
		{"trailing comma", "10.0.0.0/8,", []string{"10.0.0.0/8"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setMinimalEnv(t)
			t.Setenv("AMBAR_TRUSTED_PROXIES", tc.value)

			cfg, err := Load()
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if len(cfg.TrustedProxies) != len(tc.want) {
				t.Fatalf("parsed %v, want %v", cfg.TrustedProxies, tc.want)
			}
			for i, want := range tc.want {
				if got := cfg.TrustedProxies[i].String(); got != want {
					t.Errorf("prefix %d = %s, want %s", i, got, want)
				}
			}
		})
	}
}

// TestEmptyDurationDisables covers the two variables §13 documents as
// "empty disables this". An explicitly empty value must not fall back to the
// default.
func TestEmptyDurationDisables(t *testing.T) {
	setMinimalEnv(t)
	t.Setenv("AMBAR_BACKUP_INTERVAL", "")
	t.Setenv("AMBAR_TRASH_RETENTION", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.BackupInterval != 0 {
		t.Errorf("BackupInterval = %s, want 0 (scheduler disabled)", cfg.BackupInterval)
	}
	if cfg.TrashRetention != 0 {
		t.Errorf("TrashRetention = %s, want 0 (never auto-purge)", cfg.TrashRetention)
	}
}

func TestSessionSecretFromEnv(t *testing.T) {
	_, dataRoot := setMinimalEnv(t)
	t.Setenv("AMBAR_SESSION_SECRET", "a-secret-from-the-environment")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.SessionSecretSource != "env" {
		t.Errorf("SessionSecretSource = %q, want env", cfg.SessionSecretSource)
	}
	if string(cfg.SessionSecret) != "a-secret-from-the-environment" {
		t.Errorf("SessionSecret = %q", cfg.SessionSecret)
	}
	// Nothing should have been written when the environment supplied it.
	if _, err := os.Stat(filepath.Join(dataRoot, sessionSecretFile)); !os.IsNotExist(err) {
		t.Error("a secret file was created even though AMBAR_SESSION_SECRET was set")
	}
}

// TestSessionSecretGeneratedThenReused is §17: generate on first run, persist to
// /data, and keep using it — otherwise every restart logs everyone out.
func TestSessionSecretGeneratedThenReused(t *testing.T) {
	_, dataRoot := setMinimalEnv(t)

	first, err := Load()
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	if first.SessionSecretSource != "generated" {
		t.Errorf("first load source = %q, want generated", first.SessionSecretSource)
	}
	if len(first.SessionSecret) != SessionSecretLen {
		t.Errorf("generated secret is %d bytes, want %d", len(first.SessionSecret), SessionSecretLen)
	}

	path := filepath.Join(dataRoot, sessionSecretFile)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("secret file was not created: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("secret file mode is %o, want 600", perm)
	}

	second, err := Load()
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if second.SessionSecretSource != "file" {
		t.Errorf("second load source = %q, want file", second.SessionSecretSource)
	}
	if string(second.SessionSecret) != string(first.SessionSecret) {
		t.Error("the secret changed between runs, which would invalidate every CSRF token")
	}
}

func TestCorruptSessionSecretFileIsAnError(t *testing.T) {
	_, dataRoot := setMinimalEnv(t)
	if err := os.WriteFile(filepath.Join(dataRoot, sessionSecretFile), []byte("not-hex!!"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Load()
	if err == nil {
		t.Fatal("a corrupt secret file was accepted")
	}
	// The message has to say what to do about it.
	if !strings.Contains(err.Error(), "delete it") {
		t.Errorf("error should explain the fix: %v", err)
	}
}

func TestRelativeRootsAreMadeAbsolute(t *testing.T) {
	setMinimalEnv(t)

	// A relative root would otherwise resolve against the process working
	// directory, which for a container is a footgun.
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.MkdirAll(filepath.Join(dir, "rel-library"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AMBAR_LIBRARY_ROOT", "rel-library")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !filepath.IsAbs(cfg.LibraryRoot) {
		t.Errorf("LibraryRoot = %q, want an absolute path", cfg.LibraryRoot)
	}
}
