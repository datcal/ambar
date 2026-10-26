// Package blender drives an optional, external Blender CLI to handle the model
// formats no pure-Go decoder can read: FBX and .blend (§6). Blender is never
// baked into the image — "the difference between a 250 MB image and a 2 GB one"
// — so it is used only when a binary is configured or has been placed in
// $DATA_ROOT/tools/. When absent, callers keep those assets in needs_blender.
package blender

import (
	"os"
	"path/filepath"
	"runtime"
)

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
