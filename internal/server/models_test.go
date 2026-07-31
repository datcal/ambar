package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/datcal/ambar/internal/derive"
)

// writeDerivative places a file in an asset's derivative directory by hand, for tests
// that need to say "this derivative exists" without running a deriver that could not
// produce it anyway (there is no pure-Go FBX-to-glTF converter).
func (ts *testServer) writeDerivative(t *testing.T, assetID int64, name string, content []byte) {
	t.Helper()

	var sha string
	if err := ts.db.Reader.QueryRow(`SELECT sha256 FROM assets WHERE id = ?`, assetID).Scan(&sha); err != nil {
		t.Fatal(err)
	}
	relDir, err := derive.Dir(sha)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(ts.cfg.DataRoot, relDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), content, 0o644); err != nil {
		t.Fatal(err)
	}
}

// seedModel indexes a small OBJ model with an .mtl and a texture beside it, plus a
// file it must never be able to reach.
func (ts *testServer) seedModel(t *testing.T) int64 {
	t.Helper()
	ts.seedLibrary(t, map[string]string{
		"pack/models/hero.obj":          "mtllib hero.mtl\nv 0 0 0\n",
		"pack/models/hero.mtl":          "newmtl m\nmap_Kd textures/wood.png\n",
		"pack/models/textures/wood.png": "\x89PNG\r\n\x1a\n",
		"pack/hidden/private.png":       "PRIVATE-BYTES",
	})
	// Something outside the library entirely, to prove the traversal defence.
	outside := filepath.Join(filepath.Dir(ts.cfg.LibraryRoot), "outside.png")
	if err := os.WriteFile(outside, []byte("OUTSIDE-BYTES"), 0o644); err != nil {
		t.Fatal(err)
	}
	return ts.assetID(t, "pack/models/hero.obj")
}

// TestModelCompanionFiles: an .obj must be able to fetch its .mtl and the textures
// the .mtl names, because that is what makes browser-side OBJ viewing work (M14).
func TestModelCompanionFiles(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	id := ts.seedModel(t)

	for _, name := range []string{"hero.obj", "hero.mtl", "textures/wood.png"} {
		resp := ts.get(t, itoa("/assets/%d/file/%s", id, name))
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", name, resp.StatusCode)
			continue
		}
		// §11: library bytes are never served inline, whatever the viewer needs them for.
		if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
			t.Errorf("%s: Content-Disposition = %q, want attachment", name, cd)
		}
		if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: missing nosniff", name)
		}
		resp.Body.Close()
	}
}

// TestModelCompanionRefusesEscapes is the invariant 9 test for the new route: the
// name comes out of a model file, which is untrusted input like any other.
func TestModelCompanionRefusesEscapes(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	id := ts.seedModel(t)

	cases := []string{
		"../hidden/private.png", // another directory in the same library
		"../../outside.png",     // outside the library
		"..%2f..%2foutside.png", // encoded traversal
		"/etc/passwd",           // absolute
		"textures/../../hidden/private.png",
	}
	for _, name := range cases {
		resp := ts.get(t, itoa("/assets/%d/file/%s", id, name))
		body := ts.body(t, resp)
		if resp.StatusCode == http.StatusOK {
			t.Errorf("%s was served (status 200, %d bytes)", name, len(body))
		}
		// Assert on file *contents*, not on words from the path: net/http's
		// path-cleaning redirect echoes the URL, so a path-based check would fail for
		// the wrong reason.
		for _, leak := range []string{"PRIVATE-BYTES", "OUTSIDE-BYTES"} {
			if strings.Contains(body, leak) {
				t.Errorf("%s leaked %s", name, leak)
			}
		}
	}

	// And only the file types a model legitimately references are served at all.
	resp := ts.get(t, itoa("/assets/%d/file/../../../etc/hosts", id))
	if resp.StatusCode == http.StatusOK {
		t.Error("a non-model file type was served")
	}
	resp.Body.Close()
}

// TestObjViewerNeedsNoBlender is the user-visible half: an .obj page shows the
// viewer, names the OBJ loader, and does not tell the user to install Blender to
// look at their model.
func TestObjViewerNeedsNoBlender(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	id := ts.seedModel(t)

	body := ts.body(t, ts.get(t, itoa("/assets/%d", id)))
	for _, want := range []string{
		`id="model-viewer"`,
		`data-format="obj"`,
		`data-mtl="hero.mtl"`,
		"OBJLoader.js",
		"MTLLoader.js",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the .obj page is missing %q", want)
		}
	}
	if strings.Contains(body, "needs Blender to preview") {
		t.Error("an .obj must not be gated behind Blender any more")
	}
	// The viewer loads the original through the companion route, so relative
	// references resolve beside it.
	if !strings.Contains(body, itoa("/assets/%d/file/hero.obj", id)) {
		t.Error("the viewer should load the original .obj through the companion route")
	}
}

