package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
