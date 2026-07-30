// Package savedsearch stores the §7 saved searches: named, pinnable query
// expressions. It is deliberately tiny — the query is kept as the raw text a
// person typed and re-parsed on use, so it stays a plain string here.
package savedsearch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/datcal/ambar/internal/db"
)

// ErrEmpty means a save was attempted with no name or no query.
var ErrEmpty = errors.New("saved search needs a name and a query")

// SavedSearch is one stored query.
type SavedSearch struct {
	ID        int64
	Name      string
	Query     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Store reads and writes saved searches.
type Store struct {
	db  *db.DB
	now func() time.Time
}

// NewStore wraps a database.
func NewStore(database *db.DB) *Store {
	return &Store{db: database, now: time.Now}
}

// WithClock replaces the clock, for tests.
func (s *Store) WithClock(now func() time.Time) *Store {
	s.now = now
	return s
}

// List returns all saved searches, most recently updated first.
func (s *Store) List(ctx context.Context) ([]SavedSearch, error) {
	rows, err := s.db.Reader.QueryContext(ctx,
		`SELECT id, name, query, created_at, updated_at FROM saved_searches ORDER BY updated_at DESC, name`)
	if err != nil {
		return nil, fmt.Errorf("list saved searches: %w", err)
	}
	defer rows.Close()

	var out []SavedSearch
	for rows.Next() {
		var (
			ss               SavedSearch
			created, updated int64
		)
		if err := rows.Scan(&ss.ID, &ss.Name, &ss.Query, &created, &updated); err != nil {
			return nil, err
		}
		ss.CreatedAt = time.Unix(created, 0)
		ss.UpdatedAt = time.Unix(updated, 0)
		out = append(out, ss)
	}
	return out, rows.Err()
}

// Save creates or updates a saved search by name. Re-saving under an existing
// name updates its query, so a person iterating on "my sci-fi props" gets one
// entry, not a pile.
func (s *Store) Save(ctx context.Context, name, query string) (SavedSearch, error) {
	name = strings.TrimSpace(name)
	query = strings.TrimSpace(query)
	if name == "" || query == "" {
		return SavedSearch{}, ErrEmpty
	}
	now := s.now().Unix()
	res, err := s.db.Writer.ExecContext(ctx, `
		INSERT INTO saved_searches (name, query, created_at, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET query = excluded.query, updated_at = excluded.updated_at`,
		name, query, now, now)
	if err != nil {
		return SavedSearch{}, fmt.Errorf("save search %q: %w", name, err)
	}
	// LastInsertId is 0 on the update path, so read the row back by its unique name.
	_ = res
	var ss SavedSearch
	var created, updated int64
	if err := s.db.Reader.QueryRowContext(ctx,
		`SELECT id, name, query, created_at, updated_at FROM saved_searches WHERE name = ?`, name).
		Scan(&ss.ID, &ss.Name, &ss.Query, &created, &updated); err != nil {
		return SavedSearch{}, fmt.Errorf("read back saved search %q: %w", name, err)
	}
	ss.CreatedAt = time.Unix(created, 0)
	ss.UpdatedAt = time.Unix(updated, 0)
	return ss, nil
}

// Delete removes a saved search. Removing one that does not exist is not an error.
func (s *Store) Delete(ctx context.Context, id int64) error {
	if _, err := s.db.Writer.ExecContext(ctx, `DELETE FROM saved_searches WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete saved search %d: %w", id, err)
	}
	return nil
}
