// Package model handles the §6 3D path for the formats a pure-Go decoder can
// read: glTF and GLB. It extracts the metadata the §8 viewer overlays (triangle
// and vertex counts, bounding box, material count, animation names) and
// normalises the input to a single self-contained preview.glb, so the browser
// viewer only ever loads one format. FBX and .blend need Blender and are handled
// elsewhere (derive_state=needs_blender).
package model

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/qmuntal/gltf"
)

// ErrUnsupported means the file is not a glTF/GLB this package can read.
var ErrUnsupported = errors.New("not a glTF/GLB model")

// Info is the extracted 3D metadata (§4 model columns).
type Info struct {
	TriCount      int
	VertCount     int
	MaterialCount int
	// BBox is the axis-aligned bounding-box size per axis, in the model's units
	// (metres by glTF convention) — the basis for §8's scale reference.
	BBox           [3]float64
	AnimationNames []string
	TextureCount   int
}

// CanRead reports whether the extension is a model this package handles in pure
// Go: glTF, GLB, or Wavefront OBJ.
func CanRead(ext string) bool {
	switch strings.ToLower(strings.TrimPrefix(ext, ".")) {
	case "glb", "gltf", "obj":
		return true
	}
	return false
}

// Analyze reads a glTF/GLB and returns its metadata without writing anything.
func Analyze(path string) (Info, error) { return process(path, "") }

// Normalize reads a glTF/GLB, writes a self-contained preview.glb to destGLB, and
// returns the metadata — the §6 "normalise everything to preview.glb" for the
// formats readable in pure Go.
func Normalize(srcPath, destGLB string) (Info, error) { return process(srcPath, destGLB) }

func process(srcPath, destGLB string) (Info, error) {
	if !CanRead(filepath.Ext(srcPath)) {
		return Info{}, fmt.Errorf("%w: %s", ErrUnsupported, filepath.Base(srcPath))
	}
	doc, err := readDocument(srcPath)
	if err != nil {
		return Info{}, err
	}

	info := extract(doc)

	if destGLB != "" {
		if err := gltf.SaveBinary(doc, destGLB); err != nil {
			return Info{}, fmt.Errorf("write preview.glb: %w", err)
		}
	}
	return info, nil
}

// readDocument loads a model into a glTF document, converting OBJ on the way.
func readDocument(srcPath string) (*gltf.Document, error) {
	if strings.EqualFold(strings.TrimPrefix(filepath.Ext(srcPath), "."), "obj") {
		doc, err := parseOBJ(srcPath)
		if err != nil {
			return nil, fmt.Errorf("convert obj: %w", err)
		}
		return doc, nil
	}
	doc, err := gltf.Open(srcPath)
	if err != nil {
		return nil, fmt.Errorf("read model: %w", err)
	}
	return doc, nil
}

// extract walks the document's meshes and animations for the metadata.
func extract(doc *gltf.Document) Info {
	var info Info
	haveBounds := false
	var lo, hi [3]float64

	for _, mesh := range doc.Meshes {
		for _, prim := range mesh.Primitives {
			posIdx, ok := prim.Attributes[gltf.POSITION]
			var posAcc *gltf.Accessor
			if ok && posIdx < len(doc.Accessors) {
				posAcc = doc.Accessors[posIdx]
				info.VertCount += posAcc.Count
				if len(posAcc.Min) == 3 && len(posAcc.Max) == 3 {
					for i := 0; i < 3; i++ {
						if !haveBounds || posAcc.Min[i] < lo[i] {
							lo[i] = posAcc.Min[i]
						}
						if !haveBounds || posAcc.Max[i] > hi[i] {
							hi[i] = posAcc.Max[i]
						}
					}
					haveBounds = true
				}
			}

			// Triangle count: indexed geometry counts its indices, otherwise the
			// position count. Both divided by three assumes a triangle topology,
			// which is what a game asset almost always is.
			switch {
			case prim.Indices != nil && *prim.Indices < len(doc.Accessors):
				info.TriCount += doc.Accessors[*prim.Indices].Count / 3
			case posAcc != nil:
				info.TriCount += posAcc.Count / 3
			}
		}
	}

	if haveBounds {
		for i := 0; i < 3; i++ {
			info.BBox[i] = hi[i] - lo[i]
		}
	}
	info.MaterialCount = len(doc.Materials)
	info.TextureCount = len(doc.Textures)
	for _, anim := range doc.Animations {
		if anim.Name != "" {
			info.AnimationNames = append(info.AnimationNames, anim.Name)
		}
	}
	return info
}
