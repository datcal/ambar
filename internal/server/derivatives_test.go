package server

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/datcal/ambar/internal/derive"
	"github.com/datcal/ambar/internal/index"
	"github.com/datcal/ambar/internal/jobs"
)

// seedWithDerivatives writes files, scans, and generates derivatives synchronously.
func (ts *testServer) seedWithDerivatives(t *testing.T, files map[string][]byte) {
	t.Helper()

	for relPath, content := range files {
		full := filepath.Join(ts.cfg.LibraryRoot, filepath.FromSlash(relPath))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	ctx := context.Background()
	indexer := index.New(ts.db, index.Options{Root: ts.cfg.LibraryRoot})
	if _, err := indexer.Scan(ctx, index.ScanOptions{}); err != nil {
		t.Fatalf("seed scan: %v", err)
	}

	queue := jobs.New(ts.db, jobs.Options{Workers: 1})
	deriver := derive.New(ts.db, derive.Options{
		LibraryRoot: ts.cfg.LibraryRoot, DataRoot: ts.cfg.DataRoot,
	})
	deriver.Register(queue)

	if _, err := derive.EnqueueStale(ctx, ts.db, queue); err != nil {
		t.Fatal(err)
	}
	// Drain synchronously so the test is deterministic — no sleeping, no polling.
	drainQueue(t, ts, queue, deriver)
}

// drainQueue runs every pending derive job in the calling goroutine.
func drainQueue(t *testing.T, ts *testServer, queue *jobs.Queue, deriver *derive.Deriver) {
	t.Helper()

	ctx := context.Background()
	for i := 0; i < 500; i++ {
		var (
			id      int64
			payload string
		)
		err := ts.db.Writer.QueryRowContext(ctx, `
			UPDATE jobs SET state='running', attempts=attempts+1, started_at=0
			WHERE id = (SELECT id FROM jobs WHERE state='queued' ORDER BY priority DESC, id LIMIT 1)
			RETURNING id, payload_json`).Scan(&id, &payload)
		if err != nil {
			return // nothing left to claim
		}

		if handlerErr := deriver.Handle(ctx, []byte(payload)); handlerErr != nil {
			if _, err := ts.db.Writer.Exec(
				`UPDATE jobs SET state='failed', last_error=? WHERE id=?`,
				handlerErr.Error(), id); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if _, err := ts.db.Writer.Exec(`UPDATE jobs SET state='done' WHERE id=?`, id); err != nil {
			t.Fatal(err)
		}
	}
	t.Fatal("the queue did not drain within 500 jobs")
}

// spritePNG builds a small pixel-art PNG: few flat colours, hard edges.
func spritePNG(t *testing.T, w, h int) []byte {
	t.Helper()

	palette := []color.RGBA{
		{A: 0xff},
		{R: 0xff, G: 0xff, B: 0xff, A: 0xff},
		{R: 0xd9, G: 0x3f, B: 0x3f, A: 0xff},
		{R: 0x3f, G: 0x8f, B: 0xd9, A: 0xff},
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, palette[((x/4)+(y/4))%len(palette)])
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// --- the group-based grid ---------------------------------------------------

// TestGridShowsOneTilePerArtwork is §5.1 through the UI: the PNG, PSD and ASEPRITE of
// one sprite are one tile, not three.
func TestGridShowsOneTilePerArtwork(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)

	ts.seedWithDerivatives(t, map[string][]byte{
		"pack/PNG/Plant1/idle.png":           spritePNG(t, 32, 32),
		"pack/PSD/Plant1/idle.psd":           []byte("not really a psd"),
		"pack/ASEPRITE/Plant1/idle.aseprite": []byte("not really an aseprite"),
	})

	body := ts.body(t, ts.get(t, "/assets"))

	// One tile.
	if got := strings.Count(body, `<li class="tile`); got != 1 {
		t.Errorf("the grid shows %d tiles, want 1 (§5.1 collapses format variants)", got)
	}
	// Labelled with the variant count, so the sources are discoverable.
	if !strings.Contains(body, "3 variants") {
		t.Error("the tile does not show a variant count")
	}
	// The primary is the PNG, so the tile is named after it.
	if !strings.Contains(body, "idle.png") {
		t.Error("the tile is not named after the PNG primary")
	}
	// And it tells the user the file count differs from the tile count.
	if !strings.Contains(body, "collapsed by format variant") {
		t.Error("the grid does not explain that files were collapsed")
	}
}

func TestGridShowsThumbnails(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	ts.seedWithDerivatives(t, map[string][]byte{"pack/sprite.png": spritePNG(t, 32, 32)})

	body := ts.body(t, ts.get(t, "/assets"))
	if !strings.Contains(body, "/thumb") {
		t.Error("the grid has no thumbnail image")
	}
	// §6 and §8: pixel art must not be smoothed by the browser either.
	if !strings.Contains(body, "thumb-pixelated") {
		t.Error("a pixel-art thumbnail is not marked for pixelated rendering")
	}
}

// --- serving derivatives ----------------------------------------------------

func TestThumbRoute(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	ts.seedWithDerivatives(t, map[string][]byte{"pack/sprite.png": spritePNG(t, 64, 64)})

	id := ts.assetID(t, "pack/sprite.png")

	for _, variant := range []string{"", "?size=2x", "?size=alpha"} {
		resp := ts.get(t, fmt.Sprintf("/assets/%d/thumb%s", id, variant))
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET thumb%s = %d, want 200", variant, resp.StatusCode)
			continue
		}
		if got := resp.Header.Get("Content-Type"); got != "image/webp" {
			t.Errorf("thumb%s Content-Type = %q, want image/webp", variant, got)
		}
		// nosniff still applies, and the type is explicit — so a generated image is
		// safe to serve inline where a library file would not be.
		if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("thumb%s is missing nosniff", variant)
		}
		if len(ts.body(t, resp)) == 0 {
			t.Errorf("thumb%s is empty", variant)
		}
	}
}

