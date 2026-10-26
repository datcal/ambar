package blender

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// stubBlender writes an executable shell script that stands in for Blender's session
// script: it says it is ready, then for each request writes output to the dest path and
// answers. A src containing "bad" gets an error answer and one containing "crash" makes
// it exit. Each start is counted in a file beside it, for starts.
func stubBlender(t *testing.T, output string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("stub uses a POSIX shell script")
	}
	path := filepath.Join(t.TempDir(), "blender")
	script := `#!/bin/sh
echo start >> "$(dirname "$0")/starts"
echo "Blender 9.9 (stub)"
echo "@@ambar-ready@@"
while IFS= read -r line; do
  src=$(printf '%s' "$line" | sed 's/.*"src":"\([^"]*\)".*/\1/')
  dest=$(printf '%s' "$line" | sed 's/.*"dest":"\([^"]*\)".*/\1/')
  case "$src" in
    *crash*) echo "Segmentation fault"; exit 3 ;;
    *bad*) echo 'Error: the FBX importer gave up'
           echo '@@ambar-result@@ {"ok": false, "error": "RuntimeError: cannot read"}'; continue ;;
  esac
  printf '` + output + `' > "$dest"
  printf 'Info: exported\n@@ambar-result@@ {"ok": true}\n'
done
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// starts is how many times the stub at bin was started.
func starts(t *testing.T, bin string) int {
	t.Helper()
	raw, _ := os.ReadFile(filepath.Join(filepath.Dir(bin), "starts"))
	return strings.Count(string(raw), "start")
}

func convert(t *testing.T, bin, src string) (string, error) {
	t.Helper()
	dest := filepath.Join(t.TempDir(), "preview.glb")
	err := Convert(context.Background(), bin, src, dest)
	data, _ := os.ReadFile(dest)
	return string(data), err
}

func TestLocate(t *testing.T) {
	// A configured, executable binary is found.
	bin := stubBlender(t, "x")
	if got, ok := Locate(bin, ""); !ok || got != bin {
		t.Errorf("Locate(configured) = %q,%v", got, ok)
	}
	// A configured but missing binary is not found (and does not fall back).
	if _, ok := Locate(filepath.Join(t.TempDir(), "nope"), t.TempDir()); ok {
		t.Errorf("Locate found a nonexistent configured binary")
	}
	// Nothing configured and nothing in tools/ → not found.
	if _, ok := Locate("", t.TempDir()); ok {
		t.Errorf("Locate found Blender where there is none")
	}
}

func TestLocateToolsFallback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX exec bit")
	}
	dataRoot := t.TempDir()
	dir := filepath.Join(dataRoot, "tools", "blender")
	os.MkdirAll(dir, 0o755)
	bin := filepath.Join(dir, "blender")
	os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755)
	if got, ok := Locate("", dataRoot); !ok || got != bin {
		t.Errorf("Locate(tools fallback) = %q,%v", got, ok)
	}
}

func TestConvertProducesOutput(t *testing.T) {
	bin := stubBlender(t, "GLB-BYTES")
	if data, err := convert(t, bin, "/some/model.fbx"); err != nil || data != "GLB-BYTES" {
		t.Errorf("dest = %q, err %v", data, err)
	}
}

// TestBlenderStaysOpen: one Blender converts file after file, and an error in one file
// does not cost the next a start.
func TestBlenderStaysOpen(t *testing.T) {
	bin := stubBlender(t, "GLB")
	for _, src := range []string{"/a.fbx", "/b.fbx", "/bad.fbx", "/c.blend"} {
		_, err := convert(t, bin, src)
		if strings.Contains(src, "bad") != (err != nil) {
			t.Errorf("%s: err = %v", src, err)
		}
		if err != nil && !strings.Contains(err.Error(), "cannot read") {
			t.Errorf("%s: the error does not say Blender's reason: %v", src, err)
		}
	}
	if n := starts(t, bin); n != 1 {
		t.Errorf("Blender started %d times for four files, want once", n)
	}
}

// A Blender that dies is reported with its last words, and the next file starts another.
func TestCrashedBlenderIsReplaced(t *testing.T) {
	bin := stubBlender(t, "GLB")
	_, err := convert(t, bin, "/crash.fbx")
	if err == nil || !strings.Contains(err.Error(), "Segmentation fault") {
		t.Errorf("a crash = %v, want an error with Blender's output", err)
	}
	if data, err := convert(t, bin, "/fine.fbx"); err != nil || data != "GLB" {
		t.Errorf("after a crash: %q, %v", data, err)
	}
	if n := starts(t, bin); n != 2 {
		t.Errorf("Blender started %d times, want twice", n)
	}
}

// An idle Blender exits, giving its memory back; the next file starts another.
func TestIdleBlenderExits(t *testing.T) {
	old := IdleTimeout
	IdleTimeout = 50 * time.Millisecond
	t.Cleanup(func() { IdleTimeout = old })

	bin := stubBlender(t, "GLB")
	if _, err := convert(t, bin, "/a.fbx"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if s := pool.take(bin); s != nil {
		t.Error("an idle Blender was still open after IdleTimeout")
	}
	if _, err := convert(t, bin, "/b.fbx"); err != nil {
		t.Fatal(err)
	}
	if n := starts(t, bin); n != 2 {
		t.Errorf("Blender started %d times, want twice", n)
	}
}

func TestConvertFailsWhenNoOutput(t *testing.T) {
	// A Blender that answers but writes nothing must be treated as a failure.
	if _, err := convert(t, stubBlender(t, ""), "/m.fbx"); err == nil {
		t.Error("expected an error when Blender produces no output")
	}
}

// A Blender that exits before it is ready — a broken install, a missing library — says so.
func TestBlenderThatNeverStarts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell")
	}
	path := filepath.Join(t.TempDir(), "blender")
	os.WriteFile(path, []byte("#!/bin/sh\necho 'error while loading shared libraries: libX11.so.6'\nexit 127\n"), 0o755)
	if _, err := convert(t, path, "/m.fbx"); err == nil || !strings.Contains(err.Error(), "libX11") {
		t.Errorf("err = %v, want Blender's own complaint", err)
	}
}
