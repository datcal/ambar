package auth

import (
	"errors"
	"strings"
	"testing"
)

// testParams keep the suite fast. DefaultParams is deliberately slow, which is
// the point of argon2id but makes it unusable in a loop.
var testParams = Params{Memory: 8 * 1024, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}

func TestHashAndVerifyRoundTrip(t *testing.T) {
	const password = "correct horse battery staple"

	encoded, err := HashPasswordWithParams(password, testParams)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}

	ok, err := VerifyPassword(encoded, password)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !ok {
		t.Error("the correct password did not verify")
	}
}

func TestVerifyRejectsWrongPasswords(t *testing.T) {
	encoded, err := HashPasswordWithParams("correct horse battery staple", testParams)
	if err != nil {
		t.Fatal(err)
	}

	for _, wrong := range []string{
		"",
		"correct horse battery stapl",
		"correct horse battery staple ",
		"Correct horse battery staple",
		"correct horse battery staplf",
	} {
		ok, err := VerifyPassword(encoded, wrong)
		if err != nil {
			t.Errorf("verify(%q) returned an error: %v", wrong, err)
		}
		if ok {
			t.Errorf("verify(%q) succeeded, want failure", wrong)
		}
	}
}

// TestHashesAreSalted: two hashes of the same password must differ, or the
// database leaks which users share a password.
func TestHashesAreSalted(t *testing.T) {
	const password = "the same password twice"

	first, err := HashPasswordWithParams(password, testParams)
	if err != nil {
		t.Fatal(err)
	}
	second, err := HashPasswordWithParams(password, testParams)
	if err != nil {
		t.Fatal(err)
	}

	if first == second {
		t.Fatal("two hashes of the same password are identical; the salt is not random")
	}
	// Both must still verify.
	for i, encoded := range []string{first, second} {
		ok, err := VerifyPassword(encoded, password)
		if err != nil || !ok {
			t.Errorf("hash %d did not verify: ok=%v err=%v", i, ok, err)
		}
	}
}

func TestHashFormat(t *testing.T) {
	encoded, err := HashPasswordWithParams("some password here", testParams)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.HasPrefix(encoded, "$argon2id$v=19$m=8192,t=1,p=1$") {
		t.Errorf("unexpected hash prefix: %q", encoded)
	}
	if got := len(strings.Split(encoded, "$")); got != 6 {
		t.Errorf("hash has %d $-separated fields, want 6: %q", got, encoded)
	}
	// The plaintext must not be recoverable from, or present in, the encoding.
	if strings.Contains(encoded, "some password here") {
		t.Error("the hash contains the plaintext")
	}
}

// TestVerifyRejectsMalformedHashes matters because a corrupt or hand-edited
// users row must never verify as a success. Every case here has to come back as
// an error, not as ok=true.
func TestVerifyRejectsMalformedHashes(t *testing.T) {
	valid, err := HashPasswordWithParams("a valid password", testParams)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(valid, "$")

	tests := []struct {
		name    string
		encoded string
	}{
		{"empty", ""},
		{"plaintext", "a valid password"},
		{"bare digest", "5e884898da28047151d0e56f8dc6292773603d0d6aabbdd62a11ef721d1542d8"},
		{"too few fields", "$argon2id$v=19$m=8192,t=1,p=1$salt"},
		{"too many fields", valid + "$extra"},
		{"wrong algorithm", "$argon2i$" + strings.Join(parts[2:], "$")},
		{"bcrypt", "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"},
		{"unreadable version", "$argon2id$v=nineteen$" + strings.Join(parts[3:], "$")},
		{"wrong version", "$argon2id$v=16$" + strings.Join(parts[3:], "$")},
		{"unreadable params", "$argon2id$v=19$m=lots,t=1,p=1$" + strings.Join(parts[4:], "$")},
		{"zero memory", "$argon2id$v=19$m=0,t=1,p=1$" + strings.Join(parts[4:], "$")},
		{"missing params", "$argon2id$v=19$$" + strings.Join(parts[4:], "$")},
		{"bad base64 salt", "$argon2id$v=19$m=8192,t=1,p=1$not!base64!$" + parts[5]},
		{"bad base64 key", strings.Join(parts[:5], "$") + "$not!base64!"},
		{"empty salt", "$argon2id$v=19$m=8192,t=1,p=1$$" + parts[5]},
		{"empty key", strings.Join(parts[:5], "$") + "$"},
		{"truncated", valid[:len(valid)-10]},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ok, err := VerifyPassword(tc.encoded, "a valid password")
			if ok {
				t.Fatalf("malformed hash %q verified as a success", tc.encoded)
			}
			if err == nil {
				t.Fatalf("malformed hash %q returned no error", tc.encoded)
			}
			if !errors.Is(err, ErrInvalidHash) {
				t.Errorf("error is not ErrInvalidHash: %v", err)
			}
		})
	}
}