// TestDerivativesAreServedInlineNotAsAttachments is the deliberate difference from the
// original-file route, and the reason preview.webp exists at all.
func TestDerivativesAreServedInlineNotAsAttachments(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	ts.seedWithDerivatives(t, map[string][]byte{"pack/sprite.png": spritePNG(t, 32, 32)})

	id := ts.assetID(t, "pack/sprite.png")

	preview := ts.get(t, fmt.Sprintf("/assets/%d/preview.webp", id))
	if preview.StatusCode != http.StatusOK {
		t.Fatalf("preview = %d, want 200", preview.StatusCode)
	}
	if cd := preview.Header.Get("Content-Disposition"); strings.HasPrefix(cd, "attachment") {
		t.Error("the preview is served as an attachment; the viewer needs it inline")
	}

	// The original, by contrast, is always an attachment (§11).
	original := ts.get(t, fmt.Sprintf("/assets/%d/download", id))
	if cd := original.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") {
		t.Error("the original is not an attachment")
	}
}

// TestMissingDerivativeExplainsWhy: a bare 404 tells the user nothing about whether the
// preview is queued, unsupported or broken. §12 wants failures visible.
func TestMissingDerivativeExplainsWhy(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)

	// A .xcf, which §6 says to mark unsupported gracefully.
	ts.seedWithDerivatives(t, map[string][]byte{
		"pack/art.xcf":    []byte("gimp file"),
		"pack/sprite.png": spritePNG(t, 16, 16),
	})

	id := ts.assetID(t, "pack/art.xcf")
	resp := ts.get(t, fmt.Sprintf("/assets/%d/thumb", id))
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
	body := ts.body(t, resp)
	if !strings.Contains(body, "No preview is available") {
		t.Errorf("body = %q, want an explanation", body)
	}
	// And the specific reason, not just a shrug.
	if !strings.Contains(strings.ToLower(body), "xcf") {
		t.Errorf("body = %q, want it to name the format", body)
	}
}

