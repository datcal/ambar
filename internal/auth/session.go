package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/datcal/ambar/internal/db"
)

// Session lifetimes. §11 requires both an absolute and an idle expiry: the
// absolute one bounds a stolen cookie's usefulness even on an active session,
// the idle one logs out a forgotten browser tab.
const (
	DefaultAbsoluteTTL = 30 * 24 * time.Hour
	DefaultIdleTTL     = 7 * 24 * time.Hour

	// SessionCookieName is prefixed to make it obvious in a browser inspector
	// which application owns it.
	SessionCookieName = "ambar_session"

	sessionTokenBytes = 32
)

// ErrNoSession covers "no such session", "expired" and "user gone". Callers
// treat all three identically: not logged in.
var ErrNoSession = errors.New("no valid session")

// Session is a row of the sessions table.
type Session struct {
	ID            int64
	UserID        int64
	CreatedAt     time.Time
	ExpiresAt     time.Time
	IdleExpiresAt time.Time
}

// SessionStore issues, validates and revokes sessions.
type SessionStore struct {
	db          *db.DB
	absoluteTTL time.Duration
	idleTTL     time.Duration
	now         func() time.Time
}

func NewSessionStore(d *db.DB) *SessionStore {
	return &SessionStore{
		db:          d,
		absoluteTTL: DefaultAbsoluteTTL,
		idleTTL:     DefaultIdleTTL,
		now:         time.Now,
	}
}

// WithClock replaces the clock. For tests; expiry is otherwise untestable
// without sleeping for a week.
func (s *SessionStore) WithClock(now func() time.Time) *SessionStore {
	s.now = now
	return s
}

// WithTTL overrides both lifetimes.
func (s *SessionStore) WithTTL(absolute, idle time.Duration) *SessionStore {
	s.absoluteTTL, s.idleTTL = absolute, idle
	return s
}

// AbsoluteTTL is the cookie's Max-Age.
func (s *SessionStore) AbsoluteTTL() time.Duration { return s.absoluteTTL }

// Create issues a session and returns the cookie value.
//
// The returned token is the only time the plaintext exists; the database stores
// its SHA-256, so a leaked database backup does not hand over live sessions.
func (s *SessionStore) Create(ctx context.Context, userID int64, userAgent, ip string) (string, Session, error) {
	return s.create(ctx, s.db.Writer, userID, userAgent, ip)
}

