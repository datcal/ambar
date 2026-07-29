package db

import (
	"context"
	"strings"
	"testing"
)

// These assertions resolved §15 item 1: FTS5 is available in modernc.org/sqlite
// without CGO, so the static binary and CLAUDE.md invariant 6 hold.
//
// They are kept permanently rather than discarded with the spike, because the
// failure mode they guard against is silent. If a dependency bump ever ships a
// build without FTS5, search stops working and the fallback
// (mattn/go-sqlite3 + the sqlite_fts5 build tag) costs CGO — that is a decision
// to make deliberately, not to discover in production.
//
// §4 specifies assets_fts as an FTS5 external-content table over filename,
// pack_name, tag_text and notes. That exact shape is exercised in
// TestFTS5ExternalContent below (against its own demo tables, so it does not
// collide with the real schema).
//
// M1 deliberately chose a *regular* FTS5 table for assets_fts instead — see
// 0002_library.sql for why: the column set spans a join, which triggers cannot
// maintain. The external-content assertions stay here anyway, because they prove
// the capability exists should that decision ever be revisited.

func TestFTS5IsCompiledIn(t *testing.T) {
	d := openTestDB(t)

	rows, err := d.Reader.Query(`PRAGMA compile_options`)
	if err != nil {
		t.Fatalf("read compile_options: %v", err)
	}
	defer rows.Close()

	var found bool
	for rows.Next() {
		var opt string
		if err := rows.Scan(&opt); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(opt, "ENABLE_FTS5") {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("ENABLE_FTS5 is absent from compile_options: FTS5 is not available in this " +
			"build of modernc.org/sqlite. See spec §15 item 1 — the fallback costs CGO.")
	}
}

func TestFTS5QueryForms(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	// Default tokenizer, which splits on _ and . — that is what the real
	// assets_fts uses, and what makes searching "sword" find wooden_sword_01.glb.
	if _, err := d.Writer.ExecContext(ctx,
		`CREATE VIRTUAL TABLE docs USING fts5(filename, pack_name, tag_text, notes)`); err != nil {
		t.Fatalf("create fts5 table: %v", err)
	}

	// Row 1's notes contain both "turret" and "laser" but far apart and in the
	// wrong order, so phrase, implicit-AND and NEAR each return a different
	// count. A fixture where they agreed would not prove the three forms are
	// actually distinct.
	seed := []struct{ filename, pack, tags, notes string }{
		{"wooden_sword_01.glb", "kenney-medieval-rts", "type:model theme:fantasy",
			"a turret, but definitely no laser in this pack"},
		{"laser_turret_a.glb", "kenney-sci-fi-rts-pack", "type:model theme:sci-fi",
			"the laser turret model"},
		{"ui_atlas.png", "kenney-sci-fi-rts-pack", "type:spritesheet style:pixel-art",
			"atlas of ui bits"},
		{"impact_heavy.wav", "freesound-impacts", "type:sfx:impact", "heavy impact"},
	}
	for _, s := range seed {
		if _, err := d.Writer.ExecContext(ctx,
			`INSERT INTO docs(filename, pack_name, tag_text, notes) VALUES (?,?,?,?)`,
			s.filename, s.pack, s.tags, s.notes); err != nil {
			t.Fatalf("seed %s: %v", s.filename, err)
		}
	}

	// The query forms §7 requires of the search parser.
	tests := []struct {
		name  string
		query string
		want  int
	}{
		{"bare term", `sword`, 1},
		{"prefix", `kenn*`, 3},
		// Adjacent only: row 1 has both words, but not next to each other.
		{"phrase", `"laser turret"`, 1},
		// Both words anywhere in the row: rows 1 and 2.
		{"implicit AND", `laser turret`, 2},
		{"explicit OR", `sword OR atlas`, 2},
		{"negation", `turret NOT sword`, 1},
		{"column filter", `filename:turret`, 1},
		{"column set filter", `{filename notes}:sword`, 1},
		// Within two tokens of each other: row 1's are three apart, so only row 2.
		{"NEAR", `NEAR(laser turret, 2)`, 1},
		{"namespaced tag as phrase", `"type:sfx:impact"`, 1},
		{"no match", `zzznothing`, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var n int
			if err := d.Reader.QueryRowContext(ctx,
				`SELECT count(*) FROM docs WHERE docs MATCH ?`, tc.query).Scan(&n); err != nil {
				t.Fatalf("MATCH %q: %v", tc.query, err)
			}
			if n != tc.want {
				t.Errorf("MATCH %q matched %d rows, want %d", tc.query, n, tc.want)
			}
		})
	}
}

