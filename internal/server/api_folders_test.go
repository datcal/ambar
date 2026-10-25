package server

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"testing"
)

func TestAPIFoldersAndDirSearch(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	token := ts.apiToken(t)
	ts.seedLibrary(t, map[string]string{
		"2d/kenney/a.png":  "a",
		"2d/kenney/b.png":  "b",
		"2d/other/c.png":   "c",
		"3d/ships/d.glb":   "d",
		"3d/ships/e/f.glb": "f",
	})

	var root struct {
		Assets  int `json:"assets"`
		Folders []struct {
			Name        string `json:"name"`
			Path        string `json:"path"`
			Assets      int    `json:"assets"`
			HasChildren bool   `json:"has_children"`
		} `json:"folders"`
	}
	ts.apiJSON(t, "/api/v1/folders", token, &root)
	if len(root.Folders) != 2 || root.Folders[0].Name != "2d" || root.Folders[0].Assets != 3 {
		t.Fatalf("root folders = %+v", root)
	}

	var sub struct {
		Path    string `json:"path"`
		Folders []struct {
			Name string `json:"name"`
		} `json:"folders"`
	}
	ts.apiJSON(t, "/api/v1/folders?path=3d/ships", token, &sub)
	if sub.Path != "3d/ships" || len(sub.Folders) != 1 || sub.Folders[0].Name != "e" {
		t.Fatalf("sub folders = %+v", sub)
	}

	if r := ts.apiGet(t, "/api/v1/folders?path=nope", token); r.StatusCode != http.StatusNotFound {
		t.Errorf("unknown folder = %d, want 404", r.StatusCode)
	}
	if r := ts.apiGet(t, "/api/v1/folders", ""); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("no token = %d, want 401", r.StatusCode)
	}

	var found struct {
		Total int `json:"total"`
	}
	ts.apiJSON(t, "/api/v1/search?group=1&dir=2d/kenney", token, &found)
	if found.Total != 2 {
		t.Errorf("dir search total = %d, want 2", found.Total)
	}
	ts.apiJSON(t, "/api/v1/search?dir=3d", token, &found)
	if found.Total != 2 {
		t.Errorf("flat dir search total = %d, want 2", found.Total)
	}
}

func TestAPIThumbAsPNG(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	token := ts.apiToken(t)

	img := image.NewNRGBA(image.Rect(0, 0, 16, 16))
	for x := 0; x < 16; x++ {
		for y := 0; y < 16; y++ {
			img.Set(x, y, color.NRGBA{R: uint8(x * 16), G: uint8(y * 16), B: 90, A: 255})
		}
	}
	var src bytes.Buffer
	if err := png.Encode(&src, img); err != nil {
		t.Fatal(err)
	}
	ts.seedWithDerivatives(t, map[string][]byte{"2d/pack/hero.png": src.Bytes()})
	id := ts.assetID(t, "2d/pack/hero.png")

	resp := ts.apiGet(t, itoa("/api/v1/assets/%d/thumb?format=png", id), token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("png thumb = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/png" {
		t.Fatalf("content-type = %q, want image/png", ct)
	}
	if _, err := png.Decode(resp.Body); err != nil {
		t.Fatalf("body is not a PNG: %v", err)
	}

	// Without the parameter nothing changes.
	resp = ts.apiGet(t, itoa("/api/v1/assets/%d/thumb", id), token)
	if ct := resp.Header.Get("Content-Type"); ct != "image/webp" {
		t.Errorf("default content-type = %q, want image/webp", ct)
	}
}
