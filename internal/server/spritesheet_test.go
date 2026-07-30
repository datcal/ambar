package server

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

// insertSheetAsset inserts an image detected as a spritesheet.
func (ts *testServer) insertSheetAsset(t *testing.T, filename string) int64 {
	t.Helper()
	now := time.Now().Unix()
	ts.db.Writer.Exec(`
		INSERT INTO packs (name, slug, kind, library_rel_path, first_seen_at, last_seen_at, created_at, updated_at)
		VALUES ('sprites', 'sprites', 'folder', 'sprites', ?, ?, ?, ?)
		ON CONFLICT(library_rel_path) DO NOTHING`, now, now, now, now)
	var packID int64
	ts.db.Reader.QueryRow(`SELECT id FROM packs WHERE library_rel_path = 'sprites'`).Scan(&packID)

	res, err := ts.db.Writer.Exec(`
		INSERT INTO assets (pack_id, rel_path, filename, ext, kind, size, mtime, sha256,
		                    width, height,
		                    frame_cols, frame_rows, frame_w, frame_h, frame_count, frame_source,
		                    derive_state, derive_version,
		                    first_seen_at, last_verified_at, created_at, updated_at)
		VALUES (?, ?, ?, 'png', 'image', 4, ?, ?, 80, 60, 4, 3, 20, 20, 12, 'detected', 'ok', 4, ?, ?, ?, ?)`,
		packID, filename, filename, now, filename+"-hash", now, now, now, now)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	ts.db.Writer.Exec(`INSERT INTO assets_fts (rowid, filename, pack_name, tag_text, notes) VALUES (?, ?, 'sprites', '', '')`, id, filename)
	return id
}

func TestSpritesheetConfirmFlow(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	id := ts.insertSheetAsset(t, "hero_sheet.png")

	// The detail page shows the grid as detected, awaiting confirmation.
	detail := readBody(t, ts.get(t, itoa("/assets/%d", id)))
	for _, want := range []string{"sheet-line", "please confirm", "4 × 3 = 12 frames"} {
		if !strings.Contains(detail, want) {
			t.Errorf("sheet panel missing %q", want)
		}
	}

	// Correct it to a 2×3 grid; it becomes a confirmed manual value.
	status, _ := ts.postForm(t, itoa("/assets/%d/frames", id), url.Values{"cols": {"2"}, "rows": {"3"}})
	if status != 200 && status != 303 {
		t.Fatalf("frames status = %d", status)
	}
	var cols, rows, frameW int
	var source string
	ts.db.Reader.QueryRow(`SELECT frame_cols, frame_rows, frame_w, frame_source FROM assets WHERE id = ?`, id).
		Scan(&cols, &rows, &frameW, &source)
	if cols != 2 || rows != 3 || source != "manual" || frameW != 40 {
		t.Errorf("after correction: cols=%d rows=%d frameW=%d source=%q, want 2/3/40/manual", cols, rows, frameW, source)
	}

	// Now the detail page shows it as confirmed (manual), not "please confirm".
	detail = readBody(t, ts.get(t, itoa("/assets/%d", id)))
	if strings.Contains(detail, "please confirm") {
		t.Errorf("still asking to confirm after a manual correction")
	}
	if !strings.Contains(detail, ">manual<") {
		t.Errorf("manual source badge missing")
	}
}

// TestSpritesheetLineStatesWhatItIsFor: M14 gave the panel an explanation and a
// player; M15 removed the panel from the detail page — the animation already plays in
// the viewer above, and the space belongs to the palette and the tags. What must
// survive is the one thing a person cannot guess: what confirming the grid is *for*.
func TestSpritesheetLineStatesWhatItIsFor(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	id := ts.insertSheetAsset(t, "walk.png")

	body := readBody(t, ts.get(t, itoa("/assets/%d", id)))

	// The facts, and why they matter.
	for _, want := range []string{
		"sheet-line",
		"frames",
		"animated preview",
		"AnimatedSprite2D",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the spritesheet line is missing %q", want)
		}
	}
	// The control is still one click.
	if !strings.Contains(body, itoa("/assets/%d/frames", id)) {
		t.Error("the confirm control is missing")
	}
	// And the page no longer carries a second player: the viewer above plays it.
	if strings.Contains(body, "sheet-player") || strings.Contains(body, "/static/sheet.js") {
		t.Error("the detail page should not carry a second animation player")
	}
}
