// Package config loads and validates the AMBAR_* environment settings from
// spec §13.
//
// Every documented variable is parsed here, including the ones no code reads
// yet. A typo in a deployed .env should fail at startup with a clear message,
// not silently do nothing until the milestone that finally consumes the value.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Where the session secret is persisted when it is not supplied by the
// environment (§17: generating it beats asking the operator to invent one).
const sessionSecretFile = "session_secret"

// SessionSecretLen is the length in bytes of a generated session secret.
const SessionSecretLen = 32

// Config is the fully validated configuration. Construct it with Load.
type Config struct {
	// --- Paths and mode (§3, §17) ---

	LibraryRoot     string
	DataRoot        string
	LibraryReadonly bool

	// --- HTTP (§2) ---

	Bind    string
	BaseURL *url.URL

	// --- Client-IP trust (§2, §11) ---

	// TrustedProxies is empty by default, which means forwarded-IP headers are
	// ignored entirely and the socket peer address is used.
	TrustedProxies []netip.Prefix
	RealIPHeader   string

	// --- Cookies ---

	// CookieSecure controls the Secure attribute on the session cookie.
	//
	// §11 mandates Secure unconditionally, but §8 documents plain-HTTP LAN
	// access as a real deployment path, and a Secure cookie is never sent over
	// plain HTTP — so an unconditional flag would make LAN login impossible.
	// This is therefore derived from BaseURL's scheme and can be forced with
	// AMBAR_COOKIE_SECURE. A documented deviation, not an oversight.
	CookieSecure bool

	// --- Secrets ---

	// SessionSecret is the HMAC key for CSRF tokens. Session tokens themselves
	// are opaque random values stored as hashes and need no secret.
	SessionSecret       []byte
	SessionSecretSource string // "env", "file" or "generated"

	// --- Parsed but not yet read ---

	Workers int // M2, worker pool

	MaxUploadSize          int64         // M4, web upload cap
	MaxArchiveUncompressed int64         // M4, zip-bomb defence
	InboxPollInterval      time.Duration // M4, _inbox polling

	BackupInterval time.Duration // M11, zero disables the internal scheduler
	BackupDir      string        // M11
	BackupKeep     int           // M11

	TrashDir       string        // M13
	TrashRetention time.Duration // M13, zero means never auto-purge
	DedupeLinkMode string        // M13, reflink | hardlink | off

	AsepriteBin string // M2, optional external binary
	BlenderBin  string // M6, optional external binary
}

// DatabasePath is the SQLite file, deliberately outside the library tree (§4).
func (c *Config) DatabasePath() string {
	return filepath.Join(c.DataRoot, "ambar.db")
}

