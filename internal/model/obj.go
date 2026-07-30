package model

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/qmuntal/gltf"
	"github.com/qmuntal/gltf/modeler"
)

// parseOBJ reads a Wavefront .obj into a glTF document (§6: "obj converts in pure
// Go"). It de-indexes OBJ's independent position/normal/texcoord indices into the
// single shared index buffer glTF requires, triangulating polygons by a fan.
//
// Materials and .mtl are ignored: the viewer only needs geometry, and the counts
// and bounding box come from the positions. Groups and smoothing are ignored too.
func parseOBJ(path string) (*gltf.Document, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var (
		positions [][3]float32
		normals   [][3]float32
		texcoords [][2]float32

		outPos  []([3]float32)
		outNrm  []([3]float32)
		outTex  []([2]float32)
		indices []uint32
		combo   = map[string]uint32{}
		haveNrm bool
		haveTex bool
	)

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1024*1024), 8*1024*1024) // long face lines happen
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' {
			continue
		}
		fields := strings.Fields(line)
		switch fields[0] {
		case "v":
			p, err := vec3(fields[1:])
			if err != nil {
				return nil, err
			}
			positions = append(positions, p)
		case "vn":
			n, err := vec3(fields[1:])
			if err != nil {
				return nil, err
			}
			normals = append(normals, n)
		case "vt":
			t, err := vec2(fields[1:])
			if err != nil {
				return nil, err
			}
			texcoords = append(texcoords, t)
		case "f":
			// Resolve each face vertex to a shared index, then fan-triangulate.
			verts := fields[1:]
			idx := make([]uint32, 0, len(verts))
			for _, v := range verts {
				key := v
				id, ok := combo[key]
				if !ok {
					pi, ti, ni, err := faceRefs(v, len(positions), len(texcoords), len(normals))
					if err != nil {
						return nil, err
					}
					id = uint32(len(outPos))
					combo[key] = id
					outPos = append(outPos, positions[pi])
					if ni >= 0 && ni < len(normals) {
						outNrm = append(outNrm, normals[ni])
						haveNrm = true
					} else {
						outNrm = append(outNrm, [3]float32{})
					}
					if ti >= 0 && ti < len(texcoords) {
						outTex = append(outTex, texcoords[ti])
						haveTex = true
					} else {
						outTex = append(outTex, [2]float32{})
					}
				}
				idx = append(idx, id)
			}
			for i := 1; i+1 < len(idx); i++ {
				indices = append(indices, idx[0], idx[i], idx[i+1])
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(outPos) == 0 || len(indices) == 0 {
		return nil, fmt.Errorf("%w: no geometry in the obj", ErrUnsupported)
	}

	doc := gltf.NewDocument()
	attrs := gltf.PrimitiveAttributes{gltf.POSITION: modeler.WritePosition(doc, outPos)}
	if haveNrm {
		attrs[gltf.NORMAL] = modeler.WriteNormal(doc, outNrm)
	}
	if haveTex {
		attrs[gltf.TEXCOORD_0] = modeler.WriteTextureCoord(doc, outTex)
	}
	idxAcc := modeler.WriteIndices(doc, indices)
	doc.Meshes = append(doc.Meshes, &gltf.Mesh{
		Primitives: []*gltf.Primitive{{Indices: gltf.Index(idxAcc), Attributes: attrs}},
	})
	return doc, nil
}

// faceRefs parses an OBJ face vertex token (`v`, `v/vt`, `v/vt/vn`, `v//vn`) into
// zero-based indices, resolving OBJ's 1-based and negative (relative) references.
func faceRefs(tok string, nPos, nTex, nNrm int) (pi, ti, ni int, err error) {
	parts := strings.Split(tok, "/")
	pi, err = objIndex(parts[0], nPos)
	if err != nil {
		return 0, 0, 0, err
	}
	ti, ni = -1, -1
	if len(parts) >= 2 && parts[1] != "" {
		if ti, err = objIndex(parts[1], nTex); err != nil {
			return 0, 0, 0, err
		}
	}
	if len(parts) >= 3 && parts[2] != "" {
		if ni, err = objIndex(parts[2], nNrm); err != nil {
			return 0, 0, 0, err
		}
	}
	return pi, ti, ni, nil
}

// objIndex converts a 1-based (or negative relative) OBJ index to zero-based.
func objIndex(s string, count int) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("obj index %q: %w", s, err)
	}
	if n < 0 {
		return count + n, nil // -1 is the last element
	}
	return n - 1, nil
}

func vec3(f []string) ([3]float32, error) {
	if len(f) < 3 {
		return [3]float32{}, fmt.Errorf("obj: expected 3 floats, got %v", f)
	}
	var v [3]float32
	for i := 0; i < 3; i++ {
		x, err := strconv.ParseFloat(f[i], 32)
		if err != nil {
			return v, err
		}
		v[i] = float32(x)
	}
	return v, nil
}

func vec2(f []string) ([2]float32, error) {
	if len(f) < 2 {
		return [2]float32{}, fmt.Errorf("obj: expected 2 floats, got %v", f)
	}
	var v [2]float32
	for i := 0; i < 2; i++ {
		x, err := strconv.ParseFloat(f[i], 32)
		if err != nil {
			return v, err
		}
		v[i] = float32(x)
	}
	return v, nil
}
