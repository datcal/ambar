package server

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/datcal/ambar/internal/config"
)

func zipBytes(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("pack/a.png")
	w.Write([]byte("png"))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// uploadArchive posts a multipart upload the way upload.js does: the CSRF token in the
// header, not a body field, and no other fields — M16 split the flow, so the upload carries
// only the bytes and the destination is chosen afterwards.
func (ts *testServer) uploadArchive(t *testing.T, filename string, content []byte, source string) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("archive", filename)
	if err != nil {
		t.Fatal(err)
	}
	fw.Write(content)
	if source != "" {
		mw.WriteField("source", source)
	}
	mw.Close()

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/ingest/upload", &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-CSRF-Token", ts.csrfToken(t, "/"))
	req.Header.Set("HX-Request", "true")
	resp, err := ts.client.Do(req)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func uploadTestServer(t *testing.T) *testServer {
	return newTestServerWithConfig(t, func(c *config.Config) {
		c.MaxUploadSize = 10 << 20
	})
}

// The M16 upload is two steps: the bytes land in _inbox and nothing is extracted until a
// destination has been chosen. These tests follow that split.
func TestUploadLandsInInboxAndSuggestsADestination(t *testing.T) {
	ts := uploadTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)

	resp := ts.uploadArchive(t, "robot.zip", zipBytes(t), "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var got uploadResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode upload response: %v", err)
	}
	if got.ArchiveRelPath != "_inbox/robot.zip" {
		t.Errorf("ArchiveRelPath = %q", got.ArchiveRelPath)
	}
	if got.FileCount != 1 {
		t.Errorf("FileCount = %d, want 1 — the archive was not inspected", got.FileCount)
	}
	// The zip holds one .png, so the suggestion is the 2D folder. A suggestion, not a
	// decision: the picker preselects it and the human can change it.
	if got.Suggested != "2d" {
		t.Errorf("Suggested = %q, want 2d (reason %q)", got.Suggested, got.SuggestReason)
	}

	if _, err := os.Stat(filepath.Join(ts.cfg.LibraryRoot, "_inbox", "robot.zip")); err != nil {
		t.Errorf("upload not in _inbox: %v", err)
	}

	// Nothing is enqueued yet. This is the half that used to be automatic, and it is why the
	// destination can be a question about the real archive.
	var jobs int
	if err := ts.db.Reader.QueryRow(
		`SELECT count(*) FROM jobs WHERE type = 'ingest.archive'`).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if jobs != 0 {
		t.Errorf("%d ingest jobs enqueued by the upload alone", jobs)
	}
}

func TestIngestStartCarriesDestinationAndSource(t *testing.T) {
	ts := uploadTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)

	resp := ts.uploadArchive(t, "robot.zip", zipBytes(t), "")
	var uploaded uploadResponse
	if err := json.NewDecoder(resp.Body).Decode(&uploaded); err != nil {
		t.Fatal(err)
	}

	ts.postForm(t, "/ingest/start", url.Values{
		"archive_rel_path": {uploaded.ArchiveRelPath},
		"dest":             {"2d"},
		"source":           {"https://itch.io/robot"},
	})

	var payload string
	if err := ts.db.Reader.QueryRow(
		`SELECT payload_json FROM jobs WHERE type = 'ingest.archive'`).Scan(&payload); err != nil {
		t.Fatalf("no ingest job enqueued: %v", err)
	}
	for _, want := range []string{"itch.io/robot", `"dest_dir":"2d"`, "_inbox/robot.zip"} {
		if !bytesContains(payload, want) {
			t.Errorf("job payload is missing %q: %s", want, payload)
		}
	}
}

// The path comes back through the client, so it is checked before anything is enqueued
// (invariant 9). Nothing outside _inbox may be ingested however the form is edited.
func TestIngestStartRefusesPathsOutsideTheInbox(t *testing.T) {
	ts := uploadTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	ts.seedLibrary(t, map[string]string{"pack/hero.png": "h"})

	for _, bad := range []string{"pack/hero.png", "../etc/passwd", "_inbox/../pack/hero.png", ""} {
		code, _ := ts.postForm(t, "/ingest/start", url.Values{
			"archive_rel_path": {bad},
			"dest":             {"2d"},
		})
		if code < 400 {
			t.Errorf("archive_rel_path %q was accepted with status %d", bad, code)
		}
	}

	var jobs int
	if err := ts.db.Reader.QueryRow(
		`SELECT count(*) FROM jobs WHERE type = 'ingest.archive'`).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if jobs != 0 {
		t.Errorf("%d ingest jobs enqueued from a rejected path", jobs)
	}
}

func TestUploadRejectsNonArchive(t *testing.T) {
	ts := uploadTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)

	resp := ts.uploadArchive(t, "notes.txt", []byte("hello"), "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(ts.cfg.LibraryRoot, "_inbox", "notes.txt")); !os.IsNotExist(err) {
		t.Errorf("a non-archive was saved")
	}
}

// A configured cap is still honoured, and the partial file must not be left behind for the
// inbox poller to find.
func TestUploadOverConfiguredCapIsRejected(t *testing.T) {
	ts := newTestServerWithConfig(t, func(c *config.Config) { c.MaxUploadSize = 10 })
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)

	big := bytes.Repeat([]byte("x"), 8192)
	resp := ts.uploadArchive(t, "big.zip", big, "")
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", resp.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(ts.cfg.LibraryRoot, "_inbox", "big.zip")); !os.IsNotExist(err) {
		t.Error("the over-limit upload left a partial file in _inbox")
	}
}

// The default is no cap (M16): a 2 GB itch.io pack is the case the upload exists for, and it
// streams rather than buffering, so there is nothing to protect against on a LAN.
func TestUploadWithNoCapAcceptsSomethingOverTheOldLimit(t *testing.T) {
	ts := newTestServerWithConfig(t, func(c *config.Config) { c.MaxUploadSize = 0 })
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)

	// Not 100 MB in a unit test; enough to prove no limit is applied.
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("pack/big.png")
	w.Write(bytes.Repeat([]byte("p"), 512<<10))
	zw.Close()

	resp := ts.uploadArchive(t, "big.zip", buf.Bytes(), "")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 with no cap configured", resp.StatusCode)
	}
}

func bytesContains(haystack, needle string) bool {
	return bytes.Contains([]byte(haystack), []byte(needle))
}
