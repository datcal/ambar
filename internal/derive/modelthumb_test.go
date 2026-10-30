package derive

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// thumbStub stands in for Blender's session script: open names one texture that is not
// where it says, and thumb copies picture to the dest it was given. Every request is kept
// in a log beside it. A src containing "anim" has nothing to draw.
func thumbStub(t *testing.T, picture []byte) (bin, log string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("stub uses a POSIX shell script")
	}
	dir := t.TempDir()
	pic := filepath.Join(dir, "picture.png")
	if err := os.WriteFile(pic, picture, 0o644); err != nil {
		t.Fatal(err)
	}
	bin, log = filepath.Join(dir, "blender"), filepath.Join(dir, "requests")
	script := `#!/bin/sh
echo "@@ambar-ready@@"
while IFS= read -r line; do
  printf '%s\n' "$line" >> "` + log + `"
  case "$line" in
    *'"op":"open"'*anim*) printf '@@ambar-result@@ {"ok": true, "meshes": 0, "refs": []}\n' ;;
    *'"op":"open"'*) printf '@@ambar-result@@ {"ok": true, "meshes": 1, "refs": ["C:\\\\Work\\\\Crate_Texture.psd"]}\n' ;;
    *'"op":"thumb"'*)
      case "$(tail -n 2 "` + log + `" | head -n 1)" in
        *anim*) printf '@@ambar-result@@ {"ok": false, "nothing": "no geometry to draw"}\n'; continue ;;
      esac
      dest=$(printf '%s' "$line" | sed 's/.*"dest":"\([^"]*\)".*/\1/')
      cp "` + pic + `" "$dest"
      printf '@@ambar-result@@ {"ok": true, "untextured": 0}\n' ;;
  esac
done
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, log
}

func squarePNG(t *testing.T, visible bool) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	if visible {
		for y := 16; y < 48; y++ {
			for x := 16; x < 48; x++ {
				img.Set(x, y, color.RGBA{R: 0x80, G: 0x40, B: 0x20, A: 0xff})
			}
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func thumbTask(t *testing.T, lib, rel string) Task {
	t.Helper()
	info, err := os.Stat(filepath.Join(lib, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return Task{Op: OpThumb, AssetID: 1, SHA256: strings.Repeat("ab", 32), Ext: "fbx",
		Filename: filepath.Base(rel), LibraryPath: rel, PackPath: "pack", Size: info.Size()}
}

// TestRenderThumbFindsTexturesAndChecksThePicture: the textures Blender could not find are
// found by the library search and handed back to it, and what it drew becomes the
// thumbnail, the preview and a record that Blender drew them.
func TestRenderThumbFindsTexturesAndChecksThePicture(t *testing.T) {
	lib := t.TempDir()
	for rel, content := range map[string]string{
		"pack/Models/crate.fbx":               "fbx",
		"pack/Models/walk_anim.fbx":           "fbx",
		"pack/Textures/Crate_Texture_01.png":  "png",
		"pack/Textures/Crate_Texture_01B.png": "png",
	} {
		path := filepath.Join(lib, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(path), 0o755)
		os.WriteFile(path, []byte(content), 0o644)
	}
	bin, log := thumbStub(t, squarePNG(t, true))
	data := t.TempDir()

	task := thumbTask(t, lib, "pack/Models/crate.fbx")
	out := RenderThumb(context.Background(), task, lib, data, bin)
	if out.State != StateOK || out.Result == nil {
		t.Fatalf("outcome = %+v", out)
	}
	requests, _ := os.ReadFile(log)
	if want := filepath.Join(lib, "pack", "Textures", "Crate_Texture_01.png"); !strings.Contains(string(requests), want) {
		t.Errorf("Blender was not handed the texture found for the .psd:\n%s", requests)
	}
	rel, _ := Dir(task.SHA256)
	dir := filepath.Join(data, rel)
	for _, name := range out.Result.Files {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s was not written: %v", name, err)
		}
	}
	if ModelThumbStatus(dir, RendererBlender) != ThumbCurrent {
		t.Error("the thumbnail is not current for Blender after it drew it")
	}
	if ModelThumbStatus(dir, RendererBrowser) != ThumbCurrent {
		t.Error("a browser would draw over Blender's thumbnail")
	}
	if _, err := os.Stat(filepath.Join(dir, "blender-thumb.png")); err == nil {
		t.Error("Blender's PNG was left beside the derivatives")
	}

	// An animation with no mesh is an outcome, not a failure.
	if out := RenderThumb(context.Background(), thumbTask(t, lib, "pack/Models/walk_anim.fbx"), lib, t.TempDir(), bin); out.State != StateUnsupported {
		t.Errorf("an animation = %+v, want unsupported", out)
	}

	// A picture with nothing in it is not stored.
	blank, _ := thumbStub(t, squarePNG(t, false))
	if out := RenderThumb(context.Background(), task, lib, t.TempDir(), blank); out.State != StateUnsupported {
		t.Errorf("a blank picture = %+v, want unsupported", out)
	}
}
