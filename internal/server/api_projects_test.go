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
