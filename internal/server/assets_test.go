package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/datcal/ambar/internal/index"
)

// seedLibrary writes files into the test server's library and indexes them.
func (ts *testServer) seedLibrary(t *testing.T, files map[string]string) {
	t.Helper()

	for relPath, content := range files {
		full := filepath.Join(ts.cfg.LibraryRoot, filepath.FromSlash(relPath))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	indexer := index.New(ts.db, index.Options{Root: ts.cfg.LibraryRoot})
	if _, err := indexer.Scan(context.Background(), index.ScanOptions{}); err != nil {
		t.Fatalf("seed scan: %v", err)
	}
}

// assetID looks up an indexed asset by its library-relative path.
func (ts *testServer) assetID(t *testing.T, libPath string) int64 {
	t.Helper()

	indexer := index.New(ts.db, index.Options{Root: ts.cfg.LibraryRoot})
	page, err := indexer.List(context.Background(), index.ListOptions{
		IncludeMissing: true, Limit: index.MaxPageSize,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range page.Assets {
		if a.LibraryPath() == libPath {
			return a.ID
		}
	}
	t.Fatalf("%q is not indexed", libPath)
	return 0
}

func (ts *testServer) body(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// --- the grid ---------------------------------------------------------------

func TestAssetsRequiresLogin(t *testing.T) {
	ts := newTestServer(t)

	for _, path := range []string{"/assets", "/assets/1", "/assets/1/download"} {
		resp := ts.get(t, path)
		if resp.StatusCode != http.StatusSeeOther {
			t.Errorf("GET %s anonymous = %d, want 303", path, resp.StatusCode)
		}
	}
}

func TestAssetsGridListsIndexedFiles(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	ts.seedLibrary(t, map[string]string{
		"kenney-medieval/wooden_sword_01.glb": "sword bytes",
		"kenney-medieval/README.txt":          "docs",
		"kenney-scifi/laser_turret.glb":       "turret bytes",
	})

	resp := ts.get(t, "/assets")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := ts.body(t, resp)

	for _, want := range []string{
		"wooden_sword_01.glb", "laser_turret.glb", "kenney-medieval", "kenney-scifi", "model",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the grid does not show %q", want)
		}
	}
}

func TestAssetsGridEmptyStateExplainsScan(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)

	body := ts.body(t, ts.get(t, "/assets"))
	if !strings.Contains(body, "ambar scan") {
		t.Error("the empty grid does not tell the user to run ambar scan")
	}
}

func TestAssetsSearch(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	ts.seedLibrary(t, map[string]string{
		"pack-a/wooden_sword_01.glb": "a",
		"pack-b/laser_turret.glb":    "b",
	})

	found := ts.body(t, ts.get(t, "/assets?q=sword"))
	if !strings.Contains(found, "wooden_sword_01.glb") {
		t.Error("searching 'sword' did not find wooden_sword_01.glb")
	}
	if strings.Contains(found, "laser_turret.glb") {
		t.Error("searching 'sword' also returned laser_turret.glb")
	}

	// Prefix matching, so typing works.
	if !strings.Contains(ts.body(t, ts.get(t, "/assets?q=swo")), "wooden_sword_01.glb") {
		t.Error("prefix search 'swo' did not match")
	}

	empty := ts.body(t, ts.get(t, "/assets?q=zzznothing"))
	if !strings.Contains(empty, "Nothing matched") {
		t.Error("a search with no results does not say so")
	}
}

// TestAssetsSearchSurvivesHostileInput is the user-facing half of the FTS5
// escaping: a lone double quote in the search box must not produce a 500.
func TestAssetsSearchSurvivesHostileInput(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	ts.seedLibrary(t, map[string]string{"pack/sword.png": "a"})

	for _, q := range []string{
		`"`, `""`, `"unterminated`, `AND`, `NEAR(`, `(((`, `*`, `^`,
		`'; DROP TABLE assets; --`, `<script>alert(1)</script>`,
		strings.Repeat("x", 3000),
	} {
		resp := ts.get(t, "/assets?q="+urlEncode(q))
		if resp.StatusCode != http.StatusOK {
			t.Errorf("search %q returned %d, want 200", truncateForLog(q), resp.StatusCode)
		}
		body := ts.body(t, resp)
		// And the reflected query must be escaped, not injected.
		if strings.Contains(body, "<script>alert(1)</script>") {
			t.Error("the search term was reflected unescaped — that is XSS")
		}
	}

	// The table survived the injection attempt.
	var n int
	if err := ts.db.Reader.QueryRow(`SELECT count(*) FROM assets`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("%d assets after hostile input, want 1", n)
	}
}

func TestAssetsKindFilter(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	ts.seedLibrary(t, map[string]string{
		"pack/sprite.png": "a",
		"pack/turret.glb": "b",
		"pack/impact.wav": "c",
	})

	body := ts.body(t, ts.get(t, "/assets?kind=model"))
	if !strings.Contains(body, "turret.glb") {
		t.Error("the model filter did not include turret.glb")
	}
	for _, unwanted := range []string{"sprite.png", "impact.wav"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("the model filter also returned %q", unwanted)
		}
	}
}

func TestAssetsPaginationLink(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)

	files := map[string]string{}
	for i := 0; i < index.DefaultPageSize+10; i++ {
		files[fmt.Sprintf("pack/sprite_%04d.png", i)] = fmt.Sprintf("content %d", i)
	}
	ts.seedLibrary(t, files)

	body := ts.body(t, ts.get(t, "/assets"))
	if !strings.Contains(body, "Load more") {
		t.Fatal("no next-page control on a multi-page result")
	}
	if !strings.Contains(body, "cursor=") {
		t.Error("the next-page link carries no cursor")
	}
}

