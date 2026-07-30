package auth

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/datcal/ambar/internal/db"
)

func tokenFixture(t *testing.T) (*TokenStore, int64) {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	if _, err := database.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	res, err := database.Writer.Exec(`
		INSERT INTO users (username, password_hash, role, created_at, updated_at)
		VALUES ('alice', 'x', 'user', 0, 0)`)
	if err != nil {
		t.Fatal(err)
	}
	uid, _ := res.LastInsertId()
	return NewTokenStore(database), uid
}

func TestTokenCreateAndAuthenticate(t *testing.T) {
	s, uid := tokenFixture(t)
	ctx := context.Background()

	plain, tok, err := s.Create(ctx, uid, "laptop", []string{"write"}, nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if plain[:6] != TokenPrefix {
		t.Errorf("token has no %q prefix: %q", TokenPrefix, plain)
	}
	// write implies read.
	if len(tok.Scopes) != 2 || tok.Scopes[0] != "read" || tok.Scopes[1] != "write" {
		t.Errorf("scopes = %v, want [read write]", tok.Scopes)
	}

	u, scopes, err := s.Authenticate(ctx, plain)
	if err != nil || u.ID != uid {
		t.Fatalf("authenticate = %d,%v,%v", u.ID, scopes, err)
	}
	if !scopeSatisfies(scopes, ScopeWrite) || scopeSatisfies(scopes, ScopeAdmin) {
		t.Errorf("scope check wrong: %v", scopes)
	}

	// last_used_at is stamped.
	list, _ := s.List(ctx, uid)
	if len(list) != 1 || list[0].LastUsedAt == nil {
		t.Errorf("last_used_at not stamped: %+v", list)
	}
}

func TestTokenRevoke(t *testing.T) {
	s, uid := tokenFixture(t)
	ctx := context.Background()
	plain, tok, _ := s.Create(ctx, uid, "old", nil, nil)

	if err := s.Revoke(ctx, tok.ID, uid); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Authenticate(ctx, plain); err != ErrTokenInvalid {
		t.Errorf("revoked token authenticated: %v", err)
	}
}

func TestTokenExpiry(t *testing.T) {
	s, uid := tokenFixture(t)
	ctx := context.Background()
	past := time.Now().Add(-time.Hour)
	plain, _, _ := s.Create(ctx, uid, "expired", nil, &past)
	if _, _, err := s.Authenticate(ctx, plain); err != ErrTokenInvalid {
		t.Errorf("expired token authenticated: %v", err)
	}
}

func TestTokenUnknownRejected(t *testing.T) {
	s, _ := tokenFixture(t)
	if _, _, err := s.Authenticate(context.Background(), "ambar_bogus"); err != ErrTokenInvalid {
		t.Errorf("unknown token = %v, want ErrTokenInvalid", err)
	}
	if _, _, err := s.Authenticate(context.Background(), "not-even-prefixed"); err != ErrTokenInvalid {
		t.Errorf("unprefixed token = %v", err)
	}
}