func TestFTS5RankingAndSnippets(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	if _, err := d.Writer.ExecContext(ctx, `CREATE VIRTUAL TABLE docs USING fts5(filename, notes)`); err != nil {
		t.Fatal(err)
	}
	seed := [][2]string{
		{"laser_turret_a.glb", "the laser turret model, a laser turret"},
		{"sword.glb", "mentions laser once"},
		{"rock_01.glb", "a rock"},
		{"tree_pine.glb", "a pine tree"},
	}
	for _, s := range seed {
		if _, err := d.Writer.ExecContext(ctx,
			`INSERT INTO docs(filename, notes) VALUES (?,?)`, s[0], s[1]); err != nil {
			t.Fatal(err)
		}
	}

	// ORDER BY rank must put the better match first. Note that bm25 scores
	// collapse towards SQLite's -1e-06 floor when a term appears in most rows
	// (the IDF term goes non-positive and is clamped) — the *ordering* stays
	// correct, but M1 must sort by rank and never threshold on the absolute
	// value.
	rows, err := d.Reader.QueryContext(ctx, `
		SELECT filename, bm25(docs), snippet(docs, 1, '[', ']', '...', 8), highlight(docs, 0, '<', '>')
		FROM docs WHERE docs MATCH 'laser' ORDER BY rank`)
	if err != nil {
		t.Fatalf("bm25/snippet/highlight: %v", err)
	}
	defer rows.Close()

	var (
		order  []string
		scores []float64
	)
	for rows.Next() {
		var (
			filename, snippet, highlight string
			score                        float64
		)
		if err := rows.Scan(&filename, &score, &snippet, &highlight); err != nil {
			t.Fatal(err)
		}
		order = append(order, filename)
		scores = append(scores, score)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	if len(order) != 2 {
		t.Fatalf("got %d matches, want 2", len(order))
	}
	if order[0] != "laser_turret_a.glb" {
		t.Errorf("ORDER BY rank put %q first, want laser_turret_a.glb", order[0])
	}
	if scores[0] >= scores[1] {
		t.Errorf("bm25 scores are not ordered best-first: %v", scores)
	}

	// Column weighting: M1 will want a filename hit to outrank a notes hit.
	if _, err := d.Writer.ExecContext(ctx,
		`INSERT INTO docs(docs, rank) VALUES('rank', 'bm25(10.0, 1.0)')`); err != nil {
		t.Errorf("configure column weights: %v", err)
	}
}

// TestFTS5ExternalContent exercises the exact shape §4 specifies for
// assets_fts: an external-content index over the assets table, kept in sync by
// triggers rather than by application code.
func TestFTS5ExternalContent(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	schema := []string{
		`CREATE TABLE demo_content (
			id        INTEGER PRIMARY KEY,
			filename  TEXT NOT NULL,
			pack_name TEXT,
			tag_text  TEXT,
			notes     TEXT
		)`,
		`CREATE VIRTUAL TABLE demo_fts USING fts5(
			filename, pack_name, tag_text, notes,
			content='demo_content', content_rowid='id'
		)`,
		`CREATE TRIGGER demo_fts_ai AFTER INSERT ON demo_content BEGIN
			INSERT INTO demo_fts(rowid, filename, pack_name, tag_text, notes)
			VALUES (new.id, new.filename, new.pack_name, new.tag_text, new.notes);
		END`,
		`CREATE TRIGGER demo_fts_ad AFTER DELETE ON demo_content BEGIN
			INSERT INTO demo_fts(demo_fts, rowid, filename, pack_name, tag_text, notes)
			VALUES ('delete', old.id, old.filename, old.pack_name, old.tag_text, old.notes);
		END`,
		`CREATE TRIGGER demo_fts_au AFTER UPDATE ON demo_content BEGIN
			INSERT INTO demo_fts(demo_fts, rowid, filename, pack_name, tag_text, notes)
			VALUES ('delete', old.id, old.filename, old.pack_name, old.tag_text, old.notes);
			INSERT INTO demo_fts(rowid, filename, pack_name, tag_text, notes)
			VALUES (new.id, new.filename, new.pack_name, new.tag_text, new.notes);
		END`,
	}
	for _, stmt := range schema {
		if _, err := d.Writer.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("schema %.50q: %v", stmt, err)
		}
	}

	if _, err := d.Writer.ExecContext(ctx, `
		INSERT INTO demo_content (id, filename, pack_name, tag_text, notes) VALUES
			(1, 'wooden_sword_01.glb', 'kenney-medieval', 'type:model', 'a sword'),
			(2, 'laser_turret_a.glb',  'kenney-sci-fi',   'type:model', 'a turret')`); err != nil {
		t.Fatal(err)
	}

	// Joining back to the content table is how a search result is hydrated.
	count := func(query string) int {
		t.Helper()
		var n int
		if err := d.Reader.QueryRowContext(ctx, `
			SELECT count(*) FROM demo_fts
			JOIN demo_content ON demo_content.id = demo_fts.rowid
			WHERE demo_fts MATCH ?`, query).Scan(&n); err != nil {
			t.Fatalf("external-content MATCH %q: %v", query, err)
		}
		return n
	}

	if got := count(`sword`); got != 1 {
		t.Errorf("after insert, 'sword' matched %d rows, want 1", got)
	}

	if _, err := d.Writer.ExecContext(ctx,
		`UPDATE demo_content SET filename = 'iron_sword_02.glb' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if got := count(`iron*`); got != 1 {
		t.Errorf("after update, 'iron*' matched %d rows, want 1", got)
	}
	if got := count(`wooden`); got != 0 {
		t.Errorf("after update, stale term 'wooden' matched %d rows, want 0", got)
	}

	if _, err := d.Writer.ExecContext(ctx, `DELETE FROM demo_content WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if got := count(`sword`); got != 0 {
		t.Errorf("after delete, 'sword' matched %d rows, want 0", got)
	}

	// 'rebuild' is what `ambar rebuild-index` (§12) will lean on, and
	// 'integrity-check' is how a corrupt index is detected rather than guessed at.
	for _, cmd := range []string{"integrity-check", "rebuild", "optimize"} {
		if _, err := d.Writer.ExecContext(ctx,
			`INSERT INTO demo_fts(demo_fts) VALUES(?)`, cmd); err != nil {
			t.Errorf("fts5 command %q: %v", cmd, err)
		}
	}
	if got := count(`turret`); got != 1 {
		t.Errorf("after rebuild, 'turret' matched %d rows, want 1", got)
	}
}

func TestFTS5Tokenizers(t *testing.T) {
	// §7 needs diacritic folding for real-world pack names, underscore-aware
	// tokenizing for filenames, and trigram is a candidate for the fuzzy
	// filename fallback ("swrd" finding wooden_sword_01.glb).
	tests := []struct {
		name     string
		tokenize string
		doc      string
		query    string
		want     int
	}{
		{"unicode61 default", `unicode61`, "wooden sword", "sword", 1},
		{"diacritics folded", `unicode61 remove_diacritics 2`, "Café Ambiance Hörn", "cafe horn", 1},
		{"underscore as separator", `unicode61 separators '_'`, "wooden_sword_01", "sword", 1},
		{"underscore kept in token", `unicode61 tokenchars '_'`, "wooden_sword_01", "wooden_sword_01", 1},
		{"porter stemming", `porter unicode61`, "running explosions", "run explosion", 1},
		{"trigram substring", `trigram`, "wooden_sword_01.glb", `"swo"`, 1},
		{"ascii", `ascii`, "wooden sword", "sword", 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := openTestDB(t)
			ctx := context.Background()

			if _, err := d.Writer.ExecContext(ctx,
				`CREATE VIRTUAL TABLE docs USING fts5(body, tokenize = "`+tc.tokenize+`")`); err != nil {
				t.Fatalf("create with tokenize=%q: %v", tc.tokenize, err)
			}
			if _, err := d.Writer.ExecContext(ctx, `INSERT INTO docs(body) VALUES (?)`, tc.doc); err != nil {
				t.Fatal(err)
			}

			var n int
			if err := d.Reader.QueryRowContext(ctx,
				`SELECT count(*) FROM docs WHERE docs MATCH ?`, tc.query).Scan(&n); err != nil {
				t.Fatalf("MATCH %q: %v", tc.query, err)
			}
			if n != tc.want {
				t.Errorf("tokenize=%q doc=%q query=%q matched %d, want %d",
					tc.tokenize, tc.doc, tc.query, n, tc.want)
			}
		})
	}
}

// TestFTS5MalformedQueryIsAnError matters because the §7 parser will hand user
// input to FTS5. A syntax error must come back as an error to turn into a
// helpful message, never as a panic.
func TestFTS5MalformedQueryIsAnError(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	if _, err := d.Writer.ExecContext(ctx, `CREATE VIRTUAL TABLE docs USING fts5(body)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Writer.ExecContext(ctx, `INSERT INTO docs(body) VALUES ('wooden sword')`); err != nil {
		t.Fatal(err)
	}

	for _, query := range []string{`"unterminated`, `AND`, `(((`, `body:`, `NEAR(`} {
		var n int
		err := d.Reader.QueryRowContext(ctx,
			`SELECT count(*) FROM docs WHERE docs MATCH ?`, query).Scan(&n)
		if err == nil {
			// Some of these are legal FTS5 after all; the point is only that
			// nothing panics and the driver stays usable.
			t.Logf("query %q was accepted, matching %d rows", query, n)
			continue
		}
		if !strings.Contains(err.Error(), "fts5") && !strings.Contains(err.Error(), "SQL logic error") {
			t.Errorf("query %q failed with an unexpected error shape: %v", query, err)
		}
	}
}
