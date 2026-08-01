package server

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/url"
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

// transparentPNG builds the image a failed render produces: a fully transparent frame.
func transparentPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewNRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestBrowserModelThumbnailRefusesBlank: the regression that made every .fbx in the
// library unopenable.
//
// FBXLoader resolved with an empty scene, the island snapshotted the empty canvas
// anyway, and the server accepted the resulting transparent PNG. That did two kinds of
// damage: the tile became a blank square instead of an honest extension chip, and
// derive_state flipped to 'ok', which made the detail page believe a normalised
// preview.glb existed and hand the viewer a URL that 404s — a 3D page that opened to
// nothing at all, silently.
//
// So: a blank frame is not a thumbnail, and nothing about the asset changes when one
// arrives.
func TestBrowserModelThumbnailRefusesBlank(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	ts.seedLibrary(t, map[string]string{"pack/models/barrel.fbx": "Kaydara FBX Binary\x00"})
	id := ts.assetID(t, "pack/models/barrel.fbx")

	resp := ts.postRaw(t, itoa("/assets/%d/thumb", id), "image/png", transparentPNG(t, 512, 512))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a blank render: status = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()

	var sha, state string
	if err := ts.db.Reader.QueryRow(
		`SELECT sha256, derive_state FROM assets WHERE id = ?`, id).Scan(&sha, &state); err != nil {
		t.Fatal(err)
	}
	if state == "ok" {
		t.Error("derive_state was flipped to ok by a blank upload; the viewer would then be sent to a preview that does not exist")
	}
	relDir, err := derive.Dir(sha)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ts.cfg.DataRoot, relDir, derive.FileThumb)); !os.IsNotExist(err) {
		t.Errorf("a blank thumbnail was written to disk (err = %v)", err)
	}

	// And the tile still asks honestly rather than showing an empty square.
	if body := ts.body(t, ts.get(t, "/")); !strings.Contains(body, "thumb-pending") {
		t.Error("the tile should still be pending after a rejected render")
	}
}

