// Package model handles the §6 3D path for the formats a pure-Go decoder can
// read: glTF and GLB. It extracts the metadata the §8 viewer overlays (triangle
// and vertex counts, bounding box, material count, animation names) and
// normalises the input to a single self-contained preview.glb, so the browser
// viewer only ever loads one format. FBX and .blend need Blender and are handled
// elsewhere (derive_state=needs_blender).
package model

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
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
		embed(doc, filepath.Dir(srcPath))
		if err := gltf.SaveBinary(doc, destGLB); err != nil {
			return Info{}, fmt.Errorf("write preview.glb: %w", err)
		}
	}
	return info, nil
}

// embed makes the document self-contained, which "normalise everything to preview.glb"
// (§6) always meant and this package did not do.
//
// The encoder writes a BIN chunk only when the first buffer has no URI, and a .gltf's
// first buffer always has one — the name of the .bin beside it. So SaveBinary wrote a
// 1.4 KB glb still pointing at a 202 KB file and helpfully copied that file into the
// derivative directory, where nothing serves it. Every glTF in the library opened to an
// empty stage (M17).
//
// Clearing the URI is the whole fix for geometry: the decoder has already read the
// bytes into Data. Later buffers, which are rare, become base64 data URIs. Images are
// read from beside the model and inlined the same way.
//
// What this cannot do is find a texture that is not beside the model — an FBX-exported
// pack keeps them in a shared Textures/ directory several levels up, and resolving that
// needs the pack boundary, which this package does not know. Those keep their URI and
// are the viewer's problem, which is why the viewer loads originals through the
// companion route and its pack-wide lookup. A best-effort pass: anything unresolvable
// is left exactly as it was rather than failing the derive.
func embed(doc *gltf.Document, srcDir string) {
	for i, buf := range doc.Buffers {
		if buf.URI == "" || len(buf.Data) == 0 {
			continue
		}
		if i == 0 {
			// Becomes the GLB's BIN chunk. Worth doing even for a buffer that was
			// already a base64 data URI: the chunk is the same bytes without the
			// third they gain from base64.
			buf.URI = ""
		} else if !buf.IsEmbeddedResource() {
			buf.EmbeddedResource()
		}
	}

	for _, img := range doc.Images {
		if img.URI == "" || strings.HasPrefix(img.URI, "data:") {
			continue
		}
		data, mime, ok := readLocalAsset(srcDir, img.URI)
		if !ok {
			continue
		}
		img.URI = "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
	}
}

// readLocalAsset reads a file a model refers to by relative URI, refusing to leave the
// model's own directory.
//
// Invariant 9 in miniature: the name comes out of a file in the library, which is data
// rather than input we control, so it is unescaped, cleaned, and rejected if it climbs
// out — no absolute paths, no "..", no URLs.
func readLocalAsset(srcDir, uri string) ([]byte, string, bool) {
	name, err := url.PathUnescape(uri)
	if err != nil || name == "" {
		return nil, "", false
	}
	if strings.Contains(name, "://") || filepath.IsAbs(name) || strings.HasPrefix(name, "/") {
		return nil, "", false
	}
	clean := filepath.Clean(filepath.FromSlash(name))
	if clean == "." || strings.HasPrefix(clean, "..") {
		return nil, "", false
	}
	data, err := os.ReadFile(filepath.Join(srcDir, clean))
	if err != nil {
		return nil, "", false
	}
	mime := imageMIME(filepath.Ext(clean))
	if mime == "" {
		return nil, "", false
	}
	return data, mime, true
}

// imageMIME maps the texture formats glTF allows to their media types. An allow-list:
// a model naming a .exe as its base-colour texture gets nothing inlined.
func imageMIME(ext string) string {
	switch strings.ToLower(strings.TrimPrefix(ext, ".")) {
	case "png":
		return "image/png"
	case "jpg", "jpeg":
		return "image/jpeg"
	case "webp":
		return "image/webp"
	default:
		return ""
	}
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