// TestAssetsPaginationPreservesFilters: paging through a search must not silently
// drop the search.
func TestAssetsPaginationPreservesFilters(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)

	files := map[string]string{}
	for i := 0; i < index.DefaultPageSize+10; i++ {
		files[fmt.Sprintf("pack/sword_%04d.png", i)] = fmt.Sprintf("content %d", i)
	}
	ts.seedLibrary(t, files)

	body := ts.body(t, ts.get(t, "/assets?q=sword"))
	idx := strings.Index(body, "cursor=")
	if idx < 0 {
		t.Fatal("no cursor in the next-page link")
	}
	// The link must still carry q=sword.
	tail := body[max(0, idx-200):]
	if !strings.Contains(tail, "q=sword") {
		t.Errorf("the next-page link lost the search filter: %s", tail[:min(200, len(tail))])
	}
}

func TestAssetsMalformedCursorRedirects(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	ts.seedLibrary(t, map[string]string{"pack/a.png": "a"})

	resp := ts.get(t, "/assets?cursor=nonsense")
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("status = %d, want 303 — a bad cursor is a bad URL, not a server fault", resp.StatusCode)
	}
}

// --- the detail page --------------------------------------------------------

func TestAssetDetail(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	ts.seedLibrary(t, map[string]string{"kenney-medieval/wooden_sword_01.glb": "sword bytes"})

	id := ts.assetID(t, "kenney-medieval/wooden_sword_01.glb")
	body := ts.body(t, ts.get(t, fmt.Sprintf("/assets/%d", id)))

	for _, want := range []string{
		"wooden_sword_01.glb",
		"kenney-medieval/wooden_sword_01.glb",
		"model",
		"download",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the detail page does not show %q", want)
		}
	}
}

func TestAssetDetailNotFound(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)

	for _, path := range []string{"/assets/999999", "/assets/0", "/assets/-1", "/assets/abc"} {
		resp := ts.get(t, path)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, resp.StatusCode)
		}
	}
}

// --- download ---------------------------------------------------------------

