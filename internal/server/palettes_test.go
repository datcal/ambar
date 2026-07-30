package server

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// seedPackPalettes indexes two packs and writes swatch rows for them: one pair that
// shares an art direction, one that does not.
func (ts *testServer) seedPackPalettes(t *testing.T) (rust, forest int64) {
	t.Helper()
	ts.seedLibrary(t, map[string]string{
		"rust-pack/wall.png":   "a",
		"rust-pack/floor.png":  "b",
		"forest-pack/tree.png": "c",
	})

	swatches := func(libPath string, colours ...[3]int) int64 {
		id := ts.assetID(t, libPath)
		for i, c := range colours {
			if _, err := ts.db.Writer.Exec(`
				INSERT INTO asset_swatches (asset_id, rank, r, g, b, ratio) VALUES (?, ?, ?, ?, ?, ?)`,
				id, i, c[0], c[1], c[2], 0.5); err != nil {
				t.Fatal(err)
			}
		}
		// The view only aggregates assets that have a palette at all.
		if _, err := ts.db.Writer.Exec(
			`UPDATE assets SET palette_json = '[]', palette_kind = 'exact' WHERE id = ?`, id); err != nil {
			t.Fatal(err)
		}
		var packID int64
		if err := ts.db.Reader.QueryRow(`SELECT pack_id FROM assets WHERE id = ?`, id).Scan(&packID); err != nil {
			t.Fatal(err)
		}
		return packID
	}

	rust = swatches("rust-pack/wall.png", [3]int{0x8b, 0x3a, 0x3a}, [3]int{0x4a, 0x3b, 0x2f})
	swatches("rust-pack/floor.png", [3]int{0x8d, 0x3c, 0x3d}, [3]int{0xd9, 0xc7, 0xa1})
	forest = swatches("forest-pack/tree.png", [3]int{0x2f, 0x6b, 0x35}, [3]int{0x17, 0x3a, 0x1c})
	return rust, forest
}

func TestPalettesPageListsPacks(t *testing.T) {
	ts := removalTestServer(t)
	ts.seedPackPalettes(t)

	resp := ts.get(t, "/palettes")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := ts.body(t, resp)
	for _, want := range []string{"rust-pack", "forest-pack", "palette-strip", "Compare two packs"} {
		if !strings.Contains(body, want) {
			t.Errorf("page is missing %q", want)
		}
	}
	// Chips carry their colour as data, never as an inline style (§11 CSP).
	if !strings.Contains(body, `data-hex="#8`) {
		t.Errorf("expected a data-hex chip:\n%s", body)
	}
	if strings.Contains(body, "style=") {
		t.Error("no inline style attribute may appear on this page")
	}
}

func TestPalettesComparisonAnswersTheQuestion(t *testing.T) {
	ts := removalTestServer(t)
	rust, forest := ts.seedPackPalettes(t)

	// Two packs with nothing in common: the view says so, in words.
	body := ts.body(t, ts.get(t, itoa("/palettes?a=%d&b=%d", rust, forest)))
	if !strings.Contains(body, "next to") {
		t.Fatalf("comparison heading missing:\n%s", body)
	}
	if !strings.Contains(body, "different palettes") {
		t.Errorf("expected the mismatch verdict:\n%s", body)
	}
	if !strings.Contains(body, "Only in") {
		t.Error("the unique colours should be shown")
	}

	// A pack against itself is refused rather than reported as a perfect match, which
	// would be true and useless.
	body = ts.body(t, ts.get(t, itoa("/palettes?a=%d&b=%d", rust, rust)))
	if !strings.Contains(body, "two different packs") {
		t.Errorf("comparing a pack with itself should be refused:\n%s", body)
	}

	// Half a comparison is a prompt, not an error.
	body = ts.body(t, ts.get(t, itoa("/palettes?a=%d", rust)))
	if !strings.Contains(body, "both sides") {
		t.Errorf("expected a prompt for the second pack:\n%s", body)
	}
}

func TestPalettesComparisonToleranceIsHonoured(t *testing.T) {
	ts := removalTestServer(t)
	rust, forest := ts.seedPackPalettes(t)

	// At a tolerance wide enough to call every colour the same, the two packs agree.
	body := ts.body(t, ts.get(t, itoa("/palettes?a=%d&b=%d&tolerance=255", rust, forest)))
	if !strings.Contains(body, "±255 per channel") {
		t.Errorf("the tolerance must be stated:\n%s", body)
	}
	if !strings.Contains(body, "should sit together well") {
		t.Errorf("at maximum tolerance everything matches:\n%s", body)
	}

	// A nonsense tolerance falls back to the default rather than erroring.
	resp := ts.get(t, itoa("/palettes?a=%d&b=%d&tolerance=notanumber", rust, forest))
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestPalettesIgnoresPacksWithNoPaletteData(t *testing.T) {
	ts := removalTestServer(t)
	ts.seedLibrary(t, map[string]string{"plain-pack/x.png": "a"})

	body := ts.body(t, ts.get(t, "/palettes"))
	if !strings.Contains(body, "no palette data yet") {
		t.Errorf("a pack with nothing derived should say so:\n%s", body)
	}

	// And it cannot be compared: an unanalysed pack is not in the picker.
	var packID int64
	if err := ts.db.Reader.QueryRowContext(context.Background(),
		`SELECT id FROM packs WHERE library_rel_path = 'plain-pack'`).Scan(&packID); err != nil {
		t.Fatal(err)
	}
	body = ts.body(t, ts.get(t, itoa("/palettes?a=%d&b=%d", packID, packID+1)))
	if !strings.Contains(body, "no palette data yet") && !strings.Contains(body, "not enough palette data") {
		t.Errorf("comparing an unanalysed pack must say what is missing:\n%s", body)
	}
}
