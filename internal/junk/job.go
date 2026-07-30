package junk

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/datcal/ambar/internal/jobs"
)

// JobType is the queue type for a junk sweep (invariant 8: the library walk is real
// work and belongs on the job queue with pollable status, not inside a GET).
const JobType = "maintenance.junk"

// ReportFile is where the latest report is cached, beside the derivatives under the
// data root. It is a rebuildable report, not source of truth, so it lives with the
// other generated data rather than in the database.
const ReportFile = "junk-report.json"

// HashProvider returns the set of live content hashes. It is a callback so this
// package stays free of SQL, exactly as index.RegisterScanJob keeps the indexer
// unaware of the deriver.
type HashProvider func(ctx context.Context) (map[string]struct{}, error)

// StoredReport is a report plus when it was produced, as cached on disk.
type StoredReport struct {
	ScannedAt int64  `json:"scanned_at"` // Unix seconds
	Report    Report `json:"report"`
}

// ScannedTime is ScannedAt as a time, for templates.
func (s StoredReport) ScannedTime() time.Time { return time.Unix(s.ScannedAt, 0) }

// Runner executes junk sweeps on the queue and caches the result.
type Runner struct {
	libraryRoot string
	dataRoot    string
	hashes      HashProvider
	log         *slog.Logger
	now         func() time.Time
}

// NewRunner builds a Runner. hashes may be nil, in which case orphan detection is
// skipped (the report still covers library junk).
func NewRunner(libraryRoot, dataRoot string, hashes HashProvider, log *slog.Logger) *Runner {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Runner{
		libraryRoot: libraryRoot,
		dataRoot:    dataRoot,
		hashes:      hashes,
		log:         log,
		now:         time.Now,
	}
}

// Register attaches the handler to a queue.
func (r *Runner) Register(q *jobs.Queue) { q.Register(JobType, r.Handle) }

// Handle runs one sweep and writes the cached report. It is read-only against the
// library and the data root — it never removes anything (invariant 3).
func (r *Runner) Handle(ctx context.Context, _ []byte) error {
	var known map[string]struct{}
	if r.hashes != nil {
		h, err := r.hashes(ctx)
		if err != nil {
			return fmt.Errorf("load content hashes: %w", err)
		}
		known = h
	}

	report, err := Scan(Options{
		LibraryRoot: r.libraryRoot,
		DataRoot:    r.dataRoot,
		KnownHashes: known,
	})
	if err != nil {
		return err
	}

	if err := WriteReport(r.dataRoot, report, r.now().Unix()); err != nil {
		return err
	}
	r.log.InfoContext(ctx, "junk sweep complete",
		"findings", len(report.Findings), "items", report.TotalItems(), "bytes", report.TotalBytes())
	return nil
}

// ReportPath is the cached report's location under the data root.
func ReportPath(dataRoot string) string { return filepath.Join(dataRoot, ReportFile) }

// WriteReport caches a report atomically: a crash mid-write must not leave a
// half-written JSON file that later parses as an empty report.
func WriteReport(dataRoot string, report *Report, scannedAt int64) error {
	stored := StoredReport{ScannedAt: scannedAt, Report: *report}
	data, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal junk report: %w", err)
	}

	path := ReportPath(dataRoot)
	tmp, err := os.CreateTemp(dataRoot, ".tmp-junk-report-*")
	if err != nil {
		return fmt.Errorf("create temp report: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) //nolint:errcheck // no-op once renamed

	if _, err := tmp.Write(data); err != nil {
		tmp.Close() //nolint:errcheck
		return fmt.Errorf("write report: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close report: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename report into place: %w", err)
	}
	return nil
}

// LoadReport reads the cached report, returning (nil, nil) when none has been
// produced yet — a first visit before any sweep has run.
func LoadReport(dataRoot string) (*StoredReport, error) {
	data, err := os.ReadFile(ReportPath(dataRoot))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read junk report: %w", err)
	}
	var stored StoredReport
	if err := json.Unmarshal(data, &stored); err != nil {
		return nil, fmt.Errorf("parse junk report: %w", err)
	}
	return &stored, nil
}
