package server

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/datcal/ambar/internal/derive"
)

// insertAudioAsset inserts an audio asset, writes its original into the library
// and a peaks.json into the derivatives, returning the asset id.
func (ts *testServer) insertAudioAsset(t *testing.T, filename string) int64 {
	t.Helper()
	now := time.Now().Unix()
	sha := strings.Repeat("a", 64)

	if _, err := ts.db.Writer.Exec(`
		INSERT INTO packs (name, slug, kind, library_rel_path, first_seen_at, last_seen_at, created_at, updated_at)
		VALUES ('sfx', 'sfx', 'folder', 'sfx', ?, ?, ?, ?)
		ON CONFLICT(library_rel_path) DO NOTHING`, now, now, now, now); err != nil {
		t.Fatal(err)
	}
	var packID int64
	ts.db.Reader.QueryRow(`SELECT id FROM packs WHERE library_rel_path = 'sfx'`).Scan(&packID)

	res, err := ts.db.Writer.Exec(`
		INSERT INTO assets (pack_id, rel_path, filename, ext, kind, size, mtime, sha256,
		                    duration_ms, sample_rate, channels, bit_depth, peak_dbfs, is_loopable,
		                    derive_state, derive_version,
		                    first_seen_at, last_verified_at, created_at, updated_at)
		VALUES (?, ?, ?, 'wav', 'audio', 4, ?, ?, 1000, 44100, 1, 16, -6.0, 1, 'ok', ?, ?, ?, ?, ?)`,
		packID, filename, filename, now, sha, derive.Version, now, now, now, now)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	ts.db.Writer.Exec(`INSERT INTO assets_fts (rowid, filename, pack_name, tag_text, notes) VALUES (?, ?, 'sfx', '', '')`, id, filename)

	// The grid lists groups, so the asset needs one to appear there.
	g, err := ts.db.Writer.Exec(`
		INSERT INTO asset_groups (pack_id, group_key, primary_asset_id, variant_count, created_at, updated_at)
		VALUES (?, ?, ?, 1, ?, ?)`, packID, filename, id, now, now)
	if err != nil {
		t.Fatal(err)
	}
	gid, _ := g.LastInsertId()
	ts.db.Writer.Exec(`UPDATE assets SET group_id = ? WHERE id = ?`, gid, id)

	// Original file in the library.
	dir := filepath.Join(ts.cfg.LibraryRoot, "sfx")
	os.MkdirAll(dir, 0o755)
	if err := os.WriteFile(filepath.Join(dir, filename), []byte("RIFFwavedata"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Peaks derivative.
	rel, _ := derive.Dir(sha)
	derivDir := filepath.Join(ts.cfg.DataRoot, rel)
	os.MkdirAll(derivDir, 0o755)
	if err := os.WriteFile(filepath.Join(derivDir, derive.FilePeaks),
		[]byte(`{"version":1,"count":2,"min":[-0.5,-0.3],"max":[0.5,0.3]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestAudioDetailAndServing(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	id := ts.insertAudioAsset(t, "loop.wav")

	// The detail page renders the audio viewer, not the 2D image viewer.
	detail := readBody(t, ts.get(t, itoa("/assets/%d", id)))
	if !strings.Contains(detail, `id="audio-viewer"`) || !strings.Contains(detail, "waveform") {
		t.Errorf("audio viewer missing from detail page")
	}
	if strings.Contains(detail, `id="viewer2d"`) {
		t.Errorf("the 2D image viewer should not render for audio")
	}

	// peaks.json serves as JSON.
	peaks := ts.get(t, itoa("/assets/%d/peaks.json", id))
	if ct := peaks.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("peaks content-type = %q", ct)
	}
	if !strings.Contains(readBody(t, peaks), `"max"`) {
		t.Errorf("peaks body missing")
	}

	// The original streams inline with an audio type and supports Range.
	audio := ts.get(t, itoa("/assets/%d/audio", id))
	if ct := audio.Header.Get("Content-Type"); ct != "audio/wav" {
		t.Errorf("audio content-type = %q, want audio/wav", ct)
	}
	if cd := audio.Header.Get("Content-Disposition"); strings.Contains(cd, "attachment") {
		t.Errorf("audio must be inline, not an attachment")
	}
	audio.Body.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+itoa("/assets/%d/audio", id), nil)
	req.Header.Set("Range", "bytes=0-3")
	resp, err := ts.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		t.Errorf("Range request status = %d, want 206", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if len(body) != 4 {
		t.Errorf("Range returned %d bytes, want 4", len(body))
	}
}

// TestAudioTileMarkup is what is left of TestAuditionGridMarkup.
//
// M16 removed §8's keyboard audition along with the sidebar's "Tools" section, which was
// its only entry point — see docs/spec.md. The per-tile audio source is not part of
// that feature: it is how any future preview-on-hover or player finds the file, so it is
// still asserted here.
func TestAudioTileMarkup(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	id := ts.insertAudioAsset(t, "hit.wav")

	body := readBody(t, ts.get(t, "/assets"))
	if !strings.Contains(body, itoa(`data-audio="/assets/%d/audio"`, id)) {
		t.Errorf("audio tile not marked with its data-audio source")
	}
	if strings.Contains(body, `id="audition"`) {
		t.Errorf("the audition bar is back; it was removed with the Tools section")
	}
}
