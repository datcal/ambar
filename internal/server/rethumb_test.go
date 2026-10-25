package server

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/datcal/ambar/internal/derive"
)

// colourPNG is a thumbnail-shaped render of one colour, so which upload won is visible
// in the stored file.
func colourPNG(t *testing.T, c color.RGBA) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			img.SetRGBA(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func (ts *testServer) uploadThumb(t *testing.T, id int64, body []byte, renderer, untextured int) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+itoa("/assets/%d/thumb", id), bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "image/png")
	req.Header.Set("X-CSRF-Token", ts.csrfToken(t, "/"))
	if renderer > 0 {
		req.Header.Set("X-Ambar-Renderer", strconv.Itoa(renderer))
		req.Header.Set("X-Ambar-Untextured", strconv.Itoa(untextured))
	}
	resp, err := ts.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func (ts *testServer) thumbDir(t *testing.T, id int64) string {
	t.Helper()
	var sha string
	if err := ts.db.Reader.QueryRow(`SELECT sha256 FROM assets WHERE id = ?`, id).Scan(&sha); err != nil {
		t.Fatal(err)
	}
	rel, err := derive.Dir(sha)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(ts.cfg.DataRoot, rel)
}

// TestStaleThumbnailsAreRedrawnOnlyByABetterRenderer: the white FBX tiles drawn before
// textures were resolved by name are replaceable — by the current renderer, and only by it.
// A thumbnail it drew is kept, and so is anything an older client offers.
func TestStaleThumbnailsAreRedrawnOnlyByABetterRenderer(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	ts.seedLibrary(t, map[string]string{"3d/synty/tree.fbx": "Kaydara FBX Binary tree"})
	id := ts.assetID(t, "3d/synty/tree.fbx")
	dir := ts.thumbDir(t, id)

	white := colourPNG(t, color.RGBA{R: 0xee, G: 0xee, B: 0xee, A: 0xff})
	green := colourPNG(t, color.RGBA{R: 0x2e, G: 0x8b, B: 0x57, A: 0xff})
	red := colourPNG(t, color.RGBA{R: 0xc0, G: 0x20, B: 0x20, A: 0xff})

	// What the library holds today: a thumbnail from before the record existed.
	if code := ts.uploadThumb(t, id, white, 0, 0); code != http.StatusNoContent {
		t.Fatalf("first upload = %d", code)
	}
	if err := os.Remove(filepath.Join(dir, fileThumbMeta)); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(dir, derive.FileThumb))

	// An old client cannot replace it, stale or not.
	ts.uploadThumb(t, id, red, 0, 0)
	if now, _ := os.ReadFile(filepath.Join(dir, derive.FileThumb)); !bytes.Equal(now, before) {
		t.Error("an old renderer replaced a thumbnail")
	}

	// The current renderer can, and says how it went.
	ts.uploadThumb(t, id, green, thumbRendererVersion, 1)
	after, _ := os.ReadFile(filepath.Join(dir, derive.FileThumb))
	if bytes.Equal(after, before) {
		t.Fatal("the current renderer could not replace a stale thumbnail")
	}
	var meta thumbMeta
	raw, _ := os.ReadFile(filepath.Join(dir, fileThumbMeta))
	if err := json.Unmarshal(raw, &meta); err != nil || meta.Renderer != thumbRendererVersion || meta.Untextured != 1 {
		t.Errorf("thumb.json = %s, want renderer %d with 1 untextured", raw, thumbRendererVersion)
	}

	// And what it drew is current: kept.
	ts.uploadThumb(t, id, red, thumbRendererVersion, 0)
	if now, _ := os.ReadFile(filepath.Join(dir, derive.FileThumb)); !bytes.Equal(now, after) {
		t.Error("a current thumbnail was replaced")
	}
}

// TestRenderQueueListsMissingAndStale: the jobs page's redraw queue lists what needs a
// picture — no thumbnail, or one drawn by an older renderer — once per content, and
// nothing that is current.
func TestRenderQueueListsMissingAndStale(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	ts.seedLibrary(t, map[string]string{
		"3d/synty/a_missing.fbx":      "Kaydara FBX Binary a",
		"3d/synty copy/a_missing.fbx": "Kaydara FBX Binary a", // same bytes, another path
		"3d/synty/b_stale.fbx":        "Kaydara FBX Binary b",
		"3d/synty/c_current.fbx":      "Kaydara FBX Binary c",
		"3d/synty/d_texture.png":      "not a model",
	})
	stale := ts.assetID(t, "3d/synty/b_stale.fbx")
	current := ts.assetID(t, "3d/synty/c_current.fbx")
	grey := colourPNG(t, color.RGBA{R: 0x80, G: 0x80, B: 0x80, A: 0xff})
	ts.uploadThumb(t, stale, grey, 0, 0)
	_ = os.Remove(filepath.Join(ts.thumbDir(t, stale), fileThumbMeta))
	ts.uploadThumb(t, current, grey, thumbRendererVersion, 0)

	var page renderQueuePage
	if err := json.Unmarshal([]byte(readBody(t, ts.get(t, "/models/render-queue?after=0&limit=50"))), &page); err != nil {
		t.Fatal(err)
	}
	reasons := map[string]string{}
	for _, it := range page.Items {
		reasons[it.Src] = it.Reason
		if it.Format != "fbx" {
			t.Errorf("%s has format %q", it.Src, it.Format)
		}
	}
	if len(page.Items) != 2 {
		t.Fatalf("items = %+v, want the missing one once and the stale one", page.Items)
	}
	if !page.Done || page.MaxID == 0 {
		t.Errorf("done=%v max=%d, want a finished walk over a known range", page.Done, page.MaxID)
	}
	for src, why := range reasons {
		switch why {
		case "missing", "stale":
		default:
			t.Errorf("%s listed for %q", src, why)
		}
	}
}