// Load reads the environment, validates it, and resolves the session secret.
//
// Validation errors are collected rather than returned one at a time, so a
// misconfigured deployment reports everything wrong in a single startup log.
func Load() (*Config, error) {
	c := &Config{
		LibraryRoot:            envStr("AMBAR_LIBRARY_ROOT", "/library"),
		DataRoot:               envStr("AMBAR_DATA_ROOT", "/data"),
		Bind:                   envStr("AMBAR_BIND", "0.0.0.0:8080"),
		RealIPHeader:           strings.TrimSpace(os.Getenv("AMBAR_REAL_IP_HEADER")),
		BackupDir:              envStr("AMBAR_BACKUP_DIR", ""),
		TrashDir:               envStr("AMBAR_TRASH_DIR", ""),
		AsepriteBin:            strings.TrimSpace(os.Getenv("AMBAR_ASEPRITE_BIN")),
		BlenderBin:             strings.TrimSpace(os.Getenv("AMBAR_BLENDER_BIN")),
		MaxUploadSize:          0,
		MaxArchiveUncompressed: 0,
	}

	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	// Absolute paths only: relative roots resolve against whatever directory the
	// process happens to start in, which for a container is a footgun.
	c.LibraryRoot = mustAbs(c.LibraryRoot, "AMBAR_LIBRARY_ROOT", fail)
	c.DataRoot = mustAbs(c.DataRoot, "AMBAR_DATA_ROOT", fail)

	// Defaults that depend on other values, so they cannot sit in the literal above.
	if c.BackupDir == "" {
		c.BackupDir = filepath.Join(c.DataRoot, "backups")
	} else {
		c.BackupDir = mustAbs(c.BackupDir, "AMBAR_BACKUP_DIR", fail)
	}
	if c.TrashDir == "" {
		c.TrashDir = filepath.Join(c.LibraryRoot, "_trash")
	} else {
		c.TrashDir = mustAbs(c.TrashDir, "AMBAR_TRASH_DIR", fail)
	}

	var err error

	if c.LibraryReadonly, err = envBool("AMBAR_LIBRARY_READONLY", false); err != nil {
		fail("%w", err)
	}

	if _, _, err := net.SplitHostPort(c.Bind); err != nil {
		fail("AMBAR_BIND %q is not host:port: %w", c.Bind, err)
	}

	rawBase := envStr("AMBAR_BASE_URL", "http://localhost:8080")
	if c.BaseURL, err = parseBaseURL(rawBase); err != nil {
		fail("%w", err)
	}

	if c.TrustedProxies, err = parseCIDRs(os.Getenv("AMBAR_TRUSTED_PROXIES")); err != nil {
		fail("%w", err)
	}

	if c.Workers, err = envInt("AMBAR_WORKERS", 2); err != nil {
		fail("%w", err)
	} else if c.Workers < 1 {
		// §12: default low, because this is a NAS with a weak CPU. Zero would
		// silently disable every job, which is worse than refusing to start.
		fail("AMBAR_WORKERS must be at least 1, got %d", c.Workers)
	}

	if c.MaxUploadSize, err = envInt64("AMBAR_MAX_UPLOAD_SIZE", 104857600); err != nil {
		fail("%w", err)
	} else if c.MaxUploadSize < 1 {
		fail("AMBAR_MAX_UPLOAD_SIZE must be positive, got %d", c.MaxUploadSize)
	}

	if c.MaxArchiveUncompressed, err = envInt64("AMBAR_MAX_ARCHIVE_UNCOMPRESSED", 21474836480); err != nil {
		fail("%w", err)
	} else if c.MaxArchiveUncompressed < 1 {
		fail("AMBAR_MAX_ARCHIVE_UNCOMPRESSED must be positive, got %d", c.MaxArchiveUncompressed)
	}

	if c.InboxPollInterval, err = envDuration("AMBAR_INBOX_POLL_INTERVAL", 30*time.Second); err != nil {
		fail("%w", err)
	} else if c.InboxPollInterval < time.Second {
		fail("AMBAR_INBOX_POLL_INTERVAL must be at least 1s, got %s", c.InboxPollInterval)
	}

	// Empty disables the internal scheduler (§13), so zero is legal here.
	if c.BackupInterval, err = envDurationDisableable("AMBAR_BACKUP_INTERVAL", time.Hour); err != nil {
		fail("%w", err)
	} else if c.BackupInterval < 0 {
		fail("AMBAR_BACKUP_INTERVAL must not be negative, got %s", c.BackupInterval)
	}

	if c.BackupKeep, err = envInt("AMBAR_BACKUP_KEEP", 48); err != nil {
		fail("%w", err)
	} else if c.BackupKeep < 1 {
		fail("AMBAR_BACKUP_KEEP must be at least 1, got %d", c.BackupKeep)
	}

	// Empty means never auto-purge (§13), which is also the default.
	if c.TrashRetention, err = envDurationDisableable("AMBAR_TRASH_RETENTION", 0); err != nil {
		fail("%w", err)
	} else if c.TrashRetention < 0 {
		fail("AMBAR_TRASH_RETENTION must not be negative, got %s", c.TrashRetention)
	}

	c.DedupeLinkMode = envStr("AMBAR_DEDUPE_LINK_MODE", "reflink")
	switch c.DedupeLinkMode {
	case "reflink", "hardlink", "off":
	default:
		fail("AMBAR_DEDUPE_LINK_MODE must be reflink, hardlink or off, got %q", c.DedupeLinkMode)
	}

	cookieSecure := envStr("AMBAR_COOKIE_SECURE", "auto")
	switch cookieSecure {
	case "auto":
		c.CookieSecure = c.BaseURL != nil && c.BaseURL.Scheme == "https"
	case "true":
		c.CookieSecure = true
	case "false":
		c.CookieSecure = false
	default:
		fail("AMBAR_COOKIE_SECURE must be auto, true or false, got %q", cookieSecure)
	}

	// §17: fail loudly rather than limping along. A uid/gid mismatch between the
	// container user and the NAS volume shows up here and nowhere else obvious.
	if err := checkDir(c.DataRoot, "AMBAR_DATA_ROOT", true); err != nil {
		fail("%w", err)
	}
	if err := checkDir(c.LibraryRoot, "AMBAR_LIBRARY_ROOT", !c.LibraryReadonly); err != nil {
		fail("%w", err)
	}

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}

	// Only now, with DataRoot confirmed writable, touch the filesystem.
	if err := c.resolveSessionSecret(); err != nil {
		return nil, err
	}
	return c, nil
}

