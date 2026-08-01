package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/datcal/ambar/internal/archive"
	"github.com/datcal/ambar/internal/ingest"
	"github.com/datcal/ambar/internal/jobs"
	"github.com/datcal/ambar/internal/library"
	"github.com/datcal/ambar/internal/safepath"
)

// uploadExts are the archive types the web upload accepts.
var uploadExts = map[string]bool{".zip": true, ".rar": true, ".7z": true}

// The upload path, rebuilt in M16.
//
// What was here before was one form post straight into ingest, and it failed at the thing it
// is for. Dropping an itch.io pack into the browser:
//
//   - stopped at 100 MB and said "use _inbox instead" — so the documented way to add a pack
//     did not work for the packs people actually download;
//   - buffered the body through ParseMultipartForm, which writes anything over 8 MB to TMPDIR
//     and then copies it to _inbox: two writes of a 2 GB file on a NAS, and if /tmp is a
//     tmpfs, 2 GB of RAM;
//   - reported no progress at all, because htmx cannot report upload progress, so a
//     five-minute upload was a page doing nothing;
//   - always extracted at the library root, ignoring the 2d/3d/sounds folders the library is
//     organised into;
//   - asked for the source URL first, as a field you filled in before choosing a file.
//
// Now the part streams straight to its destination with MultipartReader (constant memory, one
// write, no cap worth having on a LAN), the response says what is inside the archive, and the
// browser asks where it should go — and afterwards, optionally, where it came from.

// uploadResponse is what the browser gets once the bytes have landed. The upload is not the
// ingest: the file is in _inbox and nothing has been extracted yet.
type uploadResponse struct {
	ArchiveRelPath string   `json:"archive_rel_path"`
	Filename       string   `json:"filename"`
	Bytes          int64    `json:"bytes"`
	FileCount      int      `json:"file_count"`
	Suggested      string   `json:"suggested"`
	SuggestReason  string   `json:"suggest_reason"`
	Folders        []string `json:"folders"`
}

// handleIngestForm renders the upload page (§5 web-upload path).
func (s *Server) handleIngestForm(w http.ResponseWriter, r *http.Request) {
	data := s.newPageData(r)
	data.Nav = "upload"
	data.Readonly = s.cfg.LibraryReadonly
	data.MaxUploadSize = s.cfg.MaxUploadSize
	data.Folders = s.libraryFolders()
	data.Flash = r.URL.Query().Get("msg")
	s.render(w, r, "ingest.html", http.StatusOK, data)
}

// handleUpload streams one archive into _inbox and reports what is inside it.
//
// It does not start the ingest. The browser gets a suggested destination and the folder list,
// asks, and posts to /ingest/start — which is how "where should this go" can be a question
// about a real archive instead of a guess made before uploading.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	if s.cfg.LibraryReadonly {
		http.Error(w, "the library is read-only; ingest is disabled", http.StatusForbidden)
		return
	}

	reader, err := r.MultipartReader()
	if err != nil {
		http.Error(w, "expected a multipart upload", http.StatusBadRequest)
		return
	}

	// MaxUploadSize <= 0 means no cap, which is the default on a LAN. A configured cap is
	// still honoured, but against the stream rather than a buffered body: the point is that
	// nothing is ever held whole.
	var limit int64
	if s.cfg.MaxUploadSize > 0 {
		limit = s.cfg.MaxUploadSize
	}

	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			http.Error(w, "could not read the upload", http.StatusBadRequest)
			return
		}
		if part.FormName() != "archive" {
			part.Close()
			continue
		}

		base := filepath.Base(part.FileName())
		if base == "" || base == "." || base == string(filepath.Separator) {
			part.Close()
			http.Error(w, "the upload has no filename", http.StatusBadRequest)
			return
		}
		if !uploadExts[strings.ToLower(filepath.Ext(base))] {
			part.Close()
			http.Error(w, "Only .zip, .rar and .7z archives can be uploaded.", http.StatusBadRequest)
			return
		}

		relPath, written, err := s.streamToInbox(base, part, limit)
		part.Close()
		if err != nil {
			var tooBig *uploadTooLarge
			if errors.As(err, &tooBig) {
				http.Error(w, tooBig.Error(), http.StatusRequestEntityTooLarge)
				return
			}
			s.log.ErrorContext(r.Context(), "saving upload failed", "error", err)
			http.Error(w, "could not save the upload", http.StatusInternalServerError)
			return
		}

		s.writeUploadResponse(w, r, relPath, base, written)
		return
	}

	http.Error(w, "no archive in the upload", http.StatusBadRequest)
}

