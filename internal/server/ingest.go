package server

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/datcal/ambar/internal/ingest"
	"github.com/datcal/ambar/internal/safepath"
)

// uploadExts are the archive types the web upload accepts. §5 keeps web upload to
// "small files"; large bundles go through _inbox over SMB.
var uploadExts = map[string]bool{".zip": true, ".rar": true, ".7z": true}

// handleIngestForm renders the upload page (§5 web-upload path).
func (s *Server) handleIngestForm(w http.ResponseWriter, r *http.Request) {
	data := s.newPageData(r)
	data.Readonly = s.cfg.LibraryReadonly
	data.MaxUploadSize = s.cfg.MaxUploadSize
	data.Flash = r.URL.Query().Get("msg")
	s.render(w, r, "ingest.html", http.StatusOK, data)
}

// handleUpload accepts one archive, drops it into _inbox and enqueues ingest.
//
// §5: "Plain multipart, for small files, with a configurable size cap ... If
// someone tries a 2 GB archive through Cloudflare it fails; the error message
// should say to use _inbox." So the body is hard-capped and the too-large case
// says exactly that.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	if s.cfg.LibraryReadonly {
		http.Error(w, "the library is read-only; ingest is disabled", http.StatusForbidden)
		return
	}

	// Cap the whole request body. The +4 KiB leaves room for the multipart framing
	// around a file that is itself right at the limit.
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxUploadSize+4096)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			s.redirectWithMessage(w, r, "/ingest", fmt.Sprintf(
				"That file is over the %s upload limit. Drop large archives into the library's _inbox/ folder instead.",
				FormatBytes(s.cfg.MaxUploadSize)))
			return
		}
		http.Error(w, "could not read the upload", http.StatusBadRequest)
		return
	}

	file, header, err := r.FormFile("archive")
	if err != nil {
		s.redirectWithMessage(w, r, "/ingest", "Choose an archive to upload.")
		return
	}
	defer file.Close()

	base := filepath.Base(header.Filename)
	if !uploadExts[strings.ToLower(filepath.Ext(base))] {
		s.redirectWithMessage(w, r, "/ingest", "Only .zip, .rar and .7z archives can be uploaded.")
		return
	}

	relPath, err := s.saveToInbox(base, file)
	if err != nil {
		s.log.ErrorContext(r.Context(), "saving upload failed", "error", err)
		http.Error(w, "could not save the upload", http.StatusInternalServerError)
		return
	}

	source := strings.TrimSpace(r.FormValue("source"))
	if _, err := ingest.Enqueue(r.Context(), s.jobs, ingest.Payload{ArchiveRelPath: relPath, SourceURL: source}); err != nil {
		s.log.ErrorContext(r.Context(), "enqueue ingest failed", "error", err)
		http.Error(w, "could not queue the archive for ingest", http.StatusInternalServerError)
		return
	}
	s.redirectWithMessage(w, r, "/jobs", fmt.Sprintf("Uploaded %s. It is being ingested.", base))
}

// saveToInbox writes the uploaded archive under _inbox, through safepath, with a
// unique name so two uploads of the same filename do not collide. It returns the
// library-relative path for the ingest payload.
func (s *Server) saveToInbox(base string, src io.Reader) (string, error) {
	inboxAbs, err := safepath.Resolve(s.cfg.LibraryRoot, ingest.InboxDir)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(inboxAbs, 0o755); err != nil {
		return "", fmt.Errorf("create _inbox: %w", err)
	}

	// Resolve the final destination through safepath too, so a crafted multipart
	// filename cannot escape _inbox even though we already took its Base.
	destAbs, err := safepath.Resolve(s.cfg.LibraryRoot, ingest.InboxDir+"/"+base)
	if err != nil {
		return "", err
	}
	destAbs = uniqueFilePath(destAbs)

	out, err := os.OpenFile(destAbs, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(out, src); err != nil {
		out.Close()
		os.Remove(destAbs)
		return "", err
	}
	if err := out.Close(); err != nil {
		return "", err
	}

	rel, err := safepath.RelUnder(s.cfg.LibraryRoot, destAbs)
	if err != nil {
		return "", err
	}
	return rel, nil
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
