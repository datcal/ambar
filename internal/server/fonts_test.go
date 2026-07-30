package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/image/font/gofont/goregular"
)

// seedFont indexes a real font file (Go's own typeface, so nothing is vendored).
func (ts *testServer) seedFont(t *testing.T) int64 {
	t.Helper()
	dir := filepath.Join(ts.cfg.LibraryRoot, "fonts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Go-Regular.ttf"), goregular.TTF, 0o644); err != nil {
		t.Fatal(err)
	}
	ts.seedLibrary(t, map[string]string{"fonts/placeholder.png": "x"})
	return ts.assetID(t, "fonts/Go-Regular.ttf")
}

// TestFontSpecimenPage: a font gets a live specimen you can type into (M15). The
// question a font list has to answer is "how does my text look in this", and a static
// image cannot answer it.
func TestFontSpecimenPage(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	id := ts.seedFont(t)

	body := ts.body(t, ts.get(t, itoa("/assets/%d", id)))
	for _, want := range []string{
		`id="specimen"`,
		itoa(`data-src="/assets/%d/font"`, id),
		`data-role="text"`,
		"/static/specimen.js",
		"Try it",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the specimen panel is missing %q", want)
		}
	}
	// The face must arrive through a fetchable route, not an inline data URL.
	if strings.Contains(body, "data:font") {
		t.Error("the font should be fetched, not inlined")
	}
}

// TestFontBytesServedSafely: the face is library content, so §11's rules apply to it
// exactly as to a download — and only fonts are served by this route.
func TestFontBytesServedSafely(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	id := ts.seedFont(t)

	resp := ts.get(t, itoa("/assets/%d/font", id))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Errorf("Content-Disposition = %q, want attachment", cd)
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("missing nosniff")
	}
	resp.Body.Close()

	// A non-font asset is not served by the font route.
	other := ts.assetID(t, "fonts/placeholder.png")
	resp = ts.get(t, itoa("/assets/%d/font", other))
	if resp.StatusCode == http.StatusOK {
		t.Error("a PNG was served as a font")
	}
	resp.Body.Close()
}