// TestVerifyUsesTheHashesOwnParams is what lets DefaultParams be raised later
// without invalidating existing passwords.
func TestVerifyUsesTheHashesOwnParams(t *testing.T) {
	const password = "parameters live in the hash"

	cheap := Params{Memory: 8 * 1024, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}
	dearer := Params{Memory: 16 * 1024, Time: 2, Threads: 1, SaltLen: 16, KeyLen: 32}

	for _, p := range []Params{cheap, dearer} {
		encoded, err := HashPasswordWithParams(password, p)
		if err != nil {
			t.Fatal(err)
		}
		ok, err := VerifyPassword(encoded, password)
		if err != nil || !ok {
			t.Errorf("hash with params %+v did not verify: ok=%v err=%v", p, ok, err)
		}
	}
}

func TestHashRejectsZeroParams(t *testing.T) {
	for _, p := range []Params{
		{},
		{Memory: 8 * 1024, Time: 0, Threads: 1, SaltLen: 16, KeyLen: 32},
		{Memory: 8 * 1024, Time: 1, Threads: 0, SaltLen: 16, KeyLen: 32},
		{Memory: 8 * 1024, Time: 1, Threads: 1, SaltLen: 0, KeyLen: 32},
		{Memory: 8 * 1024, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 0},
	} {
		if _, err := HashPasswordWithParams("password", p); err == nil {
			t.Errorf("params %+v were accepted", p)
		}
	}
}

// TestDefaultParamsAreNotWeak pins the cost, since a well-meaning "make the
// tests faster" edit to DefaultParams would silently weaken production.
func TestDefaultParamsAreNotWeak(t *testing.T) {
	if DefaultParams.Memory < 32*1024 {
		t.Errorf("DefaultParams.Memory = %d KiB, want at least 32768", DefaultParams.Memory)
	}
	if DefaultParams.Time < 1 {
		t.Error("DefaultParams.Time must be at least 1")
	}
	if DefaultParams.KeyLen < 32 {
		t.Errorf("DefaultParams.KeyLen = %d, want at least 32", DefaultParams.KeyLen)
	}
	if DefaultParams.SaltLen < 16 {
		t.Errorf("DefaultParams.SaltLen = %d, want at least 16", DefaultParams.SaltLen)
	}
}

// TestDummyHashIsUsable backs the §11 anti-enumeration path: the login handler
// verifies against this when the username does not exist, so it has to be a real
// hash that a wrong password fails against.
func TestDummyHashIsUsable(t *testing.T) {
	encoded, err := DummyHash()
	if err != nil {
		t.Fatalf("DummyHash: %v", err)
	}

	ok, err := VerifyPassword(encoded, "whatever an attacker typed")
	if err != nil {
		t.Errorf("verifying against the dummy hash errored: %v", err)
	}
	if ok {
		t.Error("an arbitrary password verified against the dummy hash")
	}

	// Two calls must differ, or the value is a fingerprint of the build.
	other, err := DummyHash()
	if err != nil {
		t.Fatal(err)
	}
	if encoded == other {
		t.Error("DummyHash returned the same value twice")
	}
}