// TestModelViewerSourceFollowsWhatIsOnDisk is the regression test for the FBX bug.
//
// The viewer's source used to be derived from derive_state alone: state 'ok' meant
// "there is a normalised preview.glb", so the page pointed the loader at
// /assets/{id}/preview.glb. That held for glTF, which derive really does normalise —
// but a browser-rendered thumbnail also sets state 'ok', and it produces no .glb at
// all. Every .fbx therefore got a URL that 404s, and a 3D page that opened to an empty
// stage with no error anywhere.
//
// The source is now decided by what exists on disk, so the two cases cannot be
// confused: preview.glb when there is one, the original file otherwise.
func TestModelViewerSourceFollowsWhatIsOnDisk(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	ts.seedLibrary(t, map[string]string{
		"pack/models/barrel.fbx": "Kaydara FBX Binary" + "\x00",
		"pack/models/crate.glb":  "glTF",
	})
	fbxID := ts.assetID(t, "pack/models/barrel.fbx")
	glbID := ts.assetID(t, "pack/models/crate.glb")

	// Both are marked derived — the .glb by derive, the .fbx by a thumbnail upload —
	// but only the .glb has a preview.glb beside it.
	for _, id := range []int64{fbxID, glbID} {
		if _, err := ts.db.Writer.Exec(
			`UPDATE assets SET derive_state = 'ok' WHERE id = ?`, id); err != nil {
			t.Fatal(err)
		}
	}
	ts.writeDerivative(t, glbID, "preview.glb", []byte("glTF-normalised"))

	tests := []struct {
		name string
		id   int64
		want string
	}{
		{"a normalised preview is used when it exists", glbID, itoa("/assets/%d/preview.glb", glbID)},
		{"an fbx is loaded from the library, not from a preview that was never written",
			fbxID, itoa("/assets/%d/file/barrel.fbx", fbxID)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := ts.body(t, ts.get(t, itoa("/assets/%d", tc.id)))
			if !strings.Contains(body, `data-src="`+tc.want+`"`) {
				t.Errorf("the viewer source is not %s", tc.want)
			}
			// And the 2D image viewer has no business on a model page: it used to
			// render too, stacking an empty canvas under the 3D stage.
			if strings.Contains(body, `id="viewer2d"`) {
				t.Error("the 2D viewer should not appear on a model page")
			}
		})
	}
}

// TestModelTextureFoundInPack: an FBX records the absolute path the artist had on
// their own machine, so every loader falls back to the basename — and the basename is
// usually not beside the model. In this library the texture is real, four directories
// up in the pack's shared Textures/. Refusing it renders the model invisible, so it is
// looked up within the pack.
func TestModelTextureFoundInPack(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	ts.seedLibrary(t, map[string]string{
		// The layout KayKit ships: models deep under Assets/, textures at the pack root.
		"hexpack/Assets/fbx/decoration/props/barrel.fbx": "Kaydara FBX Binary\x00",
		"hexpack/Textures/hexagons_medieval.png":         "PACK-TEXTURE-BYTES",
		"hexpack/Assets/fbx/decoration/beside.png":       "BESIDE-BYTES",
		// A different pack, which must stay out of reach.
		"otherpack/Textures/hexagons_medieval.png": "OTHER-PACK-BYTES",
		"otherpack/Textures/only_in_other.png":     "OTHER-ONLY-BYTES",
		"otherpack/secret.png":                     "OTHER-SECRET-BYTES",
	})
	id := ts.assetID(t, "hexpack/Assets/fbx/decoration/props/barrel.fbx")

	// Found, from the pack root, by basename alone.
	resp := ts.get(t, itoa("/assets/%d/file/hexagons_medieval.png", id))
	body := ts.body(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the pack texture was not served: status = %d", resp.StatusCode)
	}
	if body != "PACK-TEXTURE-BYTES" {
		t.Errorf("served %q, want the texture from this pack", body)
	}

	// The lookup walks up through the model's own ancestors, so a texture part-way up
	// resolves too.
	resp = ts.get(t, itoa("/assets/%d/file/beside.png", id))
	if body := ts.body(t, resp); body != "BESIDE-BYTES" {
		t.Errorf("an ancestor directory's texture was not served: %q", body)
	}

	// It never leaves the pack. Two ways of asking: a basename that exists in both
	// packs must resolve to this one, and a basename that exists *only* in the other
	// pack must not resolve at all.
	if !strings.Contains(ts.body(t, ts.get(t, itoa("/assets/%d/file/hexagons_medieval.png", id))), "PACK-TEXTURE") {
		t.Error("the wrong pack's copy was served for a name present in both")
	}
	resp = ts.get(t, itoa("/assets/%d/file/only_in_other.png", id))
	if got := ts.body(t, resp); resp.StatusCode == http.StatusOK || strings.Contains(got, "OTHER-ONLY") {
		t.Errorf("a texture from another pack was served (status %d, %q)", resp.StatusCode, got)
	}
	for _, name := range []string{"secret.png", "../../../../otherpack/secret.png"} {
		resp := ts.get(t, itoa("/assets/%d/file/%s", id, name))
		if got := ts.body(t, resp); strings.Contains(got, "OTHER-SECRET-BYTES") {
			t.Errorf("%s leaked another pack's file", name)
		}
	}
}

// TestModelNonTextureStaysBesideTheModel: the pack-wide lookup is for textures only.
// A .mtl or a .bin is genuinely relative to its model, and serving a same-named file
// from elsewhere in the pack would hand the loader the wrong geometry.
func TestModelNonTextureStaysBesideTheModel(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	ts.seedLibrary(t, map[string]string{
		"pack/models/deep/hero.obj": "v 0 0 0\n",
		"pack/hero.mtl":             "WRONG-MTL-BYTES",
		"pack/scene.bin":            "WRONG-BIN-BYTES",
	})
	id := ts.assetID(t, "pack/models/deep/hero.obj")

	for _, name := range []string{"hero.mtl", "scene.bin"} {
		resp := ts.get(t, itoa("/assets/%d/file/%s", id, name))
		body := ts.body(t, resp)
		if resp.StatusCode == http.StatusOK {
			t.Errorf("%s was served from elsewhere in the pack (%q)", name, body)
		}
		if strings.Contains(body, "WRONG-") {
			t.Errorf("%s resolved to the wrong file", name)
		}
	}
}
