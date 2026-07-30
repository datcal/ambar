package auth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/datcal/ambar/internal/db"
)

// TokenPrefix marks an Ambar API token so it is recognisable in logs and config.
const TokenPrefix = "ambar_"

// tokenBytes is the random secret length. 32 bytes of CSPRNG output is why the
// stored SHA-256 needs no salt or slow KDF (§11).
const tokenBytes = 32

// Scopes (§11). read is the floor; write implies read; admin implies both.
const (
	ScopeRead  = "read"
	ScopeWrite = "write"
	ScopeAdmin = "admin"
)

// ErrTokenInvalid means the presented bearer token is unknown, revoked or expired.
var ErrTokenInvalid = errors.New("invalid API token")

// Token is one api_tokens row as the management UI needs it — never the hash.
type Token struct {
	ID         int64
	UserID     int64
	Name       string
	Scopes     []string
	LastUsedAt *time.Time
	ExpiresAt  *time.Time
	RevokedAt  *time.Time
	CreatedAt  time.Time
}

// Revoked reports whether the token has been revoked.
func (t Token) Revoked() bool { return t.RevokedAt != nil }

// Expired reports whether the token's expiry has passed.
func (t Token) Expired() bool { return t.ExpiresAt != nil && t.ExpiresAt.Before(time.Now()) }

// TokenStore reads and writes API tokens.
type TokenStore struct {
	db  *db.DB
	now func() time.Time
}

// NewTokenStore wraps a database.
func NewTokenStore(database *db.DB) *TokenStore {
	return &TokenStore{db: database, now: time.Now}
}

// WithClock replaces the clock, for tests.
func (s *TokenStore) WithClock(now func() time.Time) *TokenStore {
	s.now = now
	return s
}

// Create mints a token for a user, returning the plaintext exactly once (§11:
// "show plaintext once at creation"). scopes is normalised; an empty set is read.
func (s *TokenStore) Create(ctx context.Context, userID int64, name string, scopes []string, expiresAt *time.Time) (plaintext string, t Token, err error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", Token{}, fmt.Errorf("a token needs a name")
	}
	scopes = normaliseScopes(scopes)

	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", Token{}, fmt.Errorf("generate token: %w", err)
	}
	plaintext = TokenPrefix + base64.RawURLEncoding.EncodeToString(raw)

	now := s.now()
	res, err := s.db.Writer.ExecContext(ctx, `
		INSERT INTO api_tokens (user_id, name, token_hash, scopes, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		userID, name, hashToken(plaintext), strings.Join(scopes, ","), unixOrNil(expiresAt), now.Unix())
	if err != nil {
		return "", Token{}, fmt.Errorf("create token: %w", err)
	}
	id, _ := res.LastInsertId()
	return plaintext, Token{ID: id, UserID: userID, Name: name, Scopes: scopes,
		ExpiresAt: expiresAt, CreatedAt: now}, nil
}

// Authenticate resolves a bearer token to its user and scopes, updating
// last_used_at. A revoked or expired token is ErrTokenInvalid.
func (s *TokenStore) Authenticate(ctx context.Context, plaintext string) (User, []string, error) {
	plaintext = strings.TrimSpace(plaintext)
	if !strings.HasPrefix(plaintext, TokenPrefix) {
		return User{}, nil, ErrTokenInvalid
	}

	var (
		id        int64
		u         User
		scopes    string
		expiresAt sql.NullInt64
		revoked   sql.NullInt64
	)
	err := s.db.Reader.QueryRowContext(ctx, `
		SELECT t.id, t.scopes, t.expires_at, t.revoked_at,
		       u.id, u.username, u.role
		FROM api_tokens t JOIN users u ON u.id = t.user_id
		WHERE t.token_hash = ?`, hashToken(plaintext)).
		Scan(&id, &scopes, &expiresAt, &revoked, &u.ID, &u.Username, &u.Role)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, nil, ErrTokenInvalid
	}
	if err != nil {
		return User{}, nil, fmt.Errorf("authenticate token: %w", err)
	}
	if revoked.Valid {
		return User{}, nil, ErrTokenInvalid
	}
	if expiresAt.Valid && time.Unix(expiresAt.Int64, 0).Before(s.now()) {
		return User{}, nil, ErrTokenInvalid
	}

	// Best-effort last-used stamp; a failure here must not fail the request.
	_, _ = s.db.Writer.ExecContext(ctx,
		`UPDATE api_tokens SET last_used_at = ? WHERE id = ?`, s.now().Unix(), id)

	return u, strings.Split(scopes, ","), nil
}

// List returns a user's tokens, newest first, without any secret material.
func (s *TokenStore) List(ctx context.Context, userID int64) ([]Token, error) {
	rows, err := s.db.Reader.QueryContext(ctx, `
		SELECT id, user_id, name, scopes, last_used_at, expires_at, revoked_at, created_at
		FROM api_tokens WHERE user_id = ? ORDER BY created_at DESC, id DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("list tokens: %w", err)
	}
	defer rows.Close()

	var out []Token
	for rows.Next() {
		var (
			t                          Token
			scopes                     string
			lastUsed, expires, revoked sql.NullInt64
			created                    int64
		)
		if err := rows.Scan(&t.ID, &t.UserID, &t.Name, &scopes, &lastUsed, &expires, &revoked, &created); err != nil {
			return nil, err
		}
		t.Scopes = strings.Split(scopes, ",")
		t.LastUsedAt = timeOrNil(lastUsed)
		t.ExpiresAt = timeOrNil(expires)
		t.RevokedAt = timeOrNil(revoked)
		t.CreatedAt = time.Unix(created, 0)
		out = append(out, t)
	}
	return out, rows.Err()
}

