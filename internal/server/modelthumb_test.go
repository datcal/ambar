package server

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/datcal/ambar/internal/derive"
)

// pngBytes builds a small PNG for the upload tests.
func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 0x80, A: 0xff})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// post sends a raw body with the CSRF header, the way the thumbnailer island does.
func (ts *testServer) postRaw(t *testing.T, path, contentType string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-CSRF-Token", ts.csrfToken(t, "/"))
	resp, err := ts.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// TestBrowserModelThumbnail: the browser renders what the server cannot, and the
// result becomes an ordinary cached derivative (M15).
func TestBrowserModelThumbnail(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	ts.seedLibrary(t, map[string]string{"pack/models/hero.obj": "v 0 0 0\n"})
	id := ts.assetID(t, "pack/models/hero.obj")

	// The grid asks for it: the tile is marked and three.js is loaded.
	body := ts.body(t, ts.get(t, "/"))
	for _, want := range []string{"thumb-pending", `data-format="obj"`, "/static/modelthumb.js"} {
		if !strings.Contains(body, want) {
			t.Errorf("the grid does not ask the browser for a thumbnail: %q missing", want)
		}
	}

	resp := ts.postRaw(t, itoa("/assets/%d/thumb", id), "image/png", pngBytes(t, 256, 256))
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	resp.Body.Close()

	// Stored as WebP by our own encoder, not as the bytes the client sent.
	var sha string
	if err := ts.db.Reader.QueryRow(`SELECT sha256 FROM assets WHERE id = ?`, id).Scan(&sha); err != nil {
		t.Fatal(err)
	}
	relDir, err := derive.Dir(sha)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(filepath.Join(ts.cfg.DataRoot, relDir, derive.FileThumb))
	if err != nil {
		t.Fatalf("no thumbnail was written: %v", err)
	}
	if bytes.HasPrefix(stored, []byte("\x89PNG")) {
		t.Error("the client's PNG was stored verbatim; it must be re-encoded")
	}
	if !bytes.Contains(stored[:12], []byte("WEBP")) {
		t.Errorf("stored thumbnail is not WebP: % x", stored[:12])
	}

	// The grid now shows it, and stops asking.
	body = ts.body(t, ts.get(t, "/"))
	if strings.Contains(body, "thumb-pending") {
		t.Error("the tile should no longer ask for a render")
	}
	if !strings.Contains(body, itoa("/assets/%d/thumb", id)) {
		t.Error("the tile should show the thumbnail")
	}

	// A second upload does not overwrite what is already there.
	resp = ts.postRaw(t, itoa("/assets/%d/thumb", id), "image/png", pngBytes(t, 64, 64))
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want 204 (a no-op)", resp.StatusCode)
	}
	resp.Body.Close()
	again, err := os.ReadFile(filepath.Join(ts.cfg.DataRoot, relDir, derive.FileThumb))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, again) {
		t.Error("an existing thumbnail must not be replaced by a later upload")
	}
}

// TestBrowserModelThumbnailRefusesJunk: this route takes bytes from a client, so it
// takes nothing on trust.
func TestBrowserModelThumbnailRefusesJunk(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	ts.seedLibrary(t, map[string]string{
		"pack/models/hero.obj": "v 0 0 0\n",
		"pack/sprite.png":      "x",
	})
	modelID := ts.assetID(t, "pack/models/hero.obj")
	spriteID := ts.assetID(t, "pack/sprite.png")

	// Not a PNG.
	resp := ts.postRaw(t, itoa("/assets/%d/thumb", modelID), "image/png", []byte("not an image"))
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("garbage: status = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()

	// Not a model: a 2D asset's thumbnail is derive's business, not the client's.
	resp = ts.postRaw(t, itoa("/assets/%d/thumb", spriteID), "image/png", pngBytes(t, 64, 64))
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("non-model: status = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()

	// No CSRF token.
	req, err := http.NewRequest(http.MethodPost,
		ts.URL+itoa("/assets/%d/thumb", modelID), bytes.NewReader(pngBytes(t, 64, 64)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "image/png")
	resp, err = ts.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode == http.StatusNoContent {
		t.Error("a thumbnail was accepted without a CSRF token")
	}
	resp.Body.Close()
}
