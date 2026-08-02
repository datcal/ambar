package server

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/datcal/ambar/internal/auth"
)

func (ts *testServer) apiDo(t *testing.T, method, path, token, body string) *http.Response {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = bytes.NewBufferString(body)
	}
	req, _ := http.NewRequest(method, ts.URL+path, r)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// readToken mints a read-only token for the test user.
func (ts *testServer) readToken(t *testing.T) string {
	t.Helper()
	var uid int64
	ts.db.Reader.QueryRow(`SELECT id FROM users WHERE username = ?`, testUsername).Scan(&uid)
	plain, _, err := auth.NewTokenStore(ts.db).Create(context.Background(), uid, "ro", []string{"read"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return plain
}

func TestAPIProjectUsesAndCredits(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	id := ts.insertAsset(t, "turret.glb")
	write := ts.apiToken(t) // read+write
	const uuid = "abcdef01-2345-6789-abcd-ef0123456789"

	// A read-only token cannot record a use.
	ro := ts.readToken(t)
	if r := ts.apiDo(t, http.MethodPost, "/api/v1/projects/"+uuid+"/uses", ro,
		`{"asset_id":`+itoa("%d", id)+`,"res_path":"res://a/x.glb"}`); r.StatusCode != http.StatusForbidden {
		t.Errorf("read token POST status = %d, want 403", r.StatusCode)
	}

	// Write token records the use.
	resp := ts.apiDo(t, http.MethodPost, "/api/v1/projects/"+uuid+"/uses", write,
		`{"asset_id":`+itoa("%d", id)+`,"res_path":"res://assets/models/turret.glb","sha256":"abc","project_name":"My Game"}`)
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("record use status = %d: %s", resp.StatusCode, body)
	}

	// It shows up in the database.
	var n int
	ts.db.Reader.QueryRow(`SELECT count(*) FROM project_uses WHERE removed_at IS NULL`).Scan(&n)
	if n != 1 {
		t.Errorf("%d active uses, want 1", n)
	}

	// credits.md renders as markdown.
	cred := ts.apiDo(t, http.MethodGet, "/api/v1/projects/"+uuid+"/credits.md", ro, "")
	if ct := cred.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/markdown") {
		t.Errorf("credits content-type = %q", ct)
	}
	body, _ := io.ReadAll(cred.Body)
	if !strings.Contains(string(body), "# Credits — My Game") {
		t.Errorf("credits body = %s", body)
	}

	// Remove the use.
	var useID int64
	ts.db.Reader.QueryRow(`SELECT id FROM project_uses`).Scan(&useID)
	del := ts.apiDo(t, http.MethodDelete, itoa("/api/v1/projects/"+uuid+"/uses/%d", useID), write, "")
	if del.StatusCode != http.StatusNoContent {
		t.Errorf("delete status = %d, want 204", del.StatusCode)
	}
}

// TestAPIProjectUsesListing covers M18's "in this project" screen: the plugin needs to see what
// the server thinks it holds, which of those the library has moved on from, and — by absence —
// which of its own manifest entries were never recorded because the server was unreachable.
func TestAPIProjectUsesListing(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.seedLibrary(t, map[string]string{
		"pack/turret.glb": "turret bytes",
		"pack/crate.png":  "crate bytes",
	})
	write := ts.apiToken(t)
	ro := ts.readToken(t)
	const uuid = "11111111-2222-3333-4444-555555555555"

	turret := ts.assetID(t, "pack/turret.glb")
	crate := ts.assetID(t, "pack/crate.png")

	// An unknown project is an empty list, not a 404: nothing imported yet is an ordinary
	// state and the plugin should show an empty screen rather than an error.
	var empty struct {
		Uses []map[string]any `json:"uses"`
	}
	ts.apiJSON(t, "/api/v1/projects/"+uuid+"/uses", ro, &empty)
	if len(empty.Uses) != 0 {
		t.Fatalf("a project with no imports listed %d uses", len(empty.Uses))
	}

	// Record two: one with the library's current hash, one with a stale hash — which is what
	// an import made before the file changed in the library looks like.
	var currentSHA string
	ts.db.Reader.QueryRow(`SELECT sha256 FROM assets WHERE id = ?`, turret).Scan(&currentSHA)
	if r := ts.apiDo(t, http.MethodPost, "/api/v1/projects/"+uuid+"/uses", write,
		`{"asset_id":`+itoa("%d", turret)+`,"res_path":"res://assets/model/pack/turret.glb","sha256":"`+currentSHA+`","project_name":"My Game"}`,
	); r.StatusCode != http.StatusCreated {
		t.Fatalf("record turret = %d", r.StatusCode)
	}
	if r := ts.apiDo(t, http.MethodPost, "/api/v1/projects/"+uuid+"/uses", write,
		`{"asset_id":`+itoa("%d", crate)+`,"res_path":"res://assets/image/pack/crate.png","sha256":"a-hash-from-before"}`,
	); r.StatusCode != http.StatusCreated {
		t.Fatalf("record crate = %d", r.StatusCode)
	}

	var got struct {
		Uses []struct {
			ID       int64  `json:"id"`
			AssetID  int64  `json:"asset_id"`
			ResPath  string `json:"res_path"`
			Filename string `json:"filename"`
			Kind     string `json:"kind"`
			Pack     string `json:"pack"`
			Size     int64  `json:"size"`
			SHA256   string `json:"sha256"`
			Imported string `json:"imported_sha256"`
			Outdated bool   `json:"outdated"`
			Missing  bool   `json:"missing"`
		} `json:"uses"`
		Project string `json:"project"`
	}
	ts.apiJSON(t, "/api/v1/projects/"+uuid+"/uses", ro, &got)

	if len(got.Uses) != 2 || got.Project != uuid {
		t.Fatalf("uses = %+v (project %q)", got.Uses, got.Project)
	}
	byName := map[string]int{}
	for i, u := range got.Uses {
		byName[u.Filename] = i
	}
	turretUse := got.Uses[byName["turret.glb"]]
	crateUse := got.Uses[byName["crate.png"]]

	if turretUse.Outdated {
		t.Error("turret was imported at the library's current hash and is not outdated")
	}
	if !crateUse.Outdated {
		t.Errorf("crate was imported at %q and the library now holds %q; that is outdated",
			crateUse.Imported, crateUse.SHA256)
	}
	if turretUse.ResPath != "res://assets/model/pack/turret.glb" || turretUse.Pack != "pack" {
		t.Errorf("turret use = %+v", turretUse)
	}
	if turretUse.ID == 0 || turretUse.AssetID != turret {
		t.Errorf("turret use is missing the ids needed to act on it: %+v", turretUse)
	}
	if turretUse.Size == 0 || turretUse.Kind == "" {
		t.Errorf("turret use has no size or kind: %+v", turretUse)
	}

	// A removed use drops out, which is what makes "remove from project" show up in the
	// screen immediately rather than after a rescan.
	if r := ts.apiDo(t, http.MethodDelete,
		itoa("/api/v1/projects/"+uuid+"/uses/%d", turretUse.ID), write, ""); r.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %d", r.StatusCode)
	}
	var after struct {
		Uses []struct {
			Filename string `json:"filename"`
		} `json:"uses"`
	}
	ts.apiJSON(t, "/api/v1/projects/"+uuid+"/uses", ro, &after)
	if len(after.Uses) != 1 || after.Uses[0].Filename != "crate.png" {
		t.Errorf("after removal: %+v", after.Uses)
	}
}
