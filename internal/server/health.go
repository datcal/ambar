package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"time"
)

// handleLiveness is the container HEALTHCHECK and nothing more.
//
// Unauthenticated, so it must leak nothing: no version, no paths, no
// dependency detail. §2 warns the instance may end up publicly reachable
// through Funnel or Cloudflare with no edge rate limiting.
func (s *Server) handleLiveness(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	status, body := http.StatusOK, `{"status":"ok"}`
	if err := s.db.Ping(ctx); err != nil {
		// Logged in full here, reported as one word to the caller.
		s.log.ErrorContext(ctx, "liveness probe: database unreachable", "error", err)
		status, body = http.StatusServiceUnavailable, `{"status":"unhealthy"}`
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body + "\n"))
}

// healthCheck is one named dependency check.
type healthCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// healthReport is the §12 health endpoint's body.
type healthReport struct {
	Status        string        `json:"status"`
	Version       string        `json:"version"`
	Commit        string        `json:"commit"`
	SchemaVersion string        `json:"schema_version"`
	Go            string        `json:"go"`
	StartedAt     time.Time     `json:"started_at"`
	UptimeSeconds int64         `json:"uptime_seconds"`
	Checks        []healthCheck `json:"checks"`

	// NotYetImplemented names the §12 checks this milestone does not perform.
	//
	// A health endpoint that silently omits checks reads as "everything is
	// fine" when it means "I did not look". Saying so explicitly is the whole
	// point of the field.
	NotYetImplemented []string `json:"not_yet_implemented"`
}

// handleHealth is the detailed report (§10, §12). Behind RequireUser.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	report := healthReport{
		Version:       s.build.Version,
		Commit:        s.build.Commit,
		Go:            runtime.Version(),
		StartedAt:     s.startedAt,
		UptimeSeconds: int64(time.Since(s.startedAt).Seconds()),
		NotYetImplemented: []string{
			"job_queue_depth (M2)",
			"failed_job_count (M2)",
			"derivatives_dir_writable (M2)",
			"blender_available (M6)",
			"reflink_support (M13)",
		},
	}

	report.Checks = append(report.Checks, s.checkDatabase(ctx))

	if v, err := s.db.SchemaVersion(ctx); err == nil {
		report.SchemaVersion = v
	}

	// §17: the container needs write access to the library, not only read,
	// because sidecars are written beside originals. Read-only mode relaxes
	// that (§3).
	report.Checks = append(report.Checks,
		checkDirectory("library_root", s.cfg.LibraryRoot, !s.cfg.LibraryReadonly),
		checkDirectory("data_root", s.cfg.DataRoot, true),
	)

	report.Status = "ok"
	status := http.StatusOK
	for _, c := range report.Checks {
		if !c.OK {
			report.Status = "unhealthy"
			status = http.StatusServiceUnavailable
			break
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(report); err != nil {
		s.log.ErrorContext(ctx, "could not write health report", "error", err)
	}
}

func (s *Server) checkDatabase(ctx context.Context) healthCheck {
	c := healthCheck{Name: "database", OK: true}
	if err := s.db.Ping(ctx); err != nil {
		return healthCheck{Name: "database", OK: false, Detail: err.Error()}
	}
	// A ping only proves the connection is alive. Reading through the read pool
	// proves the schema is actually usable.
	var n int
	if err := s.db.Reader.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&n); err != nil {
		return healthCheck{Name: "database", OK: false, Detail: "read pool query failed: " + err.Error()}
	}
	if n == 0 {
		// Not a failure: a fresh install has no users. But it is the single most
		// likely reason someone cannot log in, so say it here.
		c.Detail = "no users exist yet; create one with `ambar user add <username>`"
	}
	return c
}

// checkDirectory repeats the startup probe at runtime, because a bind mount can
// disappear or go read-only while the process is running — exactly the case §12
// cares about for a NAS share.
func checkDirectory(name, path string, needWrite bool) healthCheck {
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return healthCheck{Name: name, OK: false, Detail: fmt.Sprintf("%s does not exist (mount missing?)", path)}
	case err != nil:
		return healthCheck{Name: name, OK: false, Detail: fmt.Sprintf("%s: %v", path, err)}
	case !info.IsDir():
		return healthCheck{Name: name, OK: false, Detail: fmt.Sprintf("%s is not a directory", path)}
	}

	if _, err := os.ReadDir(path); err != nil {
		return healthCheck{Name: name, OK: false, Detail: fmt.Sprintf("%s is not readable: %v", path, err)}
	}
	if !needWrite {
		return healthCheck{Name: name, OK: true, Detail: path + " (read-only mode)"}
	}

	probe, err := os.CreateTemp(path, ".ambar-health-*")
	if err != nil {
		return healthCheck{Name: name, OK: false, Detail: fmt.Sprintf("%s is not writable: %v", path, err)}
	}
	probeName := probe.Name()
	probe.Close()
	if err := os.Remove(probeName); err != nil {
		return healthCheck{Name: name, OK: false,
			Detail: fmt.Sprintf("%s: could not remove probe file %s: %v", path, probeName, err)}
	}
	return healthCheck{Name: name, OK: true, Detail: path}
}
