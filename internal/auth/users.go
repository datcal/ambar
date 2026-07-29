package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/datcal/ambar/internal/db"
)

// RoleUser is the only role that exists. §11: keep the column, ship exactly one
// role plus an implicit owner, and do not build a permission system for two
// people who trust each other.
const RoleUser = "user"

var (
	ErrUserNotFound = errors.New("no such user")
	ErrUserExists   = errors.New("username already taken")
)

// usernamePattern keeps usernames to things that are unambiguous in a URL, a
// log line and a shell command.
var usernamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{1,63}$`)

// User is a row of the users table. PasswordHash is carried so the login
// handler can verify it; it is never rendered.
type User struct {
	ID           int64
	Username     string
	PasswordHash string
	Role         string
	CreatedAt    time.Time
	UpdatedAt    time.Time
	LastLoginAt  *time.Time
}

// UserStore reads and writes the users table.
type UserStore struct {
	db *db.DB
}

func NewUserStore(d *db.DB) *UserStore { return &UserStore{db: d} }

// NormalizeUsername is applied on both write and lookup, so "Burak" and
// " burak " are the same account rather than two.
func NormalizeUsername(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// ValidateUsername checks an already-normalized username.
func ValidateUsername(s string) error {
	if s == "" {
		return errors.New("username must not be empty")
	}
	if !usernamePattern.MatchString(s) {
		return fmt.Errorf("username %q must be 2-64 characters of a-z, 0-9, dot, dash or underscore, "+
			"starting with a letter or digit", s)
	}
	return nil
}

// Create adds a user. There is no self-registration (§11); this is reached only
// from `ambar user add`.
func (s *UserStore) Create(ctx context.Context, username, password, role string) (User, error) {
	username = NormalizeUsername(username)
	if err := ValidateUsername(username); err != nil {
		return User{}, err
	}
	if len([]rune(password)) < MinPasswordLength {
		return User{}, fmt.Errorf("password must be at least %d characters", MinPasswordLength)
	}
	if role == "" {
		role = RoleUser
	}
	if role != RoleUser {
		return User{}, fmt.Errorf("role %q does not exist; the only role is %q", role, RoleUser)
	}

	hash, err := HashPassword(password)
	if err != nil {
		return User{}, fmt.Errorf("hash password: %w", err)
	}

	now := time.Now().Unix()
	res, err := s.db.Writer.ExecContext(ctx, `
		INSERT INTO users (username, password_hash, role, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)`,
		username, hash, role, now, now)
	if err != nil {
		// The UNIQUE index is the real guard: a check-then-insert would race.
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return User{}, fmt.Errorf("%w: %s", ErrUserExists, username)
		}
		return User{}, fmt.Errorf("insert user: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return User{}, fmt.Errorf("read new user id: %w", err)
	}

	return User{
		ID:           id,
		Username:     username,
		PasswordHash: hash,
		Role:         role,
		CreatedAt:    time.Unix(now, 0),
		UpdatedAt:    time.Unix(now, 0),
	}, nil
}

const userColumns = `id, username, password_hash, role, created_at, updated_at, last_login_at`

// ByUsername returns ErrUserNotFound when there is no such account. The login
// handler must not let that distinction reach the response (§11).
func (s *UserStore) ByUsername(ctx context.Context, username string) (User, error) {
	row := s.db.Reader.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM users WHERE username = ?`, NormalizeUsername(username))
	return scanUser(row)
}

func (s *UserStore) ByID(ctx context.Context, id int64) (User, error) {
	row := s.db.Reader.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE id = ?`, id)
	return scanUser(row)
}

// Count is used by `ambar user add` messaging and by the CLI to point out that
// no users exist yet.
func (s *UserStore) Count(ctx context.Context) (int, error) {
	var n int
	if err := s.db.Reader.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count users: %w", err)
	}
	return n, nil
}

// List returns every user, oldest first.
func (s *UserStore) List(ctx context.Context) ([]User, error) {
	rows, err := s.db.Reader.QueryContext(ctx, `SELECT `+userColumns+` FROM users ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()

	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// TouchLogin records a successful login.
func (s *UserStore) TouchLogin(ctx context.Context, id int64) error {
	now := time.Now().Unix()
	if _, err := s.db.Writer.ExecContext(ctx,
		`UPDATE users SET last_login_at = ?, updated_at = ? WHERE id = ?`, now, now, id); err != nil {
		return fmt.Errorf("update last_login_at: %w", err)
	}
	return nil
}

// scanner is satisfied by both *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

func scanUser(row scanner) (User, error) {
	var (
		u         User
		created   int64
		updated   int64
		lastLogin sql.NullInt64
	)
	err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &created, &updated, &lastLogin)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrUserNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("scan user: %w", err)
	}
	u.CreatedAt = time.Unix(created, 0)
	u.UpdatedAt = time.Unix(updated, 0)
	if lastLogin.Valid {
		t := time.Unix(lastLogin.Int64, 0)
		u.LastLoginAt = &t
	}
	return u, nil
}
