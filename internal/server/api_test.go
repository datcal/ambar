package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/datcal/ambar/internal/auth"
)

// apiToken creates a read/write token for the test user and returns the plaintext.
func (ts *testServer) apiToken(t *testing.T) string {
	t.Helper()
	var uid int64
	if err := ts.db.Reader.QueryRow(`SELECT id FROM users WHERE username = ?`, testUsername).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	plain, _, err := auth.NewTokenStore(ts.db).Create(context.Background(), uid, "test", []string{"write"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return plain
}

func (ts *testServer) apiGet(t *testing.T, path, token string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, ts.URL+path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := ts.client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestAPIRequiresToken(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)

	// No token → 401 JSON.
	resp := ts.apiGet(t, "/api/v1/search", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no token status = %d, want 401", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("error content-type = %q", ct)
	}
	// A bogus token → 401.
	if r := ts.apiGet(t, "/api/v1/search", "ambar_nope"); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("bogus token status = %d, want 401", r.StatusCode)
	}
}

func TestAPISearchAndAsset(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	id := ts.insertAsset(t, "hero.png")
	token := ts.apiToken(t)

	// Search returns the asset.
	resp := ts.apiGet(t, "/api/v1/search?q=hero", token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("search status = %d", resp.StatusCode)
	}
	var search struct {
		Assets []struct {
			ID       int64             `json:"id"`
			Filename string            `json:"filename"`
			Links    map[string]string `json:"links"`
		} `json:"assets"`
		Total int `json:"total"`
	}
	json.NewDecoder(resp.Body).Decode(&search)
	if search.Total != 1 || len(search.Assets) != 1 || search.Assets[0].Filename != "hero.png" {
		t.Fatalf("search result = %+v", search)
	}
	if search.Assets[0].Links["file"] == "" {
		t.Errorf("asset has no file link")
	}

	// The asset endpoint returns detail + tags.
	resp = ts.apiGet(t, itoa("/api/v1/assets/%d", id), token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("asset status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"hero.png"`) {
		t.Errorf("asset body = %s", body)
	}

	// A missing asset → 404 JSON.
	if r := ts.apiGet(t, "/api/v1/assets/99999", token); r.StatusCode != http.StatusNotFound {
		t.Errorf("missing asset status = %d, want 404", r.StatusCode)
	}
}

func TestAPITagsAutocomplete(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	id := ts.insertAsset(t, "sprite.png")
	token := ts.apiToken(t)
	// Tag it so there is something to autocomplete.
	ts.postForm(t, itoa("/assets/%d/tags", id), url.Values{"tag": {"theme:sci-fi"}})

	resp := ts.apiGet(t, "/api/v1/tags?prefix=theme", token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tags status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "theme:sci-fi") {
		t.Errorf("autocomplete missing the tag: %s", body)
	}
}