// TestBlankImage covers the boundary directly: what counts as "nothing rendered".
func TestBlankImage(t *testing.T) {
	// A centred square of side `side` in a 512x512 frame, at the given alpha — the
	// shape a small model far from the camera would leave.
	blob := func(side int, alpha uint8) image.Image {
		const size = 512
		img := image.NewNRGBA(image.Rect(0, 0, size, size))
		off := (size - side) / 2
		for y := off; y < off+side; y++ {
			for x := off; x < off+side; x++ {
				img.SetNRGBA(x, y, color.NRGBA{R: 0xcc, G: 0x88, B: 0x44, A: alpha})
			}
		}
		return img
	}

	tests := []struct {
		name string
		img  image.Image
		want bool
	}{
		{"a fully transparent frame is what a failed loader produces", image.NewNRGBA(image.Rect(0, 0, 512, 512)), true},
		{"an empty frame", image.NewNRGBA(image.Rect(0, 0, 0, 0)), true},
		{"a frame filled with opaque colour is a render", blob(512, 0xff), false},
		{"a small model far from the camera still counts", blob(64, 0xff), false},
		{"almost-invisible alpha does not count", blob(512, 0x08), true},
		{"a handful of stray pixels is below the floor", blob(8, 0xff), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := blankImage(tc.img); got != tc.want {
				t.Errorf("blankImage() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestBrowserModelThumbnailReplacesAnOrphan: a thumbnail file whose asset is not in
// state 'ok' is left over from a path that failed, and must not shield itself from
// being replaced.
//
// This is the second half of the FBX repair. Migration 0014 puts the wrongly-'ok' rows
// back to 'pending', but the blank thumb.webp those uploads wrote stays on disk — and
// while a bare os.Stat was enough to refuse an upload, that blank file would have been
// served as the model's picture for ever, with every fresh render politely turned away.
func TestBrowserModelThumbnailReplacesAnOrphan(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	ts.seedLibrary(t, map[string]string{"pack/models/barrel.fbx": "Kaydara FBX Binary\x00"})
	id := ts.assetID(t, "pack/models/barrel.fbx")

	// The state after the repair: an orphaned blank thumbnail, and a row that does not
	// claim a preview.
	ts.writeDerivative(t, id, derive.FileThumb, []byte("STALE-BLANK-WEBP"))
	if _, err := ts.db.Writer.Exec(
		`UPDATE assets SET derive_state = 'needs_blender' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}

	resp := ts.postRaw(t, itoa("/assets/%d/thumb", id), "image/png", pngBytes(t, 256, 256))
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	resp.Body.Close()

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
		t.Fatal(err)
	}
	if bytes.Contains(stored, []byte("STALE-BLANK-WEBP")) {
		t.Error("the orphaned thumbnail was kept; the model would show it for ever")
	}
	if !bytes.Contains(stored[:12], []byte("WEBP")) {
		t.Errorf("the replacement is not WebP: % x", stored[:12])
	}

	// And now that the row says 'ok', it is protected again.
	resp = ts.postRaw(t, itoa("/assets/%d/thumb", id), "image/png", pngBytes(t, 64, 64))
	resp.Body.Close()
	again, err := os.ReadFile(filepath.Join(ts.cfg.DataRoot, relDir, derive.FileThumb))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, again) {
		t.Error("a derive-owned thumbnail must not be replaceable by a client")
	}
}

// TestModelTileWithoutAPictureAsksForOne is M17's second regression, and the one the
// first pass missed.
//
// `derive_state = 'ok'` means "derive finished". For a glTF or an OBJ that means it
// wrote a preview.glb — geometry, no picture. The grid read 'ok' as "there is a
// thumbnail", rendered an <img> at a URL that 404s, and the same field gated the
// browser renderer, so nothing ever filled it in. Measured on the real library: 212 of
// 221 glTF tiles and 42 of 42 OBJ tiles were blank and would have stayed blank.
func TestModelTileWithoutAPictureAsksForOne(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	ts.seedLibrary(t, map[string]string{
		"pack/models/hut.gltf":   `{"asset":{"version":"2.0"}}`,
		"pack/models/barrel.obj": "v 0 0 0\n",
	})
	gltfID := ts.assetID(t, "pack/models/hut.gltf")
	objID := ts.assetID(t, "pack/models/barrel.obj")

	// Both derived successfully — and for a model that says nothing about a picture.
	for _, id := range []int64{gltfID, objID} {
		if _, err := ts.db.Writer.Exec(
			`UPDATE assets SET derive_state = 'ok' WHERE id = ?`, id); err != nil {
			t.Fatal(err)
		}
	}
	// The glTF has the artifact derive really does write. It is not an image.
	ts.writeDerivative(t, gltfID, "preview.glb", []byte("glTF-normalised"))

	body := readBody(t, ts.get(t, "/assets"))
	for _, id := range []int64{gltfID, objID} {
		if strings.Contains(body, itoa(`<img src="/assets/%d/thumb"`, id)) {
			t.Errorf("asset %d renders an <img> for a thumbnail that was never written", id)
		}
		if !strings.Contains(body, itoa(`data-asset="%d"`, id)) {
			t.Errorf("asset %d was not offered to the browser thumbnailer", id)
		}
	}

	// Now give one of them a real picture: it must render it and stop asking.
	ts.writeDerivative(t, objID, "thumb.webp", []byte("RIFF....WEBPVP8L"))
	body = readBody(t, ts.get(t, "/assets"))
	if !strings.Contains(body, itoa(`<img src="/assets/%d/thumb"`, objID)) {
		t.Error("a model with a thumbnail on disk should render it")
	}
	if strings.Contains(body, itoa(`data-asset="%d"`, objID)) {
		t.Error("a model with a thumbnail is still being queued for rendering")
	}
}

// TestModelThumbnailGivesTheModelAPalette is M17's answer to "colour search only works
// on 2D, doesn't it?" — it did, and the reason was structural: swatches come from
// images, and a model's derive produces a preview.glb and never an image. Measured on
// the real library: 926 models, zero swatches, so `color:` could not return a model at
// all. The browser's render is an image, so the palette comes from that.
func TestModelThumbnailGivesTheModelAPalette(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	ts.seedLibrary(t, map[string]string{"pack/models/barrel.obj": "v 0 0 0\n"})
	id := ts.assetID(t, "pack/models/barrel.obj")

	// A render that is mostly one recognisable colour, the way a model of one material is.
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			img.SetRGBA(x, y, color.RGBA{R: 0x2e, G: 0x8b, B: 0x57, A: 0xff})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}

	// Before: no swatches, and colour search cannot see it.
	if got := ts.swatchCount(t, id); got != 0 {
		t.Fatalf("the model already has %d swatches; the fixture is wrong", got)
	}

	resp := ts.postRaw(t, itoa("/assets/%d/thumb", id), "image/png", buf.Bytes())
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	resp.Body.Close()

	if got := ts.swatchCount(t, id); got == 0 {
		t.Fatal("the render produced no swatches, so colour search still cannot find this model")
	}
	var r, g, b int
	if err := ts.db.Reader.QueryRow(
		`SELECT r, g, b FROM asset_swatches WHERE asset_id = ? ORDER BY rank LIMIT 1`,
		id).Scan(&r, &g, &b); err != nil {
		t.Fatal(err)
	}
	if r != 0x2e || g != 0x8b || b != 0x57 {
		t.Errorf("dominant swatch = #%02x%02x%02x, want #2e8b57", r, g, b)
	}

	// The point of all of it: the model now answers a colour search.
	body := readBody(t, ts.get(t, "/assets?q="+url.QueryEscape("color:2e8b57~12")))
	if !strings.Contains(body, itoa("/assets/%d", id)) {
		t.Error("colour search still does not return the model")
	}
}

func (ts *testServer) swatchCount(t *testing.T, assetID int64) int {
	t.Helper()
	var n int
	if err := ts.db.Reader.QueryRow(
		`SELECT count(*) FROM asset_swatches WHERE asset_id = ?`, assetID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