// writeUploadResponse inspects the landed archive and answers with a destination suggestion.
func (s *Server) writeUploadResponse(w http.ResponseWriter, r *http.Request, relPath, base string, written int64) {
	out := uploadResponse{
		ArchiveRelPath: relPath,
		Filename:       base,
		Bytes:          written,
		Folders:        s.libraryFolders(),
	}

	// archive.Inspect lists the entries without extracting anything (§5's inspect step), so
	// the suggestion comes from what is in the file rather than from its name.
	if abs, err := safepath.ResolveExisting(s.cfg.LibraryRoot, relPath); err == nil {
		if info, err := archive.Inspect(abs); err == nil {
			out.FileCount = info.FileCount
			out.Suggested, out.SuggestReason = suggestDestination(info.Entries)
		} else {
			s.log.InfoContext(r.Context(), "could not inspect the upload", "error", err)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(out); err != nil {
		s.log.DebugContext(r.Context(), "client went away mid-response", "error", err)
	}
}

// handleIngestStart begins the ingest of an archive already sitting in _inbox.
func (s *Server) handleIngestStart(w http.ResponseWriter, r *http.Request) {
	if s.cfg.LibraryReadonly {
		http.Error(w, "the library is read-only; ingest is disabled", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	// The path came from our own upload response, but it arrives back through the client, so
	// it is resolved and confined before anything is enqueued (invariant 9).
	//
	// Resolve *then* confine, not the other way round. A prefix test on the raw string was
	// the first version of this and it was a real hole: "_inbox/../pack/hero.png" starts with
	// "_inbox/" and resolves to an indexed original, which ingest would have tried to read as
	// an archive and then *moved into _quarantine* — a library file relocated by the
	// application, which invariant 1 forbids outright.
	relPath, ok := s.inboxArchive(r.PostFormValue("archive_rel_path"))
	if !ok {
		http.Error(w, "unknown archive", http.StatusBadRequest)
		return
	}

	dest := strings.TrimSpace(r.PostFormValue("dest"))
	if newFolder := strings.TrimSpace(r.PostFormValue("new_folder")); newFolder != "" {
		dest = newFolder
	}

	id, err := ingest.Enqueue(r.Context(), s.jobs, ingest.Payload{
		ArchiveRelPath: relPath,
		SourceURL:      strings.TrimSpace(r.PostFormValue("source")),
		DestDir:        dest,
	})
	switch {
	case errors.Is(err, jobs.ErrDuplicate):
		// Already queued for this archive — a double click, or the inbox poller having seen
		// the same file first. Not an error: the work the user asked for is going to happen.
		s.log.InfoContext(r.Context(), "ingest already queued", "archive", relPath)
	case err != nil:
		s.log.ErrorContext(r.Context(), "enqueue ingest failed", "error", err)
		http.Error(w, "could not queue the archive for ingest", http.StatusInternalServerError)
		return
	}
	s.log.InfoContext(r.Context(), "ingest enqueued", "job_id", id, "archive", relPath, "dest", dest)

	// The asset counts move when the job finishes, but the job counter moves now.
	s.nav.invalidate()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"job_id": id, "dest": dest})
}

// inboxArchive resolves a client-supplied path and returns it only if it is an existing file
// directly inside _inbox.
//
// Returns the *re-derived* relative path rather than the one it was given, so what reaches the
// job payload is normalised: whatever the client sent, what gets enqueued is a path this
// server computed from a resolved absolute one.
func (s *Server) inboxArchive(raw string) (string, bool) {
	candidate := strings.TrimSpace(raw)
	if candidate == "" {
		return "", false
	}

	abs, err := safepath.ResolveExisting(s.cfg.LibraryRoot, candidate)
	if err != nil {
		return "", false
	}
	inboxAbs, err := safepath.Resolve(s.cfg.LibraryRoot, ingest.InboxDir)
	if err != nil {
		return "", false
	}

	// Inside the inbox, and directly inside it: a subdirectory of _inbox is not something the
	// upload produces, so accepting one would only widen what a crafted form can reach.
	within, err := safepath.RelUnder(inboxAbs, abs)
	if err != nil || within == "" || strings.Contains(within, "/") {
		return "", false
	}

	info, err := os.Lstat(abs)
	if err != nil || !info.Mode().IsRegular() {
		return "", false
	}

	rel, err := safepath.RelUnder(s.cfg.LibraryRoot, abs)
	if err != nil {
		return "", false
	}
	return rel, true
}

// uploadTooLarge is returned when a configured cap is exceeded mid-stream.
type uploadTooLarge struct{ limit int64 }

func (e *uploadTooLarge) Error() string {
	return fmt.Sprintf("That archive is over the %s upload limit (AMBAR_MAX_UPLOAD_SIZE). "+
		"Raise it, or copy the file into the library's _inbox/ folder.", FormatBytes(e.limit))
}

// streamToInbox writes the part straight to its destination under _inbox.
//
// One pass, constant memory, and the partial file is removed on any failure — a cancelled
// upload must not leave something for the inbox poller to find and half-ingest.
func (s *Server) streamToInbox(base string, src io.Reader, limit int64) (relPath string, written int64, err error) {
	inboxAbs, err := safepath.Resolve(s.cfg.LibraryRoot, ingest.InboxDir)
	if err != nil {
		return "", 0, err
	}
	if err := os.MkdirAll(inboxAbs, 0o755); err != nil {
		return "", 0, fmt.Errorf("create _inbox: %w", err)
	}

	// Resolved through safepath even though the name is already a Base: a crafted multipart
	// filename must not be able to escape the inbox (invariant 9).
	destAbs, err := safepath.Resolve(s.cfg.LibraryRoot, ingest.InboxDir+"/"+base)
	if err != nil {
		return "", 0, err
	}
	destAbs = uniqueFilePath(destAbs)

	out, err := os.OpenFile(destAbs, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return "", 0, err
	}

	cleanup := func() {
		out.Close()
		os.Remove(destAbs)
	}

	reader := src
	if limit > 0 {
		// One byte over the limit is all it takes to know.
		reader = io.LimitReader(src, limit+1)
	}

	written, err = io.Copy(out, reader)
	if err != nil {
		cleanup()
		return "", 0, err
	}
	if limit > 0 && written > limit {
		cleanup()
		return "", 0, &uploadTooLarge{limit: limit}
	}
	if err := out.Close(); err != nil {
		os.Remove(destAbs)
		return "", 0, err
	}

	rel, err := safepath.RelUnder(s.cfg.LibraryRoot, destAbs)
	if err != nil {
		os.Remove(destAbs)
		return "", 0, err
	}
	return rel, written, nil
}

// libraryFolders lists the top-level folders a pack can be filed into.
//
// Read from the filesystem rather than from the index, because a folder made over SMB should
// be offerable before anything in it has been scanned. Reserved (underscore-prefixed)
// directories are pipeline space and never destinations.
func (s *Server) libraryFolders() []string {
	entries, err := os.ReadDir(s.cfg.LibraryRoot)
	if err != nil {
		s.log.Error("listing library folders failed", "error", err)
		return nil
	}
	var out []string
	for _, entry := range entries {
		if !entry.IsDir() || library.IsReserved(entry.Name()) {
			continue
		}
		out = append(out, entry.Name())
	}
	sort.Strings(out)
	return out
}

// suggestDestination guesses which folder an archive belongs in from what is inside it.
//
// A suggestion, never a decision: it arrives preselected in a picker the human can change,
// which is the whole reason the upload answers before it extracts. A genuinely mixed archive
// gets no suggestion rather than a confident wrong one.
func suggestDestination(entries []archive.Entry) (folder, reason string) {
	// Kind → the folder that kind lives in, named after the layout in §17. Anything not
	// listed here does not vote.
	folderForKind := map[library.Kind]string{
		library.KindImage:    "2d",
		library.KindTilemap:  "2d",
		library.KindTexture:  "2d",
		library.KindModel:    "3d",
		library.KindMaterial: "3d",
		library.KindRig:      "3d",
		library.KindAudio:    "sounds",
		library.KindFont:     "fonts",
	}

	votes := map[string]int{}
	total := 0
	for _, entry := range entries {
		if entry.IsDir {
			continue
		}
		ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(entry.Name)), ".")
		if ext == "" {
			continue
		}
		// An .aseprite is a 2D source file, and this library keeps those together.
		if ext == "aseprite" || ext == "ase" {
			votes["aseprite"]++
			total++
			continue
		}
		if dir, ok := folderForKind[library.KindForExt(ext)]; ok {
			votes[dir]++
			total++
		}
	}
	if total == 0 {
		return "", ""
	}

	best, bestVotes := "", 0
	for dir, n := range votes {
		if n > bestVotes || (n == bestVotes && (best == "" || dir < best)) {
			best, bestVotes = dir, n
		}
	}

	// Two thirds is the bar for "this archive is mostly one thing". Below it the archive is
	// genuinely mixed, and guessing would file half of it in the wrong place.
	share := float64(bestVotes) / float64(total)
	if share < 0.66 {
		return "", "mixed contents"
	}
	return best, fmt.Sprintf("%d%% %s", int(share*100), best)
}

// uniqueFilePath appends -1, -2, ... before the extension until the path is free.
func uniqueFilePath(path string) string {
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return path
	}
	ext := filepath.Ext(path)
	stem := strings.TrimSuffix(path, ext)
	for i := 1; i < 10000; i++ {
		candidate := fmt.Sprintf("%s-%d%s", stem, i, ext)
		if _, err := os.Lstat(candidate); os.IsNotExist(err) {
			return candidate
		}
	}
	return path
}
