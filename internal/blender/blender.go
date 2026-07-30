// Package blender drives an optional, external Blender CLI to handle the model
// formats no pure-Go decoder can read: FBX and .blend (§6). Blender is never
// baked into the image — "the difference between a 250 MB image and a 2 GB one"
// — so it is used only when a binary is configured or has been placed in
// $DATA_ROOT/tools/. When absent, callers keep those assets in needs_blender.
package blender

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// ConvertTimeout bounds one Blender invocation so a pathological file cannot hang
// a worker forever.
const ConvertTimeout = 3 * time.Minute

// Locate returns the Blender executable to use, and whether one was found: the
// configured path if set and runnable, otherwise a copy under
// $DATA_ROOT/tools/blender/ (where a runtime download would place it).
func Locate(configured, dataRoot string) (string, bool) {
	if configured != "" {
		if isExecutable(configured) {
			return configured, true
		}
		return "", false
	}
	name := "blender"
	if runtime.GOOS == "windows" {
		name = "blender.exe"
	}
	candidate := filepath.Join(dataRoot, "tools", "blender", name)
	if isExecutable(candidate) {
		return candidate, true
	}
	return "", false
}

func isExecutable(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	// On Windows the mode bits do not carry execute; existence is enough there.
	if runtime.GOOS == "windows" {
		return true
	}
	return info.Mode()&0o111 != 0
}

// convertScript runs inside Blender: import the source (FBX or .blend) into an
// empty scene and export a single GLB. The paths arrive after the `--` separator
// so Blender does not try to interpret them as its own flags.
const convertScript = `
import bpy, sys, os
argv = sys.argv[sys.argv.index("--") + 1:]
src, dest = argv[0], argv[1]
ext = os.path.splitext(src)[1].lower()
bpy.ops.wm.read_factory_settings(use_empty=True)
if ext == ".blend":
    bpy.ops.wm.open_mainfile(filepath=src)
elif ext == ".fbx":
    bpy.ops.import_scene.fbx(filepath=src)
else:
    sys.exit(2)
bpy.ops.export_scene.gltf(filepath=dest, export_format='GLB')
`

// Convert imports src (FBX/.blend) through Blender and writes a normalised GLB to
// dest. It fails if Blender exits non-zero or produces no output.
func Convert(ctx context.Context, bin, src, dest string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, ConvertTimeout)
	defer cancel()

	scriptFile, err := os.CreateTemp("", "ambar-blender-*.py")
	if err != nil {
		return fmt.Errorf("blender: temp script: %w", err)
	}
	defer os.Remove(scriptFile.Name())
	if _, err := scriptFile.WriteString(convertScript); err != nil {
		scriptFile.Close()
		return err
	}
	scriptFile.Close()

	cmd := exec.CommandContext(ctx, bin,
		"--background", "--factory-startup", "--python", scriptFile.Name(), "--", src, dest)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("blender convert failed: %w\n%s", err, tail(out))
	}
	if info, statErr := os.Stat(dest); statErr != nil || info.Size() == 0 {
		return fmt.Errorf("blender produced no output for %s\n%s", filepath.Base(src), tail(out))
	}
	return nil
}

// tail returns the last part of Blender's output for an error message.
func tail(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 800 {
		s = "…" + s[len(s)-800:]
	}
	return s
}