// Revoke marks a token revoked. Scoped to the owner so one user cannot revoke
// another's token by guessing an id.
func (s *TokenStore) Revoke(ctx context.Context, id, userID int64) error {
	_, err := s.db.Writer.ExecContext(ctx,
		`UPDATE api_tokens SET revoked_at = ? WHERE id = ? AND user_id = ? AND revoked_at IS NULL`,
		s.now().Unix(), id, userID)
	if err != nil {
		return fmt.Errorf("revoke token %d: %w", id, err)
	}
	return nil
}

// RequireToken authenticates a bearer token and requires a scope, injecting the
// user (and scopes) into the context so downstream handlers use UserFromContext.
// It answers JSON, since it guards the /api routes.
func (s *TokenStore) RequireToken(scope string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bearer := bearerToken(r)
		if bearer == "" {
			writeJSONError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		user, scopes, err := s.Authenticate(r.Context(), bearer)
		if err != nil {
			writeJSONError(w, http.StatusUnauthorized, "invalid or expired token")
			return
		}
		if !scopeSatisfies(scopes, scope) {
			writeJSONError(w, http.StatusForbidden, "token lacks the "+scope+" scope")
			return
		}
		ctx := context.WithValue(r.Context(), userContextKey{}, user)
		ctx = context.WithValue(ctx, tokenScopesKey{}, scopes)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

type tokenScopesKey struct{}

// bearerToken extracts the token from an Authorization: Bearer header.
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// scopeSatisfies implements the read < write < admin hierarchy.
func scopeSatisfies(have []string, need string) bool {
	rank := map[string]int{ScopeRead: 1, ScopeWrite: 2, ScopeAdmin: 3}
	needRank := rank[need]
	best := 0
	for _, s := range have {
		if r := rank[strings.TrimSpace(s)]; r > best {
			best = r
		}
	}
	return best >= needRank && needRank > 0
}

// normaliseScopes keeps only known scopes, always includes read, and dedupes.
func normaliseScopes(scopes []string) []string {
	set := map[string]bool{ScopeRead: true}
	for _, s := range scopes {
		switch strings.TrimSpace(s) {
		case ScopeWrite:
			set[ScopeWrite] = true
		case ScopeAdmin:
			set[ScopeAdmin] = true
		}
	}
	out := []string{ScopeRead}
	if set[ScopeWrite] {
		out = append(out, ScopeWrite)
	}
	if set[ScopeAdmin] {
		out = append(out, ScopeAdmin)
	}
	return out
}

// writeJSONError renders a JSON error, the content type the /api routes speak.
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"error":` + strconvQuote(msg) + `}`))
}

// strconvQuote is a tiny JSON string quoter for the fixed messages above.
func strconvQuote(s string) string {
	b := make([]byte, 0, len(s)+2)
	b = append(b, '"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b = append(b, '\\', byte(r))
		default:
			b = append(b, string(r)...)
		}
	}
	b = append(b, '"')
	return string(b)
}

func unixOrNil(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.Unix()
}

func timeOrNil(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := time.Unix(v.Int64, 0)
	return &t
}