// resolveSessionSecret uses AMBAR_SESSION_SECRET if set, otherwise reads or
// creates $DATA_ROOT/session_secret (§17).
func (c *Config) resolveSessionSecret() error {
	if s := os.Getenv("AMBAR_SESSION_SECRET"); s != "" {
		c.SessionSecret = []byte(s)
		c.SessionSecretSource = "env"
		return nil
	}

	path := filepath.Join(c.DataRoot, sessionSecretFile)
	switch b, err := os.ReadFile(path); {
	case err == nil:
		secret, err := hex.DecodeString(strings.TrimSpace(string(b)))
		if err != nil || len(secret) < 16 {
			return fmt.Errorf("%s is not a valid hex secret of at least 16 bytes; "+
				"delete it to have a new one generated (every session and CSRF "+
				"token issued under the old value stops working)", path)
		}
		c.SessionSecret = secret
		c.SessionSecretSource = "file"
		return nil
	case !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("read %s: %w", path, err)
	}

	secret := make([]byte, SessionSecretLen)
	if _, err := rand.Read(secret); err != nil {
		return fmt.Errorf("generate session secret: %w", err)
	}
	// 0600: the secret signs CSRF tokens, so it is not world-readable even
	// though the rest of the data root is.
	if err := os.WriteFile(path, []byte(hex.EncodeToString(secret)+"\n"), 0o600); err != nil {
		return fmt.Errorf("persist session secret to %s: %w", path, err)
	}
	c.SessionSecret = secret
	c.SessionSecretSource = "generated"
	return nil
}

// --- helpers ---------------------------------------------------------------

func envStr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func envBool(key string, def bool) (bool, error) {
	raw := envStr(key, "")
	if raw == "" {
		return def, nil
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return def, fmt.Errorf("%s must be a boolean, got %q", key, raw)
	}
	return v, nil
}

func envInt(key string, def int) (int, error) {
	v, err := envInt64(key, int64(def))
	return int(v), err
}

func envInt64(key string, def int64) (int64, error) {
	raw := envStr(key, "")
	if raw == "" {
		return def, nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return def, fmt.Errorf("%s must be an integer, got %q", key, raw)
	}
	return v, nil
}

// envDuration treats an empty or unset value as the default. Blanking a line in
// a .env file is how people disable an override, and it should not mean zero.
func envDuration(key string, def time.Duration) (time.Duration, error) {
	raw := envStr(key, "")
	if raw == "" {
		return def, nil
	}
	v, err := time.ParseDuration(raw)
	if err != nil {
		return def, fmt.Errorf("%s must be a duration such as 30s or 1h, got %q", key, raw)
	}
	return v, nil
}

// envDurationDisableable is for the two variables §13 documents as "empty
// disables this": AMBAR_BACKUP_INTERVAL and AMBAR_TRASH_RETENTION. There, an
// explicitly empty value means zero and must not fall back to the default,
// because "" is how the operator turns the feature off.
func envDurationDisableable(key string, def time.Duration) (time.Duration, error) {
	raw, set := os.LookupEnv(key)
	if set && strings.TrimSpace(raw) == "" {
		return 0, nil
	}
	return envDuration(key, def)
}

func mustAbs(path, key string, fail func(string, ...any)) string {
	if path == "" {
		fail("%s must not be empty", key)
		return path
	}
	if !filepath.IsAbs(path) {
		abs, err := filepath.Abs(path)
		if err != nil {
			fail("%s %q cannot be made absolute: %v", key, path, err)
			return path
		}
		return abs
	}
	return filepath.Clean(path)
}

func parseBaseURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("AMBAR_BASE_URL %q is not a URL: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("AMBAR_BASE_URL %q must start with http:// or https://", raw)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("AMBAR_BASE_URL %q has no host", raw)
	}
	return u, nil
}

// parseCIDRs accepts a comma-separated list of CIDRs. A bare address is
// accepted and treated as a single-host prefix, since writing 10.0.0.1 rather
// than 10.0.0.1/32 is the obvious mistake to forgive.
func parseCIDRs(raw string) ([]netip.Prefix, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var (
		out  []netip.Prefix
		errs []error
	)
	for _, field := range strings.Split(raw, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		if p, err := netip.ParsePrefix(field); err == nil {
			out = append(out, p.Masked())
			continue
		}
		addr, err := netip.ParseAddr(field)
		if err != nil {
			errs = append(errs, fmt.Errorf("AMBAR_TRUSTED_PROXIES entry %q is not a CIDR or IP address", field))
			continue
		}
		out = append(out, netip.PrefixFrom(addr, addr.BitLen()))
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return out, nil
}

// checkDir confirms a configured directory exists and is usable. When
// needWrite is false it is only probed for readability.
func checkDir(path, key string, needWrite bool) error {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%s %q does not exist", key, path)
		}
		return fmt.Errorf("%s %q is not accessible: %w", key, path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s %q is not a directory", key, path)
	}

	if _, err := os.ReadDir(path); err != nil {
		return fmt.Errorf("%s %q is not readable: %w", key, path, err)
	}
	if !needWrite {
		return nil
	}

	// Probe by actually writing: permission bits alone do not account for a
	// read-only mount or an ACL.
	probe, err := os.CreateTemp(path, ".ambar-write-probe-*")
	if err != nil {
		return fmt.Errorf("%s %q is not writable (run as the user that owns it, see §17): %w", key, path, err)
	}
	name := probe.Name()
	probe.Close()
	if err := os.Remove(name); err != nil {
		return fmt.Errorf("%s %q: could not remove write probe %s: %w", key, path, name, err)
	}
	return nil
}
