package blender

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// stubBlender writes an executable shell script that stands in for Blender: it
// writes `output` to the last argument (which Convert passes as the dest path).
func stubBlender(t *testing.T, output string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("stub uses a POSIX shell script")
	}
	path := filepath.Join(t.TempDir(), "blender")
	script := "#!/bin/sh\nfor last; do :; done\nprintf '" + output + "' > \"$last\"\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
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
	dest := filepath.Join(t.TempDir(), "preview.glb")
	if err := Convert(context.Background(), bin, "/some/model.fbx", dest); err != nil {
		t.Fatalf("convert: %v", err)
	}
	data, err := os.ReadFile(dest)
	if err != nil || string(data) != "GLB-BYTES" {
		t.Errorf("dest = %q, err %v", data, err)
	}
}

func TestConvertFailsWhenNoOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell")
	}
	// A stub that exits cleanly but writes nothing must be treated as a failure.
	path := filepath.Join(t.TempDir(), "blender")
	os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755)
	if err := Convert(context.Background(), path, "/m.fbx", filepath.Join(t.TempDir(), "out.glb")); err == nil {
		t.Error("expected an error when Blender produces no output")
	}
}
