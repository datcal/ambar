package server

import (
	"archive/zip"
	"bytes"
	"mime/multipart"
	"net/http"
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

// uploadArchive posts a multipart upload the way the htmx form does: the CSRF
// token in the header, not a body field.
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

func TestUploadLandsInInboxAndEnqueues(t *testing.T) {
	ts := uploadTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)

	resp := ts.uploadArchive(t, "robot.zip", zipBytes(t), "https://itch.io/robot")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 with HX-Redirect", resp.StatusCode)
	}
	if loc := resp.Header.Get("HX-Redirect"); loc == "" {
		t.Errorf("no HX-Redirect header")
	}

	// The archive is in _inbox, awaiting the (unstarted) ingest job.
	if _, err := os.Stat(filepath.Join(ts.cfg.LibraryRoot, "_inbox", "robot.zip")); err != nil {
		t.Errorf("upload not in _inbox: %v", err)
	}
	// An ingest job was enqueued carrying the source URL.
	var payload string
	err := ts.db.Reader.QueryRow(
		`SELECT payload_json FROM jobs WHERE type = 'ingest.archive'`).Scan(&payload)
	if err != nil {
		t.Fatalf("no ingest job enqueued: %v", err)
	}
	if !bytesContains(payload, "itch.io/robot") {
		t.Errorf("source URL missing from job: %s", payload)
	}
}

func TestUploadRejectsNonArchive(t *testing.T) {
	ts := uploadTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)

	resp := ts.uploadArchive(t, "notes.txt", []byte("hello"), "")
	loc := resp.Header.Get("HX-Redirect")
	if loc == "" || !bytesContains(loc, "ingest") {
		t.Errorf("expected redirect back to /ingest, got %q", loc)
	}
	if _, err := os.Stat(filepath.Join(ts.cfg.LibraryRoot, "_inbox", "notes.txt")); !os.IsNotExist(err) {
		t.Errorf("a non-archive was saved")
	}
}

func TestUploadTooLargeRejected(t *testing.T) {
	ts := newTestServerWithConfig(t, func(c *config.Config) { c.MaxUploadSize = 10 })
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)

	big := bytes.Repeat([]byte("x"), 8192) // well over the 10-byte limit + slack
	resp := ts.uploadArchive(t, "big.zip", big, "")
	loc := resp.Header.Get("HX-Redirect")
	if loc == "" || !bytesContains(loc, "_inbox") {
		t.Errorf("over-limit upload should redirect with the _inbox hint, got %q", loc)
	}
}

func bytesContains(haystack, needle string) bool {
	return bytes.Contains([]byte(haystack), []byte(needle))
}
