package derive

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/datcal/ambar/internal/model"
)

// TestDeriveFBXViaStubBlender proves the Blender path: with a Blender binary
// configured (here a stub that emits a valid GLB), an .fbx derives a preview.glb
// and metadata instead of sitting in needs_blender.
func TestDeriveFBXViaStubBlender(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stub uses a POSIX shell script")
	}
	dir := t.TempDir()

	// A real GLB fixture the stub will "produce".
	obj := filepath.Join(dir, "tri.obj")
	os.WriteFile(obj, []byte("v 0 0 0\nv 1 0 0\nv 0 1 0\nf 1 2 3\n"), 0o644)
	fixture := filepath.Join(dir, "fixture.glb")
	if _, err := model.Normalize(obj, fixture); err != nil {
		t.Fatalf("build fixture: %v", err)
	}

	// A stub Blender that copies the fixture to its last argument (the dest).
	stub := filepath.Join(dir, "blender")
	os.WriteFile(stub, []byte("#!/bin/sh\nfor last; do :; done\ncp '"+fixture+"' \"$last\"\n"), 0o755)

	// The .fbx content is irrelevant since Blender is stubbed.
	fbx := filepath.Join(dir, "rig.fbx")
	os.WriteFile(fbx, []byte("Kaydara FBX Binary"), 0o644)
	hash := ContentHash([]byte("Kaydara FBX Binary"))

	res, err := Generate(GenerateOptions{
		AbsPath: fbx, Ext: "fbx", SHA256: hash, DataRoot: dir, BlenderBin: stub,
	})
	if err != nil {
		t.Fatalf("generate fbx via stub blender: %v", err)
	}
	if res.Model == nil || res.Model.TriCount != 1 {
		t.Errorf("model metadata missing/wrong: %+v", res.Model)
	}
	rel, _ := Dir(hash)
	if _, err := os.Stat(filepath.Join(dir, rel, FileModelPreview)); err != nil {
		t.Errorf("preview.glb not produced: %v", err)
	}
}

// Without a Blender binary, .fbx stays needs_blender.
func TestDeriveFBXWithoutBlender(t *testing.T) {
	dir := t.TempDir()
	fbx := filepath.Join(dir, "rig.fbx")
	os.WriteFile(fbx, []byte("x"), 0o644)
	_, err := Generate(GenerateOptions{
		AbsPath: fbx, Ext: "fbx", SHA256: ContentHash([]byte("x")), DataRoot: dir,
	})
	if !isNeedsBlender(err) {
		t.Errorf("without Blender, .fbx err = %v, want ErrNeedsBlender", err)
	}
}
