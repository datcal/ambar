package dupes

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/datcal/ambar/internal/db"
	"github.com/datcal/ambar/internal/jobs"
)

// JobType is the queue type for a duplicate sweep. Invariant 8: comparing every
// pack's hash set and every image's perceptual hash is real work, and no HTTP
// handler runs it.
const JobType = "maintenance.dupes"

// ReportFile is where the latest report is cached under the data root, beside the
// junk report and the derivatives. It is regenerable output, not source of truth,
// so it does not belong in the database.
const ReportFile = "dupes-report.json"

// StoredReport is a report plus when it was produced.
type StoredReport struct {
	ScannedAt int64  `json:"scanned_at"`
	Report    Report `json:"report"`
}

// ScannedTime is ScannedAt as a time, for templates.
func (s StoredReport) ScannedTime() time.Time { return time.Unix(s.ScannedAt, 0) }

// Runner executes duplicate sweeps on the queue and caches the result.
type Runner struct {
	detector *Detector
	dataRoot string
	log      *slog.Logger
	now      func() time.Time
}

// NewRunner builds a Runner.
func NewRunner(database *db.DB, dataRoot string, opts Options, log *slog.Logger) *Runner {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Runner{
		detector: NewDetector(database, opts),
		dataRoot: dataRoot,
		log:      log,
		now:      time.Now,
	}
}

// WithClock replaces the clock, for tests.
func (r *Runner) WithClock(now func() time.Time) *Runner { r.now = now; return r }

// Register attaches the handler to a queue.
func (r *Runner) Register(q *jobs.Queue) { q.Register(JobType, r.Handle) }

// Handle runs one sweep and caches the report. Read-only throughout: this package
// has no code that can change a file.
func (r *Runner) Handle(ctx context.Context, _ []byte) error {
	report, err := r.detector.Scan(ctx)
	if err != nil {
		return err
	}
	if err := WriteReport(r.dataRoot, report, r.now().Unix()); err != nil {
		return err
	}
	r.log.InfoContext(ctx, "duplicate sweep complete",
		"packs", len(report.Packs), "exact", len(report.Exact), "near", len(report.Near),
		"reclaimable_bytes", report.Stats.ReclaimableBytes)
	return nil
}

// ReportPath is the cached report's location.
func ReportPath(dataRoot string) string { return filepath.Join(dataRoot, ReportFile) }

// WriteReport caches a report atomically, so a crash mid-write cannot leave a
// half-written file that parses as an empty report.
func WriteReport(dataRoot string, report *Report, scannedAt int64) error {
	data, err := json.MarshalIndent(StoredReport{ScannedAt: scannedAt, Report: *report}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal dupes report: %w", err)
	}

	tmp, err := os.CreateTemp(dataRoot, ".tmp-dupes-report-*")
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
	if err := os.Rename(tmpName, ReportPath(dataRoot)); err != nil {
		return fmt.Errorf("rename report into place: %w", err)
	}
	return nil
}

// LoadReport reads the cached report, returning (nil, nil) when no sweep has run
// yet — a first visit, not an error.
func LoadReport(dataRoot string) (*StoredReport, error) {
	data, err := os.ReadFile(ReportPath(dataRoot))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read dupes report: %w", err)
	}
	var stored StoredReport
	if err := json.Unmarshal(data, &stored); err != nil {
		return nil, fmt.Errorf("parse dupes report: %w", err)
	}
	return &stored, nil
}

// FindExact returns the finding for one content hash, or nil. The web flow uses it
// to re-check a selection against the cached report before planning: a checkbox
// naming a path that is no longer part of a finding must not become an operation.
func (s *StoredReport) FindExact(sha string) *ExactFinding {
	if s == nil {
		return nil
	}
	for i := range s.Report.Exact {
		if s.Report.Exact[i].Sha == sha {
			return &s.Report.Exact[i]
		}
	}
	return nil
}

// FindPack returns the pack finding with the given id, or nil.
func (s *StoredReport) FindPack(id string) *PackFinding {
	if s == nil {
		return nil
	}
	for i := range s.Report.Packs {
		if s.Report.Packs[i].ID() == id {
			return &s.Report.Packs[i]
		}
	}
	return nil
}
