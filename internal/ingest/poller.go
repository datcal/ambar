package ingest

import (
	"bufio"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/datcal/ambar/internal/jobs"
)

// archiveExts are the extensions the poller acts on. Detection is by magic at
// ingest time; the extension is only the cheap filter that keeps the poller from
// enqueuing readmes and the .url sidecars beside an archive.
var archiveExts = map[string]bool{".zip": true, ".rar": true, ".7z": true}

// Poller watches _inbox and enqueues an ingest job for each archive that has
// finished arriving (§5: "A file is ready when its size and mtime are unchanged
// across two consecutive polls"). Polling, not inotify, because it must work over
// SMB and bind mounts.
//
// The poller never extracts anything itself — invariant 8 keeps work on the job
// queue — it only decides a file is stable and enqueues it. Enqueue is
// deduplicated on the path, so seeing a still-present file on later polls is
// harmless.
type Poller struct {
	root     string
	queue    *jobs.Queue
	interval time.Duration
	log      *slog.Logger

	// prev is the previous poll's (size, mtime) per inbox-relative path, the basis
	// of the stability check.
	prev map[string]fileStamp
}

type fileStamp struct {
	size  int64
	mtime int64
}

// NewPoller builds a Poller.
func NewPoller(root string, queue *jobs.Queue, interval time.Duration, log *slog.Logger) *Poller {
	if log == nil {
		log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	return &Poller{root: root, queue: queue, interval: interval, log: log, prev: map[string]fileStamp{}}
}

// Run polls until ctx is cancelled. It is meant to run as a goroutine under
// `ambar serve`.
func (p *Poller) Run(ctx context.Context) {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if n, err := p.pollOnce(ctx); err != nil {
				p.log.WarnContext(ctx, "inbox poll failed", "error", err)
			} else if n > 0 {
				p.log.InfoContext(ctx, "queued archives from _inbox", "count", n)
			}
		}
	}
}

// pollOnce scans _inbox once, enqueues the archives that are stable since the
// last poll, and returns how many it enqueued.
func (p *Poller) pollOnce(ctx context.Context) (int, error) {
	inbox := filepath.Join(p.root, InboxDir)
	entries, err := os.ReadDir(inbox)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil // no _inbox yet is not an error
		}
		return 0, err
	}

	current := make(map[string]fileStamp, len(entries))
	enqueued := 0
	for _, e := range entries {
		if e.IsDir() || !archiveExts[strings.ToLower(filepath.Ext(e.Name()))] {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue // vanished between ReadDir and stat; catch it next poll
		}
		stamp := fileStamp{size: info.Size(), mtime: info.ModTime().UnixNano()}
		current[e.Name()] = stamp

		// Ready only when this poll matches the last: two consecutive identical
		// observations mean the copy has finished.
		if was, ok := p.prev[e.Name()]; ok && was == stamp {
			rel := InboxDir + "/" + e.Name()
			source := sniffSourceURL(inbox, e.Name())
			if _, err := Enqueue(ctx, p.queue, Payload{ArchiveRelPath: rel, SourceURL: source}); err != nil {
				// A duplicate is expected (the job is still queued/running); anything
				// else is worth logging but not fatal to the poll.
				if err != jobs.ErrDuplicate {
					p.log.WarnContext(ctx, "enqueue ingest failed", "archive", rel, "error", err)
				}
				continue
			}
			enqueued++
		}
	}
	p.prev = current
	return enqueued, nil
}

// sniffSourceURL reads a source URL dropped beside the archive (§5: "if a
// <archive>.url or <archive>.txt file is dropped ... read the source URL from
// it"). It tries the common sidecar names and returns the first URL-looking line.
func sniffSourceURL(inbox, archiveName string) string {
	stem := strings.TrimSuffix(archiveName, filepath.Ext(archiveName))
	candidates := []string{
		archiveName + ".url", archiveName + ".txt",
		stem + ".url", stem + ".txt",
	}
	for _, name := range candidates {
		if url := firstURL(filepath.Join(inbox, name)); url != "" {
			return url
		}
	}
	return ""
}

// firstURL returns the first http(s) URL in a file, tolerating the Windows
// ".url" INI format (`URL=https://...`) as well as a plain URL on a line.
func firstURL(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if i := strings.Index(strings.ToLower(line), "url="); i == 0 {
			line = strings.TrimSpace(line[len("url="):])
		}
		if strings.HasPrefix(line, "http://") || strings.HasPrefix(line, "https://") {
			return line
		}
	}
	return ""
}
