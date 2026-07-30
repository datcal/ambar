package savedsearch

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/datcal/ambar/internal/db"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	if _, err := database.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	fixed := time.Unix(1_700_000_000, 0)
	return NewStore(database).WithClock(func() time.Time { return fixed })
}

func TestSaveListDelete(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	ss, err := s.Save(ctx, "sci-fi props", "type:model theme:sci-fi")
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if ss.ID == 0 || ss.Query != "type:model theme:sci-fi" {
		t.Errorf("saved = %+v", ss)
	}

	// Re-saving the same name updates rather than duplicating.
	if _, err := s.Save(ctx, "sci-fi props", "type:model theme:sci-fi -style:realistic"); err != nil {
		t.Fatalf("re-save: %v", err)
	}
	list, err := s.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("list len = %d, want 1", len(list))
	}
	if list[0].Query != "type:model theme:sci-fi -style:realistic" {
		t.Errorf("query not updated: %q", list[0].Query)
	}

	if err := s.Delete(ctx, list[0].ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if list, _ := s.List(ctx); len(list) != 0 {
		t.Errorf("still %d after delete", len(list))
	}
}

func TestSaveRejectsEmpty(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if _, err := s.Save(ctx, "", "type:model"); !errors.Is(err, ErrEmpty) {
		t.Errorf("empty name err = %v, want ErrEmpty", err)
	}
	if _, err := s.Save(ctx, "name", "  "); !errors.Is(err, ErrEmpty) {
		t.Errorf("empty query err = %v, want ErrEmpty", err)
	}
}