func TestAssetDownload(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	ts.seedLibrary(t, map[string]string{"pack/wooden_sword.glb": "the actual bytes"})

	id := ts.assetID(t, "pack/wooden_sword.glb")
	resp := ts.get(t, fmt.Sprintf("/assets/%d/download", id))

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := ts.body(t, resp); got != "the actual bytes" {
		t.Errorf("body = %q, want the file contents", got)
	}

	// §11: an inline .svg or .html served from the app origin is stored XSS, so
	// library content is always an attachment and never sniffable.
	cd := resp.Header.Get("Content-Disposition")
	if !strings.HasPrefix(cd, "attachment") {
		t.Errorf("Content-Disposition = %q, want attachment", cd)
	}
	if !strings.Contains(cd, "wooden_sword.glb") {
		t.Errorf("Content-Disposition = %q, want the filename", cd)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("Content-Type = %q, want application/octet-stream", got)
	}
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q", got)
	}
	if resp.Header.Get("ETag") == "" {
		t.Error("no ETag; §10 wants resumable downloads and this is the validator")
	}
}

// TestAssetDownloadServesHtmlAsAttachment is the case §11 names explicitly.
func TestAssetDownloadServesHtmlAsAttachment(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	ts.seedLibrary(t, map[string]string{
		"pack/evil.html": `<script>alert(document.cookie)</script>`,
		"pack/evil.svg":  `<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`,
	})

	for _, name := range []string{"pack/evil.html", "pack/evil.svg"} {
		id := ts.assetID(t, name)
		resp := ts.get(t, fmt.Sprintf("/assets/%d/download", id))

		if !strings.HasPrefix(resp.Header.Get("Content-Disposition"), "attachment") {
			t.Errorf("%s is not served as an attachment — that is stored XSS", name)
		}
		if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "html") || strings.Contains(ct, "svg") {
			t.Errorf("%s served with Content-Type %q; the browser would render it", name, ct)
		}
	}
}

// TestAssetDownloadRefusesEscapingPath is the safepath integration test: a
// rel_path that points outside the library — from a bad migration or a
// hand-edited database — must not be served.
func TestAssetDownloadRefusesEscapingPath(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	ts.seedLibrary(t, map[string]string{"pack/legit.png": "fine"})

	// A file that genuinely exists, outside the library root.
	secret := filepath.Join(filepath.Dir(ts.cfg.LibraryRoot), "secret.txt")
	if err := os.WriteFile(secret, []byte("SECRET CONTENTS"), 0o644); err != nil {
		t.Fatal(err)
	}

	id := ts.assetID(t, "pack/legit.png")
	for _, evil := range []string{
		"../secret.txt",
		"../../secret.txt",
		"pack/../../secret.txt",
		"/etc/passwd",
		"pack/../../../../../../etc/passwd",
	} {
		if _, err := ts.db.Writer.Exec(`UPDATE assets SET rel_path = ? WHERE id = ?`, evil, id); err != nil {
			t.Fatal(err)
		}

		resp := ts.get(t, fmt.Sprintf("/assets/%d/download", id))
		body := ts.body(t, resp)

		if resp.StatusCode == http.StatusOK {
			t.Errorf("rel_path %q was served with 200 — path traversal", evil)
		}
		if strings.Contains(body, "SECRET CONTENTS") {
			t.Errorf("rel_path %q leaked a file outside the library", evil)
		}
		if strings.Contains(body, "root:") {
			t.Errorf("rel_path %q leaked /etc/passwd", evil)
		}
		// The response must not disclose the resolved path either.
		if strings.Contains(body, secret) {
			t.Errorf("rel_path %q disclosed the absolute path in the response", evil)
		}
	}
}

