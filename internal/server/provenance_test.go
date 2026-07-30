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
	ts.insertAsset(t, "sprite.png") // creates pack "pack" at rel "pack"

	var packID int64
	if err := ts.db.Reader.QueryRow(`SELECT id FROM packs WHERE library_rel_path = 'pack'`).Scan(&packID); err != nil {
		t.Fatal(err)
	}

	// The pack starts in the needs-provenance backlog.
	body := readBody(t, ts.get(t, "/provenance?view=needs"))
	if !strings.Contains(body, "pack") {
		t.Errorf("pack not listed under needs-provenance")
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

func TestProvenanceBulkSetLicense(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	ts.insertAsset(t, "a.png")

	var packID int64
	ts.db.Reader.QueryRow(`SELECT id FROM packs WHERE library_rel_path = 'pack'`).Scan(&packID)

	status, _ := ts.postForm(t, "/provenance/bulk", url.Values{
		"license": {"CC-BY-4.0"},
		"view":    {"risk"},
		"id":      {itoa("%d", packID)},
	})
	if status != 200 && status != 303 {
		t.Fatalf("bulk status = %d", status)
	}
	var spdx string
	ts.db.Reader.QueryRow(`SELECT l.spdx_id FROM packs p JOIN licenses l ON l.id = p.license_id WHERE p.id = ?`, packID).Scan(&spdx)
	if spdx != "CC-BY-4.0" {
		t.Errorf("bulk licence not applied: %q", spdx)
	}
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
