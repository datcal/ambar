package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// MinPasswordLength is enforced by `ambar user add`. §2 notes the app can end
// up publicly reachable through Funnel or Cloudflare with no edge rate
// limiting, so a short password is not a private mistake.
const MinPasswordLength = 12

// Params are the argon2id cost parameters.
//
// §12 is explicit that this is a NAS with a weak CPU running other services.
// 64 MiB with two passes lands in the low hundreds of milliseconds there, which
// is the right trade for a login that happens a few times a day. They are
// stored in each hash, so raising them later does not invalidate existing
// passwords — old hashes keep verifying with their own parameters.
type Params struct {
	Memory  uint32 // KiB
	Time    uint32 // passes
	Threads uint8
	SaltLen uint32
	KeyLen  uint32
}

// DefaultParams is what `ambar user add` uses.
var DefaultParams = Params{
	Memory:  64 * 1024,
	Time:    2,
	Threads: 2,
	SaltLen: 16,
	KeyLen:  32,
}

// ErrInvalidHash means the stored string is not a hash this code can read,
// which is a corrupt or hand-edited users row rather than a wrong password.
var ErrInvalidHash = errors.New("not a valid argon2id hash")

// HashPassword hashes with DefaultParams.
func HashPassword(password string) (string, error) {
	return HashPasswordWithParams(password, DefaultParams)
}

// HashPasswordWithParams exists so tests can use cheap parameters; production
// code should call HashPassword.
func HashPasswordWithParams(password string, p Params) (string, error) {
	if p.SaltLen == 0 || p.KeyLen == 0 || p.Memory == 0 || p.Time == 0 || p.Threads == 0 {
		return "", fmt.Errorf("argon2id parameters must all be non-zero, got %+v", p)
	}
	salt := make([]byte, p.SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("read salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Threads, p.KeyLen)

	// The standard PHC string format, the same one Python's passlib and PHP's
	// password_hash produce, so a hash is portable if this ever moves.
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.Memory, p.Time, p.Threads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// VerifyPassword reports whether password matches encoded.
//
// A false return with a nil error is a wrong password. A non-nil error means
// the stored hash could not be read at all — callers must treat that as a
// failed login too, never as a success.
func VerifyPassword(encoded, password string) (bool, error) {
	p, salt, want, err := decodeHash(encoded)
	if err != nil {
		return false, err
	}
	got := argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Threads, p.KeyLen)
	// §11: constant-time comparison.
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// DummyHash returns a hash of a random secret, used to equalize the cost of a
// login attempt against a username that does not exist (§11: no
// user-enumeration difference between "no such user" and "wrong password").
//
// Computed once at startup, since it costs a full argon2id pass.
func DummyHash() (string, error) {
	filler := make([]byte, 32)
	if _, err := rand.Read(filler); err != nil {
		return "", fmt.Errorf("read filler: %w", err)
	}
	return HashPassword(base64.RawStdEncoding.EncodeToString(filler))
}

func decodeHash(encoded string) (p Params, salt, key []byte, err error) {
	parts := strings.Split(encoded, "$")
	// "", "argon2id", "v=19", "m=...,t=...,p=...", salt, key
	if len(parts) != 6 || parts[0] != "" {
		return p, nil, nil, fmt.Errorf("%w: expected 6 $-separated fields, got %d", ErrInvalidHash, len(parts))
	}
	if parts[1] != "argon2id" {
		return p, nil, nil, fmt.Errorf("%w: algorithm is %q, want argon2id", ErrInvalidHash, parts[1])
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return p, nil, nil, fmt.Errorf("%w: unreadable version %q", ErrInvalidHash, parts[2])
	}
	if version != argon2.Version {
		return p, nil, nil, fmt.Errorf("%w: version %d, want %d", ErrInvalidHash, version, argon2.Version)
	}

	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.Memory, &p.Time, &p.Threads); err != nil {
		return p, nil, nil, fmt.Errorf("%w: unreadable parameters %q", ErrInvalidHash, parts[3])
	}
	if p.Memory == 0 || p.Time == 0 || p.Threads == 0 {
		return p, nil, nil, fmt.Errorf("%w: zero parameter in %q", ErrInvalidHash, parts[3])
	}

	if salt, err = base64.RawStdEncoding.DecodeString(parts[4]); err != nil || len(salt) == 0 {
		return p, nil, nil, fmt.Errorf("%w: unreadable salt", ErrInvalidHash)
	}
	if key, err = base64.RawStdEncoding.DecodeString(parts[5]); err != nil || len(key) == 0 {
		return p, nil, nil, fmt.Errorf("%w: unreadable key", ErrInvalidHash)
	}

	p.SaltLen = uint32(len(salt))
	p.KeyLen = uint32(len(key))
	return p, salt, key, nil
}