// TestAssetDownloadOfMissingFile: the row is kept (§12), so the route must answer
// coherently rather than 500.
func TestAssetDownloadOfMissingFile(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	ts.seedLibrary(t, map[string]string{
		"pack/gone.png": "bytes",
		"pack/stay.png": "bytes too",
	})

	id := ts.assetID(t, "pack/gone.png")

	// Delete the file and rescan so the row is marked missing.
	if err := os.Remove(filepath.Join(ts.cfg.LibraryRoot, "pack", "gone.png")); err != nil {
		t.Fatal(err)
	}
	indexer := index.New(ts.db, index.Options{Root: ts.cfg.LibraryRoot})
	if _, err := indexer.Scan(context.Background(), index.ScanOptions{}); err != nil {
		t.Fatal(err)
	}

	resp := ts.get(t, fmt.Sprintf("/assets/%d/download", id))
	if resp.StatusCode != http.StatusGone {
		t.Errorf("status = %d, want 410", resp.StatusCode)
	}
	if !strings.Contains(ts.body(t, resp), "not present") {
		t.Error("the response does not explain why there is nothing to download")
	}

	// The detail page must still work, and must say the row was kept.
	detail := ts.body(t, ts.get(t, fmt.Sprintf("/assets/%d", id)))
	if !strings.Contains(detail, "not present at the last scan") {
		t.Error("the detail page does not explain the missing state")
	}
}

// TestAssetDownloadSupportsRange matters for §10's "a 200 MB model download that
// drops should resume, not restart".
func TestAssetDownloadSupportsRange(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	ts.seedLibrary(t, map[string]string{"pack/model.glb": "0123456789abcdef"})

	id := ts.assetID(t, "pack/model.glb")

	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/assets/%d/download", ts.URL, id), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Range", "bytes=4-7")
	for _, c := range ts.client.Jar.Cookies(mustParseURL(t, ts.URL)) {
		req.AddCookie(c)
	}

	resp, err := ts.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", resp.StatusCode)
	}
	if got := ts.body(t, resp); got != "4567" {
		t.Errorf("range body = %q, want 4567", got)
	}
}

// TestAssetDownloadFilenameHeaderIsSafe: a filename containing a quote must not
// break out of the header.
func TestAssetDownloadFilenameHeaderIsSafe(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	ts.seedLibrary(t, map[string]string{`pack/od"d;name.png`: "bytes"})

	id := ts.assetID(t, `pack/od"d;name.png`)
	resp := ts.get(t, fmt.Sprintf("/assets/%d/download", id))

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	cd := resp.Header.Get("Content-Disposition")
	if !strings.HasPrefix(cd, "attachment") {
		t.Errorf("Content-Disposition = %q", cd)
	}
	// A raw unescaped quote would have terminated the parameter early.
	if strings.Contains(cd, `"od"d`) {
		t.Errorf("the filename was not escaped: %q", cd)
	}
}

// --- the landing page -------------------------------------------------------

// TestRootIsTheLibrary: M14 made the library the front door. "The whole point is
// the assets", so / is the grid, not a dashboard of counts.
func TestRootIsTheLibrary(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)

	// Empty index first: the grid's empty state is where a new user is told what to do.
	if body := ts.body(t, ts.get(t, "/")); !strings.Contains(body, "ambar scan") {
		t.Error("the empty library does not tell a new user to run ambar scan")
	}

	ts.seedLibrary(t, map[string]string{"pack/a.png": "a", "pack/b.png": "bb"})

	body := ts.body(t, ts.get(t, "/"))
	for _, want := range []string{"thumbgrid", "a.png", "b.png"} {
		if !strings.Contains(body, want) {
			t.Errorf("/ should render the grid; %q is missing", want)
		}
	}
	// The three-pane shell, with navigation permanently on the left.
	for _, want := range []string{`class="app"`, "panel-left", "Kinds", "tilesize"} {
		if !strings.Contains(body, want) {
			t.Errorf("/ should render the workspace shell; %q is missing", want)
		}
	}
	// And it is the same page as /assets, so old links keep working.
	if alt := ts.body(t, ts.get(t, "/assets")); !strings.Contains(alt, "thumbgrid") {
		t.Error("/assets should still render the grid")
	}
}

