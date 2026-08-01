package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// setPalette writes a palette directly onto an asset row. Derive is not run in
// these tests, so this stands in for what the deriver would store — the export
// handler and the detail panel both read the row, never the original file.
func (ts *testServer) setPalette(t *testing.T, id int64, kind, paletteJSON string) {
	t.Helper()
	if _, err := ts.db.Writer.ExecContext(context.Background(),
		`UPDATE assets SET palette_json = ?, palette_kind = ?, derive_state = 'ok' WHERE id = ?`,
		paletteJSON, kind, id); err != nil {
		t.Fatalf("set palette: %v", err)
	}
}

const exactPaletteJSON = `[{"hex":"#8b3a3a","r":139,"g":58,"b":58,"count":10,"ratio":0.5},` +
	`{"hex":"#000000","r":0,"g":0,"b":0,"count":6,"ratio":0.3},` +
	`{"hex":"#ffffff","r":255,"g":255,"b":255,"count":4,"ratio":0.2}]`

func paletteTestServer(t *testing.T) (*testServer, int64) {
	t.Helper()
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	ts.seedLibrary(t, map[string]string{"pack/hero.png": "not really a png"})
	id := ts.assetID(t, "pack/hero.png")
	ts.setPalette(t, id, "exact", exactPaletteJSON)
	return ts, id
}

func TestPalettePanelRendered(t *testing.T) {
	ts, id := paletteTestServer(t)

	resp := ts.get(t, itoa("/assets/%d", id))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := ts.body(t, resp)

	for _, want := range []string{
		`id="palette-panel"`,
		`exact · 3 colours`,
		`data-hex="#8b3a3a"`,
		`data-r="139"`,
		`/static/palette.js`,
		itoa("/assets/%d/palette/gpl", id),
	} {
		if !strings.Contains(body, want) {
			t.Errorf("detail page missing %q", want)
		}
	}
}

func TestPaletteApproximateBadge(t *testing.T) {
	ts, id := paletteTestServer(t)
	ts.setPalette(t, id, "quantized", exactPaletteJSON)

	body := ts.body(t, ts.get(t, itoa("/assets/%d", id)))
	if !strings.Contains(body, "approximate") {
		t.Error("a quantized palette should be labelled approximate")
	}
	if strings.Contains(body, "exact · ") {
		t.Error("a quantized palette must not claim to be exact")
	}
}

// A removed format is a 404, not a 500 or an empty file.
func TestPaletteExportRemovedFormats(t *testing.T) {
	ts, id := paletteTestServer(t)
	for _, format := range []string{"txt", "json", "css", "png"} {
		resp := ts.get(t, itoa("/assets/%d/palette/%s", id, format))
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf(".%s returned %d, want 404", format, resp.StatusCode)
		}
	}
}

func TestPaletteExportFormats(t *testing.T) {
	ts, id := paletteTestServer(t)

	// Three formats since M16: .gpl for Aseprite/GIMP/Krita, .gd and .tres for Godot. The
	// other four (.txt, .json, .css, .png) were removed with the links to them, and an old URL
	// for one now 404s — asserted below.
	cases := []struct {
		format      string
		contentType string
		wantFrag    string
	}{
		{"gpl", "text/plain; charset=utf-8", "GIMP Palette"},
		{"gd", "text/plain; charset=utf-8", "Color(0.545, 0.227, 0.227)"},
		{"tres", "text/plain; charset=utf-8", `type="Gradient"`},
	}
	for _, tc := range cases {
		t.Run(tc.format, func(t *testing.T) {
			resp := ts.get(t, itoa("/assets/%d/palette/%s", id, tc.format))
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			if ct := resp.Header.Get("Content-Type"); ct != tc.contentType {
				t.Errorf("Content-Type = %q, want %q", ct, tc.contentType)
			}
			cd := resp.Header.Get("Content-Disposition")
			if !strings.Contains(cd, "attachment") || !strings.Contains(cd, "hero-palette."+tc.format) {
				t.Errorf("Content-Disposition = %q, want attachment hero-palette.%s", cd, tc.format)
			}
			if body := ts.body(t, resp); !strings.Contains(body, tc.wantFrag) {
				t.Errorf("%s body missing %q:\n%s", tc.format, tc.wantFrag, body)
			}
		})
	}
}

func TestPaletteExportUnknownFormat(t *testing.T) {
	ts, id := paletteTestServer(t)
	if resp := ts.get(t, itoa("/assets/%d/palette/xcf", id)); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown format status = %d, want 404", resp.StatusCode)
	}
}

func TestPaletteExportNoPalette(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	// An asset that was never analysed: palette_json stays NULL, so HasPalette is
	// false and every export must 404 rather than emit an empty file.
	ts.seedLibrary(t, map[string]string{"pack/unanalysed.png": "no palette written"})
	id := ts.assetID(t, "pack/unanalysed.png")

	if resp := ts.get(t, itoa("/assets/%d/palette/gpl", id)); resp.StatusCode != http.StatusNotFound {
		t.Errorf("no-palette export status = %d, want 404", resp.StatusCode)
	}
}

// TestColourInputParsing: the notations a colour actually arrives in.
//
// "Elle gireceksem bari CSS gibi renk girelim" — and that is right, because a colour is
// usually a number you already have written down somewhere, not something to match by eye
// on a wheel. So the field takes what design tools and dev tools hand you.
func TestColourInputParsing(t *testing.T) {
	tests := []struct {
		in   string
		want string
		ok   bool
	}{
		{"#aabbcc", "aabbcc", true},
		{"aabbcc", "aabbcc", true},
		{"  #AABBCC  ", "aabbcc", true},
		{"#abc", "aabbcc", true},               // the shorthand people type from memory
		{"#aabbccff", "aabbcc", true},          // alpha dropped: a palette has no opacity
		{"rgb(170, 187, 204)", "aabbcc", true}, // what dev tools copy
		{"rgb(170 187 204)", "aabbcc", true},   // the modern space-separated form
		{"rgba(170, 187, 204, 0.5)", "aabbcc", true},
		{"", "", false},
		{"#gggggg", "", false},
		{"#aabb", "", false},
		{"rgb(300, 0, 0)", "", false}, // out of range is a typo, not a colour
		{"rgb(1, 2)", "", false},
		{"cornflowerblue", "", false}, // named colours would need a table; say no clearly
	}
	for _, tc := range tests {
		got, ok := parseColourInput(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("parseColourInput(%q) = %q, %v; want %q, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// TestColourSearchPrefersTheTypedValue: both inputs are always submitted, because the
// colour wheel has a value whether or not anyone touched it. The typed one has to win or
// typing a hex would be quietly overridden by whatever the wheel happened to be showing.
func TestColourSearchPrefersTheTypedValue(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)

	cases := []struct {
		name  string
		query string
		want  string
	}{
		{"typed wins", "/colour?hex=%234f8ef7&typed=%23aabbcc", "color%3Aaabbcc"},
		{"the wheel is used when nothing was typed", "/colour?hex=%234f8ef7&typed=", "color%3A4f8ef7"},
		{"css notation", "/colour?typed=" + url.QueryEscape("rgb(170,187,204)"), "color%3Aaabbcc"},
		{"nothing usable goes home", "/colour?typed=nonsense&hex=", "/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := ts.get(t, tc.query)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusSeeOther {
				t.Fatalf("status = %d, want 303", resp.StatusCode)
			}
			if loc := resp.Header.Get("Location"); !strings.Contains(loc, tc.want) {
				t.Errorf("redirect = %q, want it to contain %q", loc, tc.want)
			}
		})
	}
}
