package auth

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/datcal/ambar/internal/db"
)

func newTestDB(t *testing.T) *db.DB {
	t.Helper()

	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	if _, err := d.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return d
}

// insertTestUser bypasses UserStore.Create so the suite does not pay for
// argon2id on every session test.
func insertTestUser(t *testing.T, d *db.DB, username string) int64 {
	t.Helper()

	now := time.Now().Unix()
	res, err := d.Writer.ExecContext(context.Background(), `
		INSERT INTO users (username, password_hash, role, created_at, updated_at)
		VALUES (?, 'not-a-real-hash', 'user', ?, ?)`, username, now, now)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestSessionCreateAndLookup(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()
	userID := insertTestUser(t, d, "alice")

	store := NewSessionStore(d)
	token, created, err := store.Create(ctx, userID, "Mozilla/5.0", "10.0.0.5")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if token == "" {
		t.Fatal("Create returned an empty token")
	}

	sess, user, err := store.Lookup(ctx, token)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if sess.ID != created.ID {
		t.Errorf("looked up session %d, created %d", sess.ID, created.ID)
	}
	if user.Username != "alice" {
		t.Errorf("user = %q, want alice", user.Username)
	}
}

// TestSessionTokenIsNotStored is the property that makes a leaked database
// backup harmless for live sessions.
func TestSessionTokenIsNotStored(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()
	userID := insertTestUser(t, d, "alice")

	token, _, err := NewSessionStore(d).Create(ctx, userID, "", "")
	if err != nil {
		t.Fatal(err)
	}

	var stored []byte
	if err := d.Reader.QueryRowContext(ctx, `SELECT token_hash FROM sessions`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if string(stored) == token {
		t.Fatal("the plaintext token is stored in the database")
	}
	if len(stored) != 32 {
		t.Errorf("token_hash is %d bytes, want a 32-byte SHA-256", len(stored))
	}
	// And the hash must be the right one, or Lookup would never match.
	if string(stored) != string(hashToken(token)) {
		t.Error("stored hash does not match SHA-256 of the token")
	}
}

func TestLookupRejectsUnknownTokens(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()
	insertTestUser(t, d, "alice")

	store := NewSessionStore(d)
	for _, token := range []string{"", "not-a-token", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"} {
		if _, _, err := store.Lookup(ctx, token); !errors.Is(err, ErrNoSession) {
			t.Errorf("lookup(%q) returned %v, want ErrNoSession", token, err)
		}
	}
}

// TestSessionExpiry drives both clocks from §11.
func TestSessionExpiry(t *testing.T) {
	t.Run("absolute expiry", func(t *testing.T) {
		d := newTestDB(t)
		ctx := context.Background()
		userID := insertTestUser(t, d, "alice")

		now := time.Now()
		clock := func() time.Time { return now }
		// Idle window longer than the absolute one, so only the absolute clock
		// can be what ends this session.
		store := NewSessionStore(d).WithTTL(time.Hour, 24*time.Hour).WithClock(clock)

		token, _, err := store.Create(ctx, userID, "", "")
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.Lookup(ctx, token); err != nil {
			t.Fatalf("fresh session did not validate: %v", err)
		}

		now = now.Add(59 * time.Minute)
		if _, _, err := store.Lookup(ctx, token); err != nil {
			t.Errorf("session expired early: %v", err)
		}

		now = now.Add(2 * time.Minute) // past the hour
		if _, _, err := store.Lookup(ctx, token); !errors.Is(err, ErrNoSession) {
			t.Errorf("session survived its absolute expiry: %v", err)
		}
	})

	t.Run("idle expiry", func(t *testing.T) {
		d := newTestDB(t)
		ctx := context.Background()
		userID := insertTestUser(t, d, "alice")

		now := time.Now()
		clock := func() time.Time { return now }
		store := NewSessionStore(d).WithTTL(30*24*time.Hour, time.Hour).WithClock(clock)

		token, _, err := store.Create(ctx, userID, "", "")
		if err != nil {
			t.Fatal(err)
		}

		// Untouched for longer than the idle window.
		now = now.Add(90 * time.Minute)
		if _, _, err := store.Lookup(ctx, token); !errors.Is(err, ErrNoSession) {
			t.Errorf("session survived its idle expiry: %v", err)
		}
	})

	t.Run("idle window slides on use", func(t *testing.T) {
		d := newTestDB(t)
		ctx := context.Background()
		userID := insertTestUser(t, d, "alice")

		now := time.Now()
		clock := func() time.Time { return now }
		store := NewSessionStore(d).WithTTL(30*24*time.Hour, time.Hour).WithClock(clock)

		token, _, err := store.Create(ctx, userID, "", "")
		if err != nil {
			t.Fatal(err)
		}

		// Use it every 40 minutes: past the halfway mark, so the window slides.
		for i := 0; i < 5; i++ {
			now = now.Add(40 * time.Minute)
			if _, _, err := store.Lookup(ctx, token); err != nil {
				t.Fatalf("session expired at step %d despite regular use: %v", i, err)
			}
		}

		// Now leave it alone for longer than the idle window.
		now = now.Add(2 * time.Hour)
		if _, _, err := store.Lookup(ctx, token); !errors.Is(err, ErrNoSession) {
			t.Error("session survived going idle after being active")
		}
	})

	t.Run("sliding never exceeds the absolute expiry", func(t *testing.T) {
		d := newTestDB(t)
		ctx := context.Background()
		userID := insertTestUser(t, d, "alice")

		now := time.Now()
		clock := func() time.Time { return now }
		// Idle window longer than absolute: sliding must clamp.
		store := NewSessionStore(d).WithTTL(time.Hour, 24*time.Hour).WithClock(clock)

		token, created, err := store.Create(ctx, userID, "", "")
		if err != nil {
			t.Fatal(err)
		}

		now = now.Add(30 * time.Minute)
		sess, _, err := store.Lookup(ctx, token)
		if err != nil {
			t.Fatal(err)
		}
		if sess.IdleExpiresAt.After(created.ExpiresAt) {
			t.Errorf("idle expiry %s was slid past the absolute expiry %s",
				sess.IdleExpiresAt, created.ExpiresAt)
		}
	})
}

// TestRotateInvalidatesTheOldToken is the §11 session-fixation defence.
func TestRotateInvalidatesTheOldToken(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()
	userID := insertTestUser(t, d, "alice")

	store := NewSessionStore(d)
	oldToken, _, err := store.Create(ctx, userID, "", "")
	if err != nil {
		t.Fatal(err)
	}

	newToken, _, err := store.Rotate(ctx, oldToken, userID, "", "")
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if newToken == oldToken {
		t.Fatal("Rotate returned the same token, so the session was not rotated")
	}
	if _, _, err := store.Lookup(ctx, oldToken); !errors.Is(err, ErrNoSession) {
		t.Errorf("the pre-rotation token still works: %v", err)
	}
	if _, _, err := store.Lookup(ctx, newToken); err != nil {
		t.Errorf("the post-rotation token does not work: %v", err)
	}

	var n int
	if err := d.Reader.QueryRowContext(ctx, `SELECT count(*) FROM sessions`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("%d session rows after rotation, want 1", n)
	}
}

// TestRotateFromNoSession covers a first login, where there is no old token.
func TestRotateFromNoSession(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()
	userID := insertTestUser(t, d, "alice")

	token, _, err := NewSessionStore(d).Rotate(ctx, "", userID, "", "")
	if err != nil {
		t.Fatalf("rotate with no previous session: %v", err)
	}
	if token == "" {
		t.Error("no token issued")
	}
}

func TestDestroy(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()
	userID := insertTestUser(t, d, "alice")

	store := NewSessionStore(d)
	token, _, err := store.Create(ctx, userID, "", "")
	if err != nil {
		t.Fatal(err)
	}

	if err := store.Destroy(ctx, token); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if _, _, err := store.Lookup(ctx, token); !errors.Is(err, ErrNoSession) {
		t.Errorf("session survived being destroyed: %v", err)
	}

	// A second logout must not be an error.
	if err := store.Destroy(ctx, token); err != nil {
		t.Errorf("destroying an already-gone session errored: %v", err)
	}
	if err := store.Destroy(ctx, ""); err != nil {
		t.Errorf("destroying an empty token errored: %v", err)
	}
}

func TestDestroyAllForUser(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()
	alice := insertTestUser(t, d, "alice")
	bob := insertTestUser(t, d, "bob")

	store := NewSessionStore(d)
	var aliceTokens []string
	for i := 0; i < 3; i++ {
		token, _, err := store.Create(ctx, alice, "", "")
		if err != nil {
			t.Fatal(err)
		}
		aliceTokens = append(aliceTokens, token)
	}
	bobToken, _, err := store.Create(ctx, bob, "", "")
	if err != nil {
		t.Fatal(err)
	}

	if err := store.DestroyAllForUser(ctx, alice); err != nil {
		t.Fatalf("destroy all: %v", err)
	}
	for i, token := range aliceTokens {
		if _, _, err := store.Lookup(ctx, token); !errors.Is(err, ErrNoSession) {
			t.Errorf("alice's session %d survived: %v", i, err)
		}
	}
	// The other user must be untouched — §11's two people work independently.
	if _, _, err := store.Lookup(ctx, bobToken); err != nil {
		t.Errorf("bob's session was destroyed along with alice's: %v", err)
	}
}

func TestDeleteExpired(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()
	userID := insertTestUser(t, d, "alice")

	now := time.Now()
	clock := func() time.Time { return now }
	store := NewSessionStore(d).WithTTL(time.Hour, time.Hour).WithClock(clock)

	expired, _, err := store.Create(ctx, userID, "", "")
	if err != nil {
		t.Fatal(err)
	}

	now = now.Add(2 * time.Hour)
	live, _, err := store.Create(ctx, userID, "", "")
	if err != nil {
		t.Fatal(err)
	}

	n, err := store.DeleteExpired(ctx)
	if err != nil {
		t.Fatalf("delete expired: %v", err)
	}
	if n != 1 {
		t.Errorf("deleted %d rows, want 1", n)
	}
	if _, _, err := store.Lookup(ctx, expired); !errors.Is(err, ErrNoSession) {
		t.Error("the expired session still resolves")
	}
	if _, _, err := store.Lookup(ctx, live); err != nil {
		t.Errorf("the live session was deleted: %v", err)
	}
}

// TestTokensAreUnique guards against a broken CSPRNG path silently issuing
// duplicate tokens.
func TestTokensAreUnique(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()
	userID := insertTestUser(t, d, "alice")

	store := NewSessionStore(d)
	seen := make(map[string]bool, 200)
	for i := 0; i < 200; i++ {
		token, _, err := store.Create(ctx, userID, "", "")
		if err != nil {
			t.Fatal(err)
		}
		if seen[token] {
			t.Fatalf("token %q was issued twice", token)
		}
		seen[token] = true
		// 32 random bytes, base64url without padding.
		if len(token) != 43 {
			t.Fatalf("token length %d, want 43 for 32 random bytes", len(token))
		}
	}
}

func TestLongUserAgentIsTruncated(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()
	userID := insertTestUser(t, d, "alice")

	long := make([]byte, 4096)
	for i := range long {
		long[i] = 'A'
	}
	if _, _, err := NewSessionStore(d).Create(ctx, userID, string(long), ""); err != nil {
		t.Fatal(err)
	}

	var stored string
	if err := d.Reader.QueryRowContext(ctx, `SELECT user_agent FROM sessions`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if len(stored) > 512 {
		t.Errorf("stored user_agent is %d bytes, want at most 512", len(stored))
	}
}