// TestStatusPageShowsIndexStats: the old dashboard, now one page along.
func TestStatusPageShowsIndexStats(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	ts.seedLibrary(t, map[string]string{"pack/a.png": "a", "pack/b.png": "bb"})

	body := ts.body(t, ts.get(t, "/status"))
	if !strings.Contains(body, "Browse assets") {
		t.Error("the status page has no link to the library")
	}
	// A document page, not the workspace shell.
	if strings.Contains(body, "panel-left") {
		t.Error("the status page should stay a centred document")
	}
}

func TestFormatBytes(t *testing.T) {
	tests := map[int64]string{
		0: "0 B", 1: "1 B", 999: "999 B",
		1024: "1.0 KB", 1536: "1.5 KB",
		1024 * 1024:                "1.0 MB",
		1024 * 1024 * 20:           "20 MB",
		1024 * 1024 * 1024 * 3 / 2: "1.5 GB",
	}
	for in, want := range tests {
		if got := FormatBytes(in); got != want {
			t.Errorf("FormatBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestLibraryDir(t *testing.T) {
	tests := map[string]string{
		"pack/PNG/sprite.png": "pack/PNG",
		"pack/sprite.png":     "pack",
		"sprite.png":          "",
	}
	for in, want := range tests {
		if got := libraryDir(in); got != want {
			t.Errorf("libraryDir(%q) = %q, want %q", in, got, want)
		}
	}
}

func urlEncode(s string) string {
	var b strings.Builder
	for _, r := range []byte(s) {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '-' || r == '_' || r == '.' || r == '~' {
			b.WriteByte(r)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", r)
	}
	return b.String()
}

func truncateForLog(s string) string {
	if len(s) > 40 {
		return s[:40] + "..."
	}
	return s
}

// TestFolderTreeNavigation covers M14's sidebar: the tree with counts, and the
// click-through that filters the grid to one directory.
func TestFolderTreeNavigation(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	ts.seedLibrary(t, map[string]string{
		"2d/pack-a/PNG/hero.png": "a",
		"2d/pack-a/PNG/tree.png": "b",
		"2d/pack-b/sprite.png":   "c",
		"audio/hit.wav":          "d",
	})

	body := ts.body(t, ts.get(t, "/"))
	if !strings.Contains(body, "Folders") {
		t.Fatal("the sidebar has no folder tree")
	}
	// The href percent-encodes the separator (html/template does this in a query
	// context), so the readable assertion is on the title attribute.
	for _, want := range []string{`title="2d"`, `title="audio"`, "whole library"} {
		if !strings.Contains(body, want) {
			t.Errorf("the tree is missing %q", want)
		}
	}
	// Collapsed by default: pack-a's children are not rendered until it is opened.
	if strings.Contains(body, `title="2d/pack-a/PNG"`) {
		t.Error("deep branches should stay collapsed until browsed")
	}

	// Browsing a directory filters the grid and opens that branch.
	body = ts.body(t, ts.get(t, "/?dir=2d/pack-a"))
	if !strings.Contains(body, "hero.png") || !strings.Contains(body, "tree.png") {
		t.Error("browsing 2d/pack-a should show its assets")
	}
	if strings.Contains(body, "sprite.png") || strings.Contains(body, "hit.wav") {
		t.Error("browsing a directory must exclude everything outside it")
	}
	if !strings.Contains(body, `title="2d/pack-a/PNG"`) {
		t.Error("the browsed branch should be expanded")
	}
	if !strings.Contains(body, `class="on"`) {
		t.Error("the browsed node should be marked as current")
	}

	// A traversal in the parameter browses the whole library rather than erroring.
	body = ts.body(t, ts.get(t, "/?dir=../../etc"))
	if !strings.Contains(body, "hero.png") || !strings.Contains(body, "hit.wav") {
		t.Errorf("a bad dir should fall back to the whole library:\n%s", body[:200])
	}
}
