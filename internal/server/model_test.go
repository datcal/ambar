package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/datcal/ambar/internal/derive"
)

// insertModelAsset inserts a 3D asset with a preview.glb derivative and returns
// its id.
func (ts *testServer) insertModelAsset(t *testing.T, filename string) int64 {
	t.Helper()
	now := time.Now().Unix()
	sha := strings.Repeat("b", 64)

	ts.db.Writer.Exec(`
		INSERT INTO packs (name, slug, kind, library_rel_path, first_seen_at, last_seen_at, created_at, updated_at)
		VALUES ('models', 'models', 'folder', 'models', ?, ?, ?, ?)
		ON CONFLICT(library_rel_path) DO NOTHING`, now, now, now, now)
	var packID int64
	ts.db.Reader.QueryRow(`SELECT id FROM packs WHERE library_rel_path = 'models'`).Scan(&packID)

	res, err := ts.db.Writer.Exec(`
		INSERT INTO assets (pack_id, rel_path, filename, ext, kind, size, mtime, sha256,
		                    tri_count, vert_count, bbox_x, bbox_y, bbox_z, material_count,
		                    derive_state, derive_version,
		                    first_seen_at, last_verified_at, created_at, updated_at)
		VALUES (?, ?, ?, 'glb', 'model', 8, ?, ?, 1200, 640, 1.0, 1.8, 0.5, 2, 'ok', ?, ?, ?, ?, ?)`,
		packID, filename, filename, now, sha, derive.Version, now, now, now, now)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	ts.db.Writer.Exec(`INSERT INTO assets_fts (rowid, filename, pack_name, tag_text, notes) VALUES (?, ?, 'models', '', '')`, id, filename)

	rel, _ := derive.Dir(sha)
	dir := filepath.Join(ts.cfg.DataRoot, rel)
	os.MkdirAll(dir, 0o755)
	if err := os.WriteFile(filepath.Join(dir, derive.FileModelPreview), []byte("glTF-binary-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestModelDetailAndPreview(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	id := ts.insertModelAsset(t, "crate.glb")

	detail := readBody(t, ts.get(t, itoa("/assets/%d", id)))
	for _, want := range []string{
		`id="model-viewer"`,
		"/static/vendor/three/three.min.js",
		"/static/model-viewer.js",
		"Triangles", "1200", // metadata overlay
		"1.00 × 1.80 × 0.50 m",
	} {
		if !strings.Contains(detail, want) {
			t.Errorf("model detail page missing %q", want)
		}
	}

	// preview.glb serves inline with the glTF-binary type.
	prev := ts.get(t, itoa("/assets/%d/preview.glb", id))
	if prev.StatusCode != http.StatusOK {
		t.Fatalf("preview status = %d", prev.StatusCode)
	}
	if ct := prev.Header.Get("Content-Type"); ct != "model/gltf-binary" {
		t.Errorf("preview content-type = %q, want model/gltf-binary", ct)
	}
	prev.Body.Close()
}