func (s *SessionStore) create(ctx context.Context, ex execer, userID int64, userAgent, ip string) (string, Session, error) {
	raw := make([]byte, sessionTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", Session{}, fmt.Errorf("generate session token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)

	now := s.now()
	sess := Session{
		UserID:    userID,
		CreatedAt: now,
		ExpiresAt: now.Add(s.absoluteTTL),
	}
	// Clamped at creation, not just when sliding, so idle_expires_at <=
	// expires_at holds for every row. Otherwise a configuration where the idle
	// window is longer than the absolute one stores an idle expiry that can
	// never be reached, and the table stops meaning what it says.
	sess.IdleExpiresAt = now.Add(s.idleTTL)
	if sess.IdleExpiresAt.After(sess.ExpiresAt) {
		sess.IdleExpiresAt = sess.ExpiresAt
	}

	// A very long User-Agent is an attacker's cheap way to bloat the table.
	if len(userAgent) > 512 {
		userAgent = userAgent[:512]
	}

	res, err := ex.ExecContext(ctx, `
		INSERT INTO sessions (user_id, token_hash, created_at, expires_at, idle_expires_at, user_agent, ip)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		userID, hashToken(token), now.Unix(), sess.ExpiresAt.Unix(), sess.IdleExpiresAt.Unix(), userAgent, ip)
	if err != nil {
		return "", Session{}, fmt.Errorf("insert session: %w", err)
	}
	if sess.ID, err = res.LastInsertId(); err != nil {
		return "", Session{}, fmt.Errorf("read new session id: %w", err)
	}
	return token, sess, nil
}

// Lookup validates a cookie value and returns the session and its user.
//
// Expired rows are rejected but not deleted here: the read path stays a read,
// and DeleteExpired at startup does the tidying. An expired row is never live,
// so leaving it in place is not a security question.
func (s *SessionStore) Lookup(ctx context.Context, token string) (Session, User, error) {
	if token == "" {
		return Session{}, User{}, ErrNoSession
	}

	var (
		sess            Session
		user            User
		created         int64
		expires         int64
		idleExpires     int64
		userCreated     int64
		userUpdated     int64
		userLastLoginAt sql.NullInt64
	)
	err := s.db.Reader.QueryRowContext(ctx, `
		SELECT s.id, s.user_id, s.created_at, s.expires_at, s.idle_expires_at,
		       u.id, u.username, u.password_hash, u.role, u.created_at, u.updated_at, u.last_login_at
		FROM sessions s
		JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = ?`, hashToken(token),
	).Scan(
		&sess.ID, &sess.UserID, &created, &expires, &idleExpires,
		&user.ID, &user.Username, &user.PasswordHash, &user.Role,
		&userCreated, &userUpdated, &userLastLoginAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, User{}, ErrNoSession
	}
	if err != nil {
		return Session{}, User{}, fmt.Errorf("look up session: %w", err)
	}

	sess.CreatedAt = time.Unix(created, 0)
	sess.ExpiresAt = time.Unix(expires, 0)
	sess.IdleExpiresAt = time.Unix(idleExpires, 0)

	now := s.now()
	if !now.Before(sess.ExpiresAt) || !now.Before(sess.IdleExpiresAt) {
		return Session{}, User{}, ErrNoSession
	}

	user.CreatedAt = time.Unix(userCreated, 0)
	user.UpdatedAt = time.Unix(userUpdated, 0)
	if userLastLoginAt.Valid {
		t := time.Unix(userLastLoginAt.Int64, 0)
		user.LastLoginAt = &t
	}

	// Slide the idle window, but only past the halfway mark, so an active
	// session costs one write per few days rather than one write per request.
	if remaining := sess.IdleExpiresAt.Sub(now); remaining < s.idleTTL/2 {
		next := now.Add(s.idleTTL)
		// Never past the absolute expiry.
		if next.After(sess.ExpiresAt) {
			next = sess.ExpiresAt
		}
		if _, err := s.db.Writer.ExecContext(ctx,
			`UPDATE sessions SET idle_expires_at = ? WHERE id = ?`, next.Unix(), sess.ID); err != nil {
			return Session{}, User{}, fmt.Errorf("slide session idle expiry: %w", err)
		}
		sess.IdleExpiresAt = next
	}

	return sess, user, nil
}

// Rotate issues a new session and invalidates oldToken, atomically.
//
// §11 requires rotation on login: the pre-login token is often one an attacker
// planted, and reusing it is session fixation.
func (s *SessionStore) Rotate(ctx context.Context, oldToken string, userID int64, userAgent, ip string) (string, Session, error) {
	tx, err := s.db.Writer.BeginTx(ctx, nil)
	if err != nil {
		return "", Session{}, fmt.Errorf("begin session rotation: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed

	if oldToken != "" {
		if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, hashToken(oldToken)); err != nil {
			return "", Session{}, fmt.Errorf("delete previous session: %w", err)
		}
	}
	token, sess, err := s.create(ctx, tx, userID, userAgent, ip)
	if err != nil {
		return "", Session{}, err
	}
	if err := tx.Commit(); err != nil {
		return "", Session{}, fmt.Errorf("commit session rotation: %w", err)
	}
	return token, sess, nil
}

// Destroy revokes one session. Unknown tokens are not an error, so a double
// logout is harmless.
func (s *SessionStore) Destroy(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	if _, err := s.db.Writer.ExecContext(ctx,
		`DELETE FROM sessions WHERE token_hash = ?`, hashToken(token)); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

// DestroyAllForUser revokes every session belonging to a user.
func (s *SessionStore) DestroyAllForUser(ctx context.Context, userID int64) error {
	if _, err := s.db.Writer.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID); err != nil {
		return fmt.Errorf("delete sessions for user %d: %w", userID, err)
	}
	return nil
}

// DeleteExpired removes rows that can no longer authenticate anything.
//
// Called once at startup, not from a background ticker. Nothing in this
// application deletes on a schedule — see rule 3 in ARCHITECTURE.md. These rows
// are already dead as far as Lookup is concerned, so this is bookkeeping.
func (s *SessionStore) DeleteExpired(ctx context.Context) (int64, error) {
	now := s.now().Unix()
	res, err := s.db.Writer.ExecContext(ctx,
		`DELETE FROM sessions WHERE expires_at <= ? OR idle_expires_at <= ?`, now, now)
	if err != nil {
		return 0, fmt.Errorf("delete expired sessions: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil // the delete succeeded; the count is only for logging
	}
	return n, nil
}

// SetSessionCookie writes the session cookie.
//
// secure comes from config.CookieSecure. See the comment on that field for why
// it is not unconditionally true.
func SetSessionCookie(w http.ResponseWriter, token string, secure bool, maxAge time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   int(maxAge.Seconds()),
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// ClearSessionCookie expires the cookie in the browser. The server-side row
// must be deleted separately — that is what actually ends the session.
func ClearSessionCookie(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// hashToken is what the database stores. SHA-256 without a salt is correct
// here: the input is 32 bytes of CSPRNG output, so there is nothing to
// brute-force and no need for a slow KDF on every request.
func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// execer is satisfied by *sql.DB and *sql.Tx, so Create works inside Rotate's
// transaction.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}
