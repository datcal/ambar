package server

import (
	"fmt"
	"io"
	"net/url"
	"strings"
	"testing"
	"time"
)

// insertAsset adds a pack, an asset and its FTS row directly, returning the
// asset id — enough for the tag handlers, which do not read the file itself.
func (ts *testServer) insertAsset(t *testing.T, filename string) int64 {
	t.Helper()
	now := time.Now().Unix()
	// One shared pack across inserts; the asset's rel_path (the filename) is what
	// differs, so the (pack_id, rel_path) identity stays unique.
	if _, err := ts.db.Writer.Exec(`
		INSERT INTO packs (name, slug, kind, library_rel_path, first_seen_at, last_seen_at, created_at, updated_at)
		VALUES ('pack', 'pack', 'folder', 'pack', ?, ?, ?, ?)
		ON CONFLICT(library_rel_path) DO NOTHING`, now, now, now, now); err != nil {
		t.Fatalf("insert pack: %v", err)
	}
	var packID int64
	if err := ts.db.Reader.QueryRow(`SELECT id FROM packs WHERE library_rel_path = 'pack'`).Scan(&packID); err != nil {
		t.Fatalf("read pack id: %v", err)
	}
	res, err := ts.db.Writer.Exec(`
		INSERT INTO assets (pack_id, rel_path, filename, ext, kind, size, mtime, sha256,
		                    first_seen_at, last_verified_at, created_at, updated_at)
		VALUES (?, ?, ?, 'png', 'image', 1, ?, ?, ?, ?, ?, ?)`,
		packID, filename, filename, now, filename+"-hash", now, now, now, now)
	if err != nil {
		t.Fatalf("insert asset: %v", err)
	}
	id, _ := res.LastInsertId()
	if _, err := ts.db.Writer.Exec(`
		INSERT INTO assets_fts (rowid, filename, pack_name, tag_text, notes)
		VALUES (?, ?, 'pack', '', '')`, id, filename); err != nil {
		t.Fatalf("insert fts: %v", err)
	}
	return id
}

func (ts *testServer) postForm(t *testing.T, path string, form url.Values) (int, string) {
	t.Helper()
	form.Set("csrf_token", ts.csrfToken(t, "/"))
	resp, err := ts.client.PostForm(ts.URL+path, form)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func TestAssetTagAddRemoveFlow(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	id := ts.insertAsset(t, "sprite.png")

	// Add a tag: the returned fragment shows it.
	status, body := ts.postForm(t, itoa("/assets/%d/tags", id), url.Values{"tag": {"theme:sci-fi"}})
	if status != 200 {
		t.Fatalf("add status = %d\n%s", status, body)
	}
	if !strings.Contains(body, "theme:sci-fi</a>") {
		t.Errorf("tag not shown after add:\n%s", body)
	}

	// It is now on the detail page too.
	detail := ts.get(t, itoa("/assets/%d", id))
	db, _ := io.ReadAll(detail.Body)
	if !strings.Contains(string(db), "theme:sci-fi</a>") {
		t.Errorf("tag missing from detail page")
	}

	// Find the tag id to remove it. There is exactly one direct tag.
	var tagID int64
	if err := ts.db.Reader.QueryRow(
		`SELECT tag_id FROM asset_tags WHERE asset_id = ?`, id).Scan(&tagID); err != nil {
		t.Fatalf("read tag id: %v", err)
	}
	status, body = ts.postForm(t, itoa("/assets/%d/tags/remove", id), url.Values{"tag_id": {itoa("%d", tagID)}})
	if status != 200 {
		t.Fatalf("remove status = %d", status)
	}
	if strings.Contains(body, "theme:sci-fi</a>") {
		t.Errorf("tag still shown after remove:\n%s", body)
	}
}

func TestAssetTagInvalidIsRejected(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	id := ts.insertAsset(t, "sprite.png")

	status, body := ts.postForm(t, itoa("/assets/%d/tags", id), url.Values{"tag": {"no-namespace"}})
	if status != 422 {
		t.Errorf("status = %d, want 422 for a namespaceless tag", status)
	}
	if !strings.Contains(body, "valid tag") {
		t.Errorf("no error message shown:\n%s", body)
	}
	var n int
	ts.db.Reader.QueryRow(`SELECT count(*) FROM asset_tags WHERE asset_id = ?`, id).Scan(&n)
	if n != 0 {
		t.Errorf("an invalid tag was stored")
	}
}

func TestTagSuggest(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	id := ts.insertAsset(t, "sprite.png")
	ts.postForm(t, itoa("/assets/%d/tags", id), url.Values{"tag": {"theme:sci-fi"}})

	resp := ts.get(t, "/api/v1/tags/suggest?q=theme")
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `<option value="theme:sci-fi">`) {
		t.Errorf("suggest did not return the tag:\n%s", body)
	}
}

// itoa is a tiny sprintf helper to keep the path calls readable.
func itoa(format string, args ...any) string {
	return fmt.Sprintf(format, args...)
}

func TestBulkTagAllMatches(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	a := ts.insertAsset(t, "one.png")
	b := ts.insertAsset(t, "two.png")

	// Tag everything matching an empty search (the whole library).
	status, _ := ts.postForm(t, "/assets/tags/bulk", urlValues(
		"tag", "batch:reviewed", "scope", "all", "q", ""))
	if status != 303 && status != 200 {
		t.Fatalf("bulk status = %d", status)
	}
	for _, id := range []int64{a, b} {
		var n int
		ts.db.Reader.QueryRow(`SELECT count(*) FROM asset_tags at JOIN tags t ON t.id=at.tag_id
			WHERE at.asset_id=? AND t.namespace='batch'`, id).Scan(&n)
		if n != 1 {
			t.Errorf("asset %d not bulk-tagged (n=%d)", id, n)
		}
	}
}

func TestBulkTagSelected(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	a := ts.insertAsset(t, "one.png")
	b := ts.insertAsset(t, "two.png")

	form := urlValues("tag", "batch:pick", "scope", "selected")
	form.Add("id", itoa("%d", a)) // only a
	form.Set("csrf_token", ts.csrfToken(t, "/"))
	resp, err := ts.client.PostForm(ts.URL+"/assets/tags/bulk", form)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	var na, nb int
	ts.db.Reader.QueryRow(`SELECT count(*) FROM asset_tags at JOIN tags t ON t.id=at.tag_id WHERE at.asset_id=? AND t.namespace='batch'`, a).Scan(&na)
	ts.db.Reader.QueryRow(`SELECT count(*) FROM asset_tags at JOIN tags t ON t.id=at.tag_id WHERE at.asset_id=? AND t.namespace='batch'`, b).Scan(&nb)
	if na != 1 || nb != 0 {
		t.Errorf("selected-only tagging wrong: a=%d b=%d, want 1 0", na, nb)
	}
}

func TestSavedSearchFlow(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)

	status, _ := ts.postForm(t, "/searches", urlValues("name", "props", "q", "type:model"))
	if status != 303 && status != 200 {
		t.Fatalf("save status = %d", status)
	}
	// It now shows on the assets page.
	resp := ts.get(t, "/assets")
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "props") {
		t.Errorf("saved search not listed on the assets page")
	}
}

// urlValues builds url.Values from key/value pairs.
func urlValues(kv ...string) url.Values {
	v := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		v.Set(kv[i], kv[i+1])
	}
	return v
}
