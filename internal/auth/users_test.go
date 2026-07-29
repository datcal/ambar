package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestCreateAndLookupUser(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()
	store := NewUserStore(d)

	created, err := store.Create(ctx, "burak", "a-long-enough-password", RoleUser)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.ID == 0 {
		t.Error("created user has no ID")
	}
	if created.Role != RoleUser {
		t.Errorf("role = %q, want %q", created.Role, RoleUser)
	}
	if created.LastLoginAt != nil {
		t.Error("a new user already has a last login time")
	}

	found, err := store.ByUsername(ctx, "burak")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if found.ID != created.ID {
		t.Errorf("looked up %d, created %d", found.ID, created.ID)
	}

	// The stored hash must verify, and must not be the plaintext.
	if found.PasswordHash == "a-long-enough-password" {
		t.Fatal("the password is stored in plaintext")
	}
	ok, err := VerifyPassword(found.PasswordHash, "a-long-enough-password")
	if err != nil || !ok {
		t.Errorf("the stored hash does not verify: ok=%v err=%v", ok, err)
	}

	byID, err := store.ByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}
	if byID.Username != "burak" {
		t.Errorf("ByID returned %q", byID.Username)
	}
}

// TestUsernameNormalization: "Burak" and " burak " must be one account, not
// three, on both write and lookup.
func TestUsernameNormalization(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()
	store := NewUserStore(d)

	if _, err := store.Create(ctx, "  BuRaK  ", "a-long-enough-password", RoleUser); err != nil {
		t.Fatalf("create: %v", err)
	}

	for _, variant := range []string{"burak", "BURAK", " Burak ", "BuRaK"} {
		user, err := store.ByUsername(ctx, variant)
		if err != nil {
			t.Errorf("lookup(%q): %v", variant, err)
			continue
		}
		if user.Username != "burak" {
			t.Errorf("lookup(%q) returned username %q, want burak", variant, user.Username)
		}
	}

	// And a differently-cased duplicate must be refused.
	if _, err := store.Create(ctx, "BURAK", "another-long-password", RoleUser); !errors.Is(err, ErrUserExists) {
		t.Errorf("creating a case-variant duplicate returned %v, want ErrUserExists", err)
	}
}

func TestCreateRejectsDuplicates(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()
	store := NewUserStore(d)

	if _, err := store.Create(ctx, "burak", "a-long-enough-password", RoleUser); err != nil {
		t.Fatal(err)
	}
	_, err := store.Create(ctx, "burak", "a-different-long-password", RoleUser)
	if !errors.Is(err, ErrUserExists) {
		t.Errorf("err = %v, want ErrUserExists", err)
	}
}

func TestValidateUsername(t *testing.T) {
	tests := []struct {
		username string
		valid    bool
	}{
		{"burak", true},
		{"al", true},
		{"a.b_c-d", true},
		{"user123", true},
		{"", false},
		{"a", false},                     // too short
		{strings.Repeat("a", 65), false}, // too long
		{".leading-dot", false},
		{"-leading-dash", false},
		{"has space", false},
		{"has/slash", false},
		{"has:colon", false},
		{"../traversal", false},
		{"has\nnewline", false},
		{"emoji-🎮", false},
		{"UPPER", false}, // callers normalize first
	}
	for _, tc := range tests {
		err := ValidateUsername(tc.username)
		if tc.valid && err != nil {
			t.Errorf("ValidateUsername(%q) = %v, want valid", tc.username, err)
		}
		if !tc.valid && err == nil {
			t.Errorf("ValidateUsername(%q) accepted an invalid username", tc.username)
		}
	}
}

func TestCreateRejectsInvalidInput(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()
	store := NewUserStore(d)

	t.Run("bad username", func(t *testing.T) {
		if _, err := store.Create(ctx, "has space", "a-long-enough-password", RoleUser); err == nil {
			t.Error("an invalid username was accepted")
		}
	})

	t.Run("short password", func(t *testing.T) {
		_, err := store.Create(ctx, "shorty", strings.Repeat("x", MinPasswordLength-1), RoleUser)
		if err == nil {
			t.Error("a too-short password was accepted")
		}
		if !strings.Contains(err.Error(), "at least") {
			t.Errorf("unclear error: %v", err)
		}
	})

	t.Run("password at the minimum is fine", func(t *testing.T) {
		if _, err := store.Create(ctx, "exact", strings.Repeat("x", MinPasswordLength), RoleUser); err != nil {
			t.Errorf("a password of exactly the minimum length was rejected: %v", err)
		}
	})

	t.Run("unknown role", func(t *testing.T) {
		_, err := store.Create(ctx, "wannabeadmin", "a-long-enough-password", "admin")
		if err == nil {
			t.Error("role \"admin\" was accepted; §11 ships exactly one role")
		}
	})

	t.Run("empty role defaults to user", func(t *testing.T) {
		u, err := store.Create(ctx, "defaulted", "a-long-enough-password", "")
		if err != nil {
			t.Fatal(err)
		}
		if u.Role != RoleUser {
			t.Errorf("role = %q, want %q", u.Role, RoleUser)
		}
	})
}

func TestByUsernameNotFound(t *testing.T) {
	d := newTestDB(t)
	if _, err := NewUserStore(d).ByUsername(context.Background(), "nobody"); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("err = %v, want ErrUserNotFound", err)
	}
}

func TestCountAndList(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()
	store := NewUserStore(d)

	n, err := store.Count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("Count on an empty database = %d, want 0", n)
	}

	for _, name := range []string{"burak", "ada"} {
		if _, err := store.Create(ctx, name, "a-long-enough-password", RoleUser); err != nil {
			t.Fatal(err)
		}
	}

	if n, err = store.Count(ctx); err != nil || n != 2 {
		t.Errorf("Count = %d (err %v), want 2", n, err)
	}

	users, err := store.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 2 {
		t.Fatalf("List returned %d users, want 2", len(users))
	}
	// Oldest first.
	if users[0].Username != "burak" {
		t.Errorf("List order = %q first, want burak", users[0].Username)
	}
}

func TestTouchLogin(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()
	store := NewUserStore(d)

	created, err := store.Create(ctx, "burak", "a-long-enough-password", RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.TouchLogin(ctx, created.ID); err != nil {
		t.Fatalf("TouchLogin: %v", err)
	}

	found, err := store.ByUsername(ctx, "burak")
	if err != nil {
		t.Fatal(err)
	}
	if found.LastLoginAt == nil {
		t.Fatal("last_login_at was not recorded")
	}
	if found.LastLoginAt.IsZero() {
		t.Error("last_login_at is the zero time")
	}
}
