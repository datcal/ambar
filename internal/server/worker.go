package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"github.com/datcal/ambar/internal/derive"
	"github.com/datcal/ambar/internal/remote"
)

// The remote derive worker API (internal/remote). Write-scoped tokens only: a worker
// records derivatives and moves jobs through the queue, which is a write by any reading.
//
// None of these runs derive work. A claim is a few queue updates, a result is an upload
// and the same recording a local worker does, a release is a requeue — so ARCHITECTURE
// rule 8, no long-running handlers, holds: the long part happens on the worker.

// maxResultUpload bounds one result request: the largest derivative a worker may send
// (a converted model's preview.glb) plus room for the rest.
const maxResultUpload = 1<<30 + 64<<20

// EnableRemoteWorkers attaches the coordinator that answers the worker API. Without it
// every worker request is refused; with it, whether a claim is answered is the jobs page's
// choice, read on each request.
func (s *Server) EnableRemoteWorkers(c *remote.Coordinator) {
	s.remote = c
}

// remoteAvailable gates the worker API. Only a *claim* depends on the current choice
// (Coordinator.Claim refuses in ModeOff): a result or a release for a job already handed
// out is accepted whatever the page says now, so switching to "this server" does not
// throw away work a worker is halfway through.
func (s *Server) remoteAvailable(w http.ResponseWriter) bool {
	if s.remote == nil {
		s.apiError(w, http.StatusServiceUnavailable, remote.ErrDisabled.Error())
		return false
	}
	if s.cfg.LibraryReadonly {
		// Derivatives live under the data root, but a read-only deployment means
		// "change nothing", as the thumbnail upload already honours.
		s.apiError(w, http.StatusServiceUnavailable, "this instance is read-only")
		return false
	}
	return true
}

func (s *Server) handleWorkerClaim(w http.ResponseWriter, r *http.Request) {
	if !s.remoteAvailable(w) {
		return
	}
	var req remote.ClaimRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		s.apiError(w, http.StatusBadRequest, "invalid claim request")
		return
	}

	claim, err := s.remote.Claim(r.Context(), req)
	switch {
	case errors.Is(err, remote.ErrVersionMismatch):
		s.apiError(w, http.StatusConflict, err.Error())
	case errors.Is(err, remote.ErrDisabled):
		s.apiError(w, http.StatusServiceUnavailable, err.Error())
	case err != nil:
		s.log.ErrorContext(r.Context(), "remote claim failed", "error", err)
		s.apiError(w, http.StatusInternalServerError, "could not claim a job")
	case claim == nil:
		w.WriteHeader(http.StatusNoContent)
	default:
		s.writeJSON(w, r, http.StatusOK, claim)
	}
}

// handleWorkerResult receives one job's outcome and its derivative files.
//
// The files are streamed into a staging directory beside the derivatives — the same
// filesystem, so Accept moves each into place with a rename — under a random name that
// the junk report's orphan scan does not mistake for a content hash.
func (s *Server) handleWorkerResult(w http.ResponseWriter, r *http.Request) {
	if !s.remoteAvailable(w) {
		return
	}
	jobID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || jobID <= 0 {
		s.apiError(w, http.StatusBadRequest, "invalid job id")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxResultUpload)
	reader, err := r.MultipartReader()
	if err != nil {
		s.apiError(w, http.StatusBadRequest, "expected a multipart body")
		return
	}

	staging, err := s.stagingDir()
	if err != nil {
		s.log.ErrorContext(r.Context(), "could not create a staging directory", "error", err)
		s.apiError(w, http.StatusInternalServerError, "could not stage the upload")
		return
	}
	defer os.RemoveAll(staging) //nolint:errcheck

	var (
		session    string
		outcome    derive.Outcome
		gotOutcome bool
	)
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			s.apiError(w, http.StatusBadRequest, "could not read the upload")
			return
		}
		switch part.FormName() {
		case "session":
			raw, _ := io.ReadAll(io.LimitReader(part, 256))
			session = string(raw)
		case "outcome":
			if err := json.NewDecoder(io.LimitReader(part, 16<<20)).Decode(&outcome); err != nil {
				s.apiError(w, http.StatusBadRequest, "invalid outcome")
				return
			}
			gotOutcome = true
		case "file":
			if err := stageFile(staging, part); err != nil {
				s.apiError(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		part.Close() //nolint:errcheck
	}
	if !gotOutcome || session == "" {
		s.apiError(w, http.StatusBadRequest, "a result needs a session and an outcome")
		return
	}

	err = s.remote.Submit(r.Context(), jobID, session, outcome, staging)
	switch {
	case errors.Is(err, remote.ErrNoLease):
		s.apiError(w, http.StatusConflict, err.Error())
	case err != nil:
		s.apiError(w, http.StatusBadRequest, err.Error())
	default:
		s.nav.invalidate()
		w.WriteHeader(http.StatusNoContent)
	}
}

// stageFile writes one uploaded derivative under its own name, refusing names the
// server does not write and anything over that name's limit.
func stageFile(staging string, part *multipart.Part) error {
	name := filepath.Base(part.FileName())
	limit, ok := derive.DerivativeLimit(name)
	if !ok {
		return fmt.Errorf("%q is not a derivative file this server writes", name)
	}
	f, err := os.OpenFile(filepath.Join(staging, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("%s was sent twice", name)
	}
	n, err := io.Copy(f, io.LimitReader(part, limit+1))
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("could not store %s: %v", name, err)
	}
	if n > limit {
		return fmt.Errorf("%s is over the %d byte limit", name, limit)
	}
	return nil
}

// stagingDir makes a fresh directory under derivatives/.incoming.
func (s *Server) stagingDir() (string, error) {
	parent := filepath.Join(s.cfg.DataRoot, "derivatives", ".incoming")
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", err
	}
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	dir := filepath.Join(parent, "upload-"+hex.EncodeToString(raw))
	return dir, os.Mkdir(dir, 0o755)
}

func (s *Server) handleWorkerRelease(w http.ResponseWriter, r *http.Request) {
	if !s.remoteAvailable(w) {
		return
	}
	var req remote.ReleaseRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		s.apiError(w, http.StatusBadRequest, "invalid release request")
		return
	}
	s.remote.Release(r.Context(), req)
	w.WriteHeader(http.StatusNoContent)
}
