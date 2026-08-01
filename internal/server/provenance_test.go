package server

import (
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProvenanceCaptureFlow(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	// Seeded through a real scan rather than a bare INSERT: the grid lists asset *groups*, and
	// only a scan creates those, so a directly inserted row would never appear in the search
	// this test now uses.
	ts.seedLibrary(t, map[string]string{"pack/sprite.png": "art"})

	var packID int64
	if err := ts.db.Reader.QueryRow(`SELECT id FROM packs WHERE library_rel_path = 'pack'`).Scan(&packID); err != nil {
		t.Fatal(err)
	}

	// The pack starts in the backlog, which since M16 is a search rather than a page: the grid
	// lists the affected *assets*, each fixable on its own detail page.
	body := readBody(t, ts.get(t, "/?q=-has%3Aprovenance"))
	if !strings.Contains(body, "sprite.png") {
		t.Errorf("an asset with no provenance is missing from -has:provenance")
	}

	// Capture provenance with a licence and mark it complete.
	status, _ := ts.postForm(t, itoa("/packs/%d/provenance", packID), url.Values{
		"source_url":    {"https://kenney.itch.io/sci-fi"},
		"source_author": {"Kenney"},
		"license":       {"CC0-1.0"},
		"complete":      {"on"},
		"view":          {"needs"},
	})
	if status != 200 && status != 303 {
		t.Fatalf("save status = %d", status)
	}

	// The database reflects it.
	var state, author string
	var licenseID *int64
	if err := ts.db.Reader.QueryRow(
		`SELECT provenance_state, source_author, license_id FROM packs WHERE id = ?`, packID).
		Scan(&state, &author, &licenseID); err != nil {
		t.Fatal(err)
	}
	if state != "complete" || author != "Kenney" || licenseID == nil {
		t.Errorf("provenance not saved: state=%q author=%q license=%v", state, author, licenseID)
	}

	// The sidecar was written beside the (would-be) originals (§3).
	if _, err := os.Stat(filepath.Join(ts.cfg.LibraryRoot, "pack", ".ambar.json")); err != nil {
		t.Errorf("sidecar not written on provenance save: %v", err)
	}

	// It has left the needs-provenance backlog.
	body = readBody(t, ts.get(t, "/provenance?view=needs"))
	if strings.Contains(body, ">pack<") {
		t.Errorf("pack still shown as needing provenance after completion")
	}
}

// TestProvenanceBulkSetLicense went with the /provenance page in M16. What replaced it is
// narrower on purpose: a licence applies to a pack, and a form that set one on twenty packs at
// once was a fast way to record something wrong about nineteen of them.

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestAssetProvenanceSaveIsPartial is the guard on the M16 asset-page form.
//
// The full capture form writes the entire record, so the obvious implementation — post two
// fields at the existing pack endpoint — would blank the author, the price, the order
// reference and the notes of any pack that had them. That is silent data loss on a page
// whose whole purpose is recording provenance, so it gets a test rather than a comment.
func TestAssetProvenanceSaveIsPartial(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	ts.seedLibrary(t, map[string]string{"pack/hero.png": "art"})

	id := ts.assetID(t, "pack/hero.png")
	var packID int64
	if err := ts.db.Reader.QueryRow(`SELECT pack_id FROM assets WHERE id = ?`, id).Scan(&packID); err != nil {
		t.Fatal(err)
	}

	// Something in the fields the asset page does not show.
	if _, err := ts.db.Writer.Exec(`
		UPDATE packs SET source_author = ?, notes = ?, order_ref = ? WHERE id = ?`,
		"Kenney", "bought in the 2026 bundle", "INV-42", packID); err != nil {
		t.Fatal(err)
	}

	ts.postForm(t, itoa("/assets/%d/provenance", id), url.Values{
		"license":    {"CC0-1.0"},
		"source_url": {"https://kenney.itch.io/example"},
	})

	var author, notes, orderRef, sourceURL, state string
	var licenseID *int64
	if err := ts.db.Reader.QueryRow(`
		SELECT source_author, notes, order_ref, source_url, provenance_state, license_id
		FROM packs WHERE id = ?`, packID,
	).Scan(&author, &notes, &orderRef, &sourceURL, &state, &licenseID); err != nil {
		t.Fatal(err)
	}

	if sourceURL != "https://kenney.itch.io/example" {
		t.Errorf("source_url = %q, want the posted one", sourceURL)
	}
	if licenseID == nil {
		t.Error("the licence was not recorded")
	}
	// Both halves known, so the pack leaves the backlog.
	if state != "complete" {
		t.Errorf("provenance_state = %q, want complete", state)
	}
	// And nothing else moved.
	if author != "Kenney" || notes != "bought in the 2026 bundle" || orderRef != "INV-42" {
		t.Errorf("the form overwrote fields it does not show: author=%q notes=%q order=%q",
			author, notes, orderRef)
	}
}

// A licence with no link is still an unanswered question, so the pack stays on the backlog.
func TestAssetProvenanceIncompleteStaysOnTheBacklog(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	ts.seedLibrary(t, map[string]string{"pack/hero.png": "art"})

	id := ts.assetID(t, "pack/hero.png")
	ts.postForm(t, itoa("/assets/%d/provenance", id), url.Values{"license": {"CC0-1.0"}})

	var state string
	if err := ts.db.Reader.QueryRow(`
		SELECT p.provenance_state FROM packs p JOIN assets a ON a.pack_id = p.id WHERE a.id = ?`,
		id).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "needs_provenance" {
		t.Errorf("provenance_state = %q, want needs_provenance", state)
	}
}
