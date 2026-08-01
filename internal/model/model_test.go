package model

import (
	"os"
	"path/filepath"
	"strings"
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

// writeSeparateGLTF builds the shape every glTF in this library has: a .gltf naming an
// external .bin for its geometry and an external .png for its texture.
func writeSeparateGLTF(t *testing.T, dir, name string) string {
	t.Helper()
	doc := gltf.NewDocument()
	pos := modeler.WritePosition(doc, [][3]float32{{0, 0, 0}, {1, 0, 0}, {0, 1, 0}})
	idx := modeler.WriteIndices(doc, []uint16{0, 1, 2})
	doc.Meshes = append(doc.Meshes, &gltf.Mesh{
		Primitives: []*gltf.Primitive{{
			Indices:    gltf.Index(idx),
			Attributes: gltf.PrimitiveAttributes{gltf.POSITION: pos},
		}},
	})
	// A one-pixel PNG beside the model, referenced by name.
	png := []byte{
		0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a,
		0, 0, 0, 0x0d, 'I', 'H', 'D', 'R',
		0, 0, 0, 1, 0, 0, 0, 1, 8, 6, 0, 0, 0, 0x1f, 0x15, 0xc4, 0x89,
		0, 0, 0, 0, 'I', 'E', 'N', 'D', 0xae, 0x42, 0x60, 0x82,
	}
	if err := os.WriteFile(filepath.Join(dir, "skin.png"), png, 0o600); err != nil {
		t.Fatal(err)
	}
	doc.Images = append(doc.Images, &gltf.Image{Name: "skin", URI: "skin.png", MimeType: "image/png"})

	// Naming the buffer makes gltf.Save write the geometry to a separate .bin, which
	// is exactly the on-disk shape SaveBinary then failed to collapse. Left unnamed it
	// would be inlined as base64 and the test would prove nothing.
	bin := strings.TrimSuffix(name, filepath.Ext(name)) + ".bin"
	doc.Buffers[0].URI = bin

	path := filepath.Join(dir, name)
	if err := gltf.Save(doc, path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, bin)); err != nil {
		t.Fatalf("fixture did not write an external buffer: %v", err)
	}
	return path
}

// TestNormalizeEmbedsExternalBuffer is M17's regression.
//
// SaveBinary writes a BIN chunk only when the first buffer has no URI, and a .gltf's
// first buffer always names the .bin beside it. The result was a "GLB" of about 1.4 KB
// that still pointed at a 202 KB file nothing served, so all 442 glTF assets in the
// library opened to an empty stage. The test asserts the property that was missing:
// what Normalize writes must stand on its own with every neighbouring file removed.
func TestNormalizeEmbedsExternalBuffer(t *testing.T) {
	srcDir := t.TempDir()
	src := writeSeparateGLTF(t, srcDir, "hut.gltf")

	outDir := t.TempDir()
	dst := filepath.Join(outDir, "preview.glb")
	if _, err := Normalize(src, dst); err != nil {
		t.Fatalf("normalize: %v", err)
	}

	// Nothing may have been written beside the preview: an external buffer file there
	// is the encoder telling us it did not embed.
	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "preview.glb" {
			t.Errorf("preview.glb has a companion file %q; it is not self-contained", e.Name())
		}
	}

	doc, err := gltf.Open(dst)
	if err != nil {
		t.Fatalf("reopen preview.glb: %v", err)
	}
	if len(doc.Buffers) == 0 {
		t.Fatal("preview.glb has no buffers")
	}
	if doc.Buffers[0].URI != "" {
		t.Errorf("buffer 0 URI = %q, want empty (a GLB BIN chunk)", doc.Buffers[0].URI)
	}
	if len(doc.Buffers[0].Data) == 0 {
		t.Error("buffer 0 carries no data")
	}
	if len(doc.Images) != 1 || !strings.HasPrefix(doc.Images[0].URI, "data:image/png;base64,") {
		t.Errorf("image URI = %q, want an inlined data URI", doc.Images[0].URI)
	}

	// The decisive check: readable with the source directory gone.
	if err := os.RemoveAll(srcDir); err != nil {
		t.Fatal(err)
	}
	info, err := Analyze(dst)
	if err != nil {
		t.Fatalf("preview.glb does not read without its source directory: %v", err)
	}
	if info.TriCount != 1 || info.VertCount != 3 {
		t.Errorf("geometry lost: tris=%d verts=%d, want 1 and 3", info.TriCount, info.VertCount)
	}
}

// TestEmbedRefusesEscapingTexture keeps invariant 9 in the one place this package reads
// a path out of library data: a model naming ../../secret.png gets nothing inlined, and
// the derive still succeeds.
func TestEmbedRefusesEscapingTexture(t *testing.T) {
	for _, uri := range []string{"../outside.png", "/etc/passwd.png", "http://host/x.png", "sub/../../up.png"} {
		doc := gltf.NewDocument()
		doc.Images = append(doc.Images, &gltf.Image{URI: uri})
		embed(doc, t.TempDir())
		if strings.HasPrefix(doc.Images[0].URI, "data:") {
			t.Errorf("%q was inlined; it must not be", uri)
		}
	}
}
