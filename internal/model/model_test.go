package model

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/qmuntal/gltf"
	"github.com/qmuntal/gltf/modeler"
)

// writeTriangleGLB builds a one-triangle, one-material GLB at path.
func writeTriangleGLB(t *testing.T, path string) {
	t.Helper()
	doc := gltf.NewDocument()
	pos := modeler.WritePosition(doc, [][3]float32{{0, 0, 0}, {2, 0, 0}, {0, 3, 0}})
	idx := modeler.WriteIndices(doc, []uint16{0, 1, 2})
	doc.Meshes = append(doc.Meshes, &gltf.Mesh{
		Primitives: []*gltf.Primitive{{
			Indices:    gltf.Index(idx),
			Attributes: gltf.PrimitiveAttributes{gltf.POSITION: pos},
		}},
	})
	doc.Materials = append(doc.Materials, &gltf.Material{Name: "steel"})
	doc.Animations = append(doc.Animations, &gltf.Animation{Name: "idle"})
	if err := gltf.SaveBinary(doc, path); err != nil {
		t.Fatal(err)
	}
}

func TestAnalyzeGLB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tri.glb")
	writeTriangleGLB(t, path)

	info, err := Analyze(path)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if info.TriCount != 1 {
		t.Errorf("tris = %d, want 1", info.TriCount)
	}
	if info.VertCount != 3 {
		t.Errorf("verts = %d, want 3", info.VertCount)
	}
	if info.MaterialCount != 1 {
		t.Errorf("materials = %d, want 1", info.MaterialCount)
	}
	if len(info.AnimationNames) != 1 || info.AnimationNames[0] != "idle" {
		t.Errorf("animations = %v, want [idle]", info.AnimationNames)
	}
	// The bounding box spans (2,3,0).
	if info.BBox[0] < 1.9 || info.BBox[1] < 2.9 {
		t.Errorf("bbox = %v, want ~[2 3 0]", info.BBox)
	}
}

func TestNormalizeWritesPreviewGLB(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "tri.glb")
	writeTriangleGLB(t, src)
	dst := filepath.Join(dir, "preview.glb")

	info, err := Normalize(src, dst)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if info.TriCount != 1 {
		t.Errorf("tris = %d", info.TriCount)
	}
	fi, err := os.Stat(dst)
	if err != nil || fi.Size() == 0 {
		t.Errorf("preview.glb not written: %v", err)
	}
	// The written file is itself a valid GLB.
	if _, err := Analyze(dst); err != nil {
		t.Errorf("preview.glb is not a valid GLB: %v", err)
	}
}

func TestUnsupportedExt(t *testing.T) {
	if CanRead(".fbx") {
		t.Error("fbx must not be readable here")
	}
	if _, err := Analyze(filepath.Join(t.TempDir(), "x.fbx")); err == nil {
		t.Error("expected ErrUnsupported for fbx")
	}
}

func TestConvertOBJ(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "quad.obj")
	// A unit quad (two triangles) with normals and texcoords.
	obj := `# a quad
v 0 0 0
v 2 0 0
v 2 3 0
v 0 3 0
vt 0 0
vt 1 0
vt 1 1
vt 0 1
vn 0 0 1
f 1/1/1 2/2/1 3/3/1 4/4/1
`
	if err := os.WriteFile(src, []byte(obj), 0o644); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(dir, "preview.glb")
	info, err := Normalize(src, dst)
	if err != nil {
		t.Fatalf("normalize obj: %v", err)
	}
	if info.TriCount != 2 {
		t.Errorf("tris = %d, want 2 (quad fan)", info.TriCount)
	}
	if info.VertCount != 4 {
		t.Errorf("verts = %d, want 4", info.VertCount)
	}
	if info.BBox[0] < 1.9 || info.BBox[1] < 2.9 {
		t.Errorf("bbox = %v, want ~[2 3 0]", info.BBox)
	}
	// The converted preview.glb is itself a valid GLB.
	if _, err := Analyze(dst); err != nil {
		t.Errorf("converted preview.glb invalid: %v", err)
	}
}

func TestOBJNegativeAndNoNormals(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "tri.obj")
	// Negative (relative) indices, position-only faces.
	obj := "v 0 0 0\nv 1 0 0\nv 0 1 0\nf -3 -2 -1\n"
	os.WriteFile(src, []byte(obj), 0o644)
	info, err := Analyze(src)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if info.TriCount != 1 || info.VertCount != 3 {
		t.Errorf("got tris=%d verts=%d, want 1/3", info.TriCount, info.VertCount)
	}
}