// TestDerivativeRouteRefusesATamperedHash: the path is built from the content hash, so a
// hand-edited hash must not become a traversal.
func TestDerivativeRouteRefusesATamperedHash(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	ts.seedWithDerivatives(t, map[string][]byte{"pack/sprite.png": spritePNG(t, 16, 16)})

	id := ts.assetID(t, "pack/sprite.png")

	// A secret outside the data root.
	secret := filepath.Join(filepath.Dir(ts.cfg.DataRoot), "secret.txt")
	if err := os.WriteFile(secret, []byte("SECRET CONTENTS"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, evil := range []string{
		"../../../secret.txt",
		"../..",
		"/etc/passwd",
		strings.Repeat("a", 64), // valid shape, no such directory
		"not-a-hash",
	} {
		if _, err := ts.db.Writer.Exec(`UPDATE assets SET sha256 = ? WHERE id = ?`, evil, id); err != nil {
			t.Fatal(err)
		}
		resp := ts.get(t, fmt.Sprintf("/assets/%d/thumb", id))
		body := ts.body(t, resp)

		if resp.StatusCode == http.StatusOK {
			t.Errorf("sha256 %q was served with 200", evil)
		}
		if strings.Contains(body, "SECRET CONTENTS") || strings.Contains(body, "root:") {
			t.Errorf("sha256 %q leaked a file outside the data root", evil)
		}
	}
}

// --- the detail page --------------------------------------------------------

func TestAssetDetailListsVariants(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	ts.seedWithDerivatives(t, map[string][]byte{
		"pack/PNG/hero.png": spritePNG(t, 32, 32),
		"pack/PSD/hero.psd": []byte("psd bytes"),
	})

	id := ts.assetID(t, "pack/PNG/hero.png")
	body := ts.body(t, ts.get(t, fmt.Sprintf("/assets/%d", id)))

	// §5.1: "the detail panel lists variants with download links per format".
	if !strings.Contains(body, "2 formats of this artwork") {
		t.Error("the detail page does not list the format variants")
	}
	if !strings.Contains(body, "PSD/hero.psd") {
		t.Error("the variant list does not include the PSD")
	}
	if !strings.Contains(body, "primary") {
		t.Error("the variant list does not mark the primary")
	}
	// The framing matters: these are formats of one artwork, not duplicates.
	if !strings.Contains(body, "One logical asset") {
		t.Error("the detail page does not explain that variants are not duplicates")
	}
}

func TestAssetDetailIncludesTheViewer(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	ts.seedWithDerivatives(t, map[string][]byte{"pack/sprite.png": spritePNG(t, 32, 32)})

	id := ts.assetID(t, "pack/sprite.png")
	body := ts.body(t, ts.get(t, fmt.Sprintf("/assets/%d", id)))

	// §8's viewer, configured through data attributes so the CSP needs no inline JS.
	for _, want := range []string{
		`id="viewer2d"`,
		`data-pixel-art="true"`,
		"/preview.webp",
		`data-zoom="fit"`, `data-zoom="8"`,
		`data-bg="checker"`, `data-bg="grey"`, `data-bg="black"`, `data-bg="white"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the viewer is missing %s", want)
		}
	}
	// No inline script anywhere, or the CSP would have to allow unsafe-inline.
	if strings.Contains(body, "<script>") {
		t.Error("the page contains an inline script")
	}
}

func TestAssetDetailShowsAnalysis(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	ts.seedWithDerivatives(t, map[string][]byte{"pack/sprite.png": spritePNG(t, 32, 32)})

	id := ts.assetID(t, "pack/sprite.png")
	body := ts.body(t, ts.get(t, fmt.Sprintf("/assets/%d", id)))

	if !strings.Contains(body, "pixel art") {
		t.Error("the detail page does not report pixel-art detection")
	}
	if !strings.Contains(body, "Colours") {
		t.Error("the detail page does not report the colour count")
	}
}

// --- the jobs page (§12) ----------------------------------------------------

func TestJobsPage(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	ts.seedWithDerivatives(t, map[string][]byte{"pack/sprite.png": spritePNG(t, 16, 16)})

	resp := ts.get(t, "/jobs")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := ts.body(t, resp)

	for _, want := range []string{"Background work", "asset.derive", "Scan the library"} {
		if !strings.Contains(body, want) {
			t.Errorf("the jobs page is missing %q", want)
		}
	}
}

func TestJobsPageRequiresLogin(t *testing.T) {
	ts := newTestServer(t)
	if resp := ts.get(t, "/jobs"); resp.StatusCode != http.StatusSeeOther {
		t.Errorf("GET /jobs anonymous = %d, want a redirect", resp.StatusCode)
	}
}

// TestJobsPageShowsFailures is §12's point: a silently failing pipeline is easy to miss
// for weeks, so the error text has to be on the page.
func TestJobsPageShowsFailures(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)

	// A truncated PNG: structurally a PNG, undecodable.
	valid := spritePNG(t, 32, 32)
	ts.seedWithDerivatives(t, map[string][]byte{
		"pack/broken.png": valid[:len(valid)/2],
		"pack/good.png":   valid,
	})

	body := ts.body(t, ts.get(t, "/jobs"))
	if !strings.Contains(body, "failed") {
		t.Error("the jobs page does not show the failure")
	}
	// The retry action §6 asks for.
	if !strings.Contains(body, "Retry everything that failed") {
		t.Error("the jobs page offers no retry action")
	}
}

// TestScanFromTheUIReturnsImmediately is invariant 8: no long-running HTTP handlers.
// The response must come back before the scan has finished.
func TestScanFromTheUIReturnsImmediately(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)

	// Enough files that a synchronous scan would be measurably slow.
	files := map[string][]byte{}
	for i := 0; i < 200; i++ {
		files[fmt.Sprintf("pack/sprite_%03d.png", i)] = spritePNG(t, 32, 32)
	}
	for relPath, content := range files {
		full := filepath.Join(ts.cfg.LibraryRoot, filepath.FromSlash(relPath))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	csrf := ts.csrfToken(t, "/jobs")
	resp, err := ts.client.PostForm(ts.URL+"/scan", url.Values{"csrf_token": {csrf}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("status = %d, want 303", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/jobs" {
		t.Errorf("Location = %q, want /jobs", got)
	}

	// The scan is queued, not done: nothing is indexed yet, because no worker is
	// running in this test.
	var assets int
	if err := ts.db.Reader.QueryRow(`SELECT count(*) FROM assets`).Scan(&assets); err != nil {
		t.Fatal(err)
	}
	if assets != 0 {
		t.Errorf("%d assets were indexed inside the HTTP handler; invariant 8 says the "+
			"handler must only enqueue", assets)
	}

	var queued int
	if err := ts.db.Reader.QueryRow(
		`SELECT count(*) FROM jobs WHERE type = 'library.scan' AND state = 'queued'`).
		Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued != 1 {
		t.Errorf("%d scan jobs queued, want 1", queued)
	}
}

func TestScanRequiresCSRF(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)

	resp, err := ts.client.PostForm(ts.URL+"/scan", url.Values{})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}

// TestDoubleScanRequestQueuesOnce: two people pressing the button must not walk the
// library twice.
func TestDoubleScanRequestQueuesOnce(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)

	for i := 0; i < 3; i++ {
		csrf := ts.csrfToken(t, "/jobs")
		resp, err := ts.client.PostForm(ts.URL+"/scan", url.Values{"csrf_token": {csrf}})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("request %d: status %d", i, resp.StatusCode)
		}
	}

	var queued int
	if err := ts.db.Reader.QueryRow(
		`SELECT count(*) FROM jobs WHERE type = 'library.scan'`).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued != 1 {
		t.Errorf("%d scan jobs after three requests, want 1", queued)
	}
}

func TestRetryFailedRequiresCSRF(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)

	resp, err := ts.client.PostForm(ts.URL+"/jobs/retry-failed", url.Values{})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}

// TestRetryFailedResetsBothHalves: resetting the asset rows alone would leave nothing
// scheduled, and requeueing the jobs alone would leave the assets marked failed.
func TestRetryFailedResetsBothHalves(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)

	valid := spritePNG(t, 32, 32)
	ts.seedWithDerivatives(t, map[string][]byte{"pack/broken.png": valid[:len(valid)/2]})

	var failedBefore int
	if err := ts.db.Reader.QueryRow(
		`SELECT count(*) FROM assets WHERE derive_state = 'failed'`).Scan(&failedBefore); err != nil {
		t.Fatal(err)
	}
	if failedBefore == 0 {
		t.Fatal("the broken image did not fail, so there is nothing to retry")
	}

	csrf := ts.csrfToken(t, "/jobs")
	resp, err := ts.client.PostForm(ts.URL+"/jobs/retry-failed", url.Values{"csrf_token": {csrf}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}

	var failedAfter, pendingAfter int
	if err := ts.db.Reader.QueryRow(`
		SELECT sum(derive_state = 'failed'), sum(derive_state = 'pending') FROM assets`).
		Scan(&failedAfter, &pendingAfter); err != nil {
		t.Fatal(err)
	}
	if failedAfter != 0 {
		t.Errorf("%d assets are still failed after a retry", failedAfter)
	}
	if pendingAfter == 0 {
		t.Error("no assets were reset to pending")
	}

	// And work is actually scheduled again.
	var queued int
	if err := ts.db.Reader.QueryRow(
		`SELECT count(*) FROM jobs WHERE state = 'queued'`).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued == 0 {
		t.Error("nothing was queued after the retry; the reset assets would never be picked up")
	}
}

// --- idempotence ------------------------------------------------------------

// TestSecondScanDoesNoDeriveWork is §6's "Idempotent, keyed on sha256 +
// derive_version, so rescans do no work."
func TestSecondScanDoesNoDeriveWork(t *testing.T) {
	ts := newTestServer(t)
	ts.seedWithDerivatives(t, map[string][]byte{
		"pack/a.png": spritePNG(t, 32, 32),
		"pack/b.png": spritePNG(t, 48, 48),
	})

	ctx := context.Background()
	queue := jobs.New(ts.db, jobs.Options{Workers: 1})

	enqueued, err := derive.EnqueueStale(ctx, ts.db, queue)
	if err != nil {
		t.Fatal(err)
	}
	if enqueued != 0 {
		t.Errorf("%d derive jobs enqueued on a second pass, want 0", enqueued)
	}
}

// TestBumpingDeriveVersionRegeneratesEverything is the other half of §4's rationale:
// "when the thumbnail algorithm improves, bump the version and only stale derivatives
// regenerate."
func TestBumpingDeriveVersionRegeneratesEverything(t *testing.T) {
	ts := newTestServer(t)
	ts.seedWithDerivatives(t, map[string][]byte{
		"pack/a.png": spritePNG(t, 32, 32),
		"pack/b.png": spritePNG(t, 48, 48),
	})

	// Simulate a version bump by ageing the stored version.
	if _, err := ts.db.Writer.Exec(
		`UPDATE assets SET derive_version = derive_version - 1`); err != nil {
		t.Fatal(err)
	}

	queue := jobs.New(ts.db, jobs.Options{Workers: 1})
	enqueued, err := derive.EnqueueStale(context.Background(), ts.db, queue)
	if err != nil {
		t.Fatal(err)
	}
	if enqueued != 2 {
		t.Errorf("%d derive jobs enqueued after a version bump, want 2", enqueued)
	}
}

// TestIdenticalFilesShareOneDerivative: derivatives are keyed on content, so two copies
// of the same bytes generate once.
func TestIdenticalFilesShareOneDerivative(t *testing.T) {
	ts := newTestServer(t)
	sprite := spritePNG(t, 32, 32)
	ts.seedWithDerivatives(t, map[string][]byte{
		"pack-a/sprite.png": sprite,
		"pack-b/sprite.png": sprite,
	})

	// Both assets are ok, and there is exactly one derivative directory.
	var ok int
	if err := ts.db.Reader.QueryRow(
		`SELECT count(*) FROM assets WHERE derive_state = 'ok'`).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	if ok != 2 {
		t.Errorf("%d assets derived ok, want 2", ok)
	}

	dirs := 0
	root := filepath.Join(ts.cfg.DataRoot, "derivatives")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, prefix := range entries {
		if !prefix.IsDir() {
			continue
		}
		inner, err := os.ReadDir(filepath.Join(root, prefix.Name()))
		if err != nil {
			t.Fatal(err)
		}
		dirs += len(inner)
	}
	if dirs != 1 {
		t.Errorf("%d derivative directories for two identical files, want 1", dirs)
	}
}
