package search

import (
	"context"
	"strings"
	"testing"
)

// fakeResolver treats configured tokens as tags resolving to fixed id sets;
// everything else is unknown (so it falls through to free text).
type fakeResolver map[string][]int64

func (f fakeResolver) ResolveTag(_ context.Context, token string) ([]int64, bool, error) {
	ids, ok := f[token]
	return ids, ok, nil
}

func compile(t *testing.T, query string, r TagResolver) Compiled {
	t.Helper()
	q, err := Parse(query)
	if err != nil {
		t.Fatalf("parse %q: %v", query, err)
	}
	c, err := Compile(context.Background(), q, "a", r)
	if err != nil {
		t.Fatalf("compile %q: %v", query, err)
	}
	return c
}

func TestCompileFreeText(t *testing.T) {
	c := compile(t, "sword", fakeResolver{})
	if !strings.Contains(c.SQL, "assets_fts MATCH ?") {
		t.Errorf("SQL = %q", c.SQL)
	}
	if len(c.Args) != 1 || c.Args[0] != `"sword"*` {
		t.Errorf("args = %#v", c.Args)
	}
}

func TestCompilePhrase(t *testing.T) {
	c := compile(t, `"laser turret"`, fakeResolver{})
	if len(c.Args) != 1 || c.Args[0] != `"laser turret"` {
		t.Errorf("args = %#v", c.Args)
	}
}

func TestCompileAliasWordBecomesTag(t *testing.T) {
	// "cc0" is a known alias -> tag membership, not free text.
	c := compile(t, "cc0", fakeResolver{"cc0": {5}})
	if strings.Contains(c.SQL, "assets_fts") {
		t.Errorf("alias word treated as free text: %q", c.SQL)
	}
	if !strings.Contains(c.SQL, "asset_tags") || !strings.Contains(c.SQL, "pack_tags") {
		t.Errorf("tag membership missing: %q", c.SQL)
	}
	if len(c.Args) != 2 { // id repeated for asset_tags and pack_tags
		t.Errorf("args = %#v", c.Args)
	}
}

func TestCompileTagExpandsToDescendants(t *testing.T) {
	c := compile(t, "type:sfx", fakeResolver{"type:sfx": {1, 2, 3}})
	// 3 ids twice = 6 args.
	if len(c.Args) != 6 {
		t.Errorf("args = %#v", c.Args)
	}
}

func TestCompileUnknownTagMatchesNothing(t *testing.T) {
	c := compile(t, "theme:nonexistent", fakeResolver{})
	if !strings.Contains(c.SQL, "0 = 1") {
		t.Errorf("unknown tag should match nothing: %q", c.SQL)
	}
}

func TestCompileNegation(t *testing.T) {
	c := compile(t, "-has:alpha", fakeResolver{})
	if !strings.Contains(c.SQL, "NOT (") {
		t.Errorf("negation missing: %q", c.SQL)
	}
	if !strings.Contains(c.SQL, "has_alpha") {
		t.Errorf("column missing: %q", c.SQL)
	}
}

func TestCompileKindAndField(t *testing.T) {
	c := compile(t, "type:model width:>=64", fakeResolver{})
	if !strings.Contains(c.SQL, "a.kind = ?") {
		t.Errorf("kind missing: %q", c.SQL)
	}
	if !strings.Contains(c.SQL, "a.width >= ?") {
		t.Errorf("field missing: %q", c.SQL)
	}
	// model, 64
	if len(c.Args) != 2 || c.Args[0] != "model" || c.Args[1].(float64) != 64 {
		t.Errorf("args = %#v", c.Args)
	}
}

func TestCompileOrGroups(t *testing.T) {
	c := compile(t, "type:model OR type:audio", fakeResolver{})
	if !strings.Contains(c.SQL, " OR ") {
		t.Errorf("OR missing: %q", c.SQL)
	}
	// Each group parenthesised and both KindTerms present.
	if strings.Count(c.SQL, "a.kind = ?") != 2 {
		t.Errorf("expected two kind clauses: %q", c.SQL)
	}
}

func TestCompileEmpty(t *testing.T) {
	// A field with no column behind it constrains nothing, and the compiler must produce no
	// SQL at all rather than a clause that quietly matches everything.
	//
	// `tris:` and `duration:` used to be the example here; M16 connected them to the columns
	// M5 and M6 added, so `acquired:` — a pack-level provenance date this compiler cannot
	// join to yet — is what is left.
	c := compile(t, "acquired:>2026-01", fakeResolver{})
	if c.SQL != "" {
		t.Errorf("all-no-op query should compile to empty, got %q", c.SQL)
	}
}

// The model and audio filters are real (M16): they must produce SQL against the columns M5
// and M6 added, not silently match the whole library.
func TestCompileModelAndAudioFields(t *testing.T) {
	c := compile(t, "tris:<5000 materials:>1 duration:>1000", fakeResolver{})
	for _, want := range []string{"a.tri_count < ?", "a.material_count > ?", "a.duration_ms > ?"} {
		if !strings.Contains(c.SQL, want) {
			t.Errorf("SQL = %q, want it to contain %q", c.SQL, want)
		}
	}
}

// A pixel size compiles to an exact match on both columns (M16).
func TestCompileDimensions(t *testing.T) {
	c := compile(t, "32x32", fakeResolver{})
	if !strings.Contains(c.SQL, "a.width = ?") || !strings.Contains(c.SQL, "a.height = ?") {
		t.Errorf("SQL = %q", c.SQL)
	}
	if len(c.Args) != 2 || c.Args[0] != 32 || c.Args[1] != 32 {
		t.Errorf("Args = %#v, want [32 32]", c.Args)
	}
}

func TestCompileStylePixelArt(t *testing.T) {
	c := compile(t, "style:pixel-art", fakeResolver{})
	if !strings.Contains(c.SQL, "is_pixel_art") {
		t.Errorf("SQL = %q", c.SQL)
	}
}
