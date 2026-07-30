// Package junk detects the removable clutter of spec §9.1 "Junk cleanup":
// `__MACOSX` shadow trees, OS metadata files (`.DS_Store`, `Thumbs.db`,
// `desktop.ini`, AppleDouble `._*`), zero-byte files, empty directories, and
// orphaned derivative directories under the data root.
//
// It is a DETECTOR AND REPORTER ONLY (§9.1, invariant 3). Nothing here deletes,
// moves, or trashes anything — it walks, measures, and returns a report sorted by
// how many bytes each finding would reclaim. The removal path, its trash staging,
// and the safety invariants ship together in M13; keeping detection free of any
// destructive code is deliberate, because §9.1 calls removal "the highest-risk
// surface in the codebase".
//
// Like internal/library, this package is free of SQL: orphan detection needs the
// set of live content hashes, but the caller supplies it, so the whole package
// stays testable against a fixture tree with no database in the way.
package junk

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/datcal/ambar/internal/library"
)

// Kind labels a category of finding.
type Kind string

const (
	// KindMacOSX is a `__MACOSX` shadow tree — the one that matters most, because
	// it mirrors a whole pack and doubles the apparent asset count (§5.1).
	KindMacOSX Kind = "macosx"
	// KindOSJunk is an OS metadata file: .DS_Store, Thumbs.db, desktop.ini, ._*.
	KindOSJunk Kind = "os_junk"
	// KindZeroByte is a zero-length regular file.
	KindZeroByte Kind = "zero_byte"
	// KindEmptyDir is a directory with no entries at all.
	KindEmptyDir Kind = "empty_dir"
	// KindOrphanDerivative is a derivatives/<xx>/<sha>/ directory whose content hash
	// no longer matches any asset.
	KindOrphanDerivative Kind = "orphan_derivative"
)

// Title is a short human label for a finding kind.
func (k Kind) Title() string {
	switch k {
	case KindMacOSX:
		return "__MACOSX shadow trees"
	case KindOSJunk:
		return "OS metadata files"
	case KindZeroByte:
		return "Zero-byte files"
	case KindEmptyDir:
		return "Empty directories"
	case KindOrphanDerivative:
		return "Orphaned derivatives"
	default:
		return string(k)
	}
}

// Explanation says why a finding kind is safe to consider removing.
func (k Kind) Explanation() string {
	switch k {
	case KindMacOSX:
		return "macOS archive cruft that duplicates a pack's tree; never real content."
	case KindOSJunk:
		return "Finder and Explorer metadata; safe to remove, and there are usually many."
	case KindZeroByte:
		return "Empty files that carry no data."
	case KindEmptyDir:
		return "Directories left behind with nothing in them."
	case KindOrphanDerivative:
		return "Generated thumbnails whose source asset is gone; regenerated on demand."
	default:
		return ""
	}
}

// Item is one candidate path.
type Item struct {
	// Path is slash-separated. For library findings it is relative to the library
	// root; for orphaned derivatives it is relative to the data root
	// (e.g. derivatives/ab/abcd…).
	Path string `json:"path"`
	// Bytes is the reclaimable size: the file's size, or the recursive total for a
	// `__MACOSX` tree or an orphan derivative directory.
	Bytes int64 `json:"bytes"`
	// Detail is an optional note, e.g. "142 files" for a tree.
	Detail string `json:"detail,omitempty"`
}

// Finding groups the items of one Kind.
type Finding struct {
	Kind       Kind   `json:"kind"`
	Items      []Item `json:"items"`
	TotalBytes int64  `json:"total_bytes"`
}

// Count is the number of items, for templates.
func (f Finding) Count() int { return len(f.Items) }

// Report is the full set of findings, sorted by reclaimable bytes descending —
// §9.1: "sort by largest win. That number is what makes the view worth opening."
type Report struct {
	Findings []Finding `json:"findings"`
	// LibraryScanned and DataScanned record which roots were walked, so a report
	// with no data root is distinguishable from one that found no orphans.
	LibraryScanned bool `json:"library_scanned"`
	DataScanned    bool `json:"data_scanned"`
}

// TotalBytes is the sum across every finding.
func (r Report) TotalBytes() int64 {
	var total int64
	for _, f := range r.Findings {
		total += f.TotalBytes
	}
	return total
}

// TotalItems is the number of candidate paths across every finding.
func (r Report) TotalItems() int {
	n := 0
	for _, f := range r.Findings {
		n += len(f.Items)
	}
	return n
}

// Empty reports whether nothing was found.
func (r Report) Empty() bool { return len(r.Findings) == 0 }

// Options configures a scan.
type Options struct {
	// LibraryRoot is the absolute, already-validated library root. Empty skips the
	// library walk.
	LibraryRoot string
	// DataRoot is where derivatives/ lives. Empty skips orphan detection.
	DataRoot string
	// KnownHashes is the set of content hashes (lowercase hex) still referenced by
	// an asset. A derivative directory whose hash is not in this set is orphaned.
	// Required for orphan detection; without it that check is skipped rather than
	// reporting every derivative as an orphan.
	KnownHashes map[string]struct{}
}

// osJunkGlobs is the §9.1 OS-metadata list. Deliberately NOT the full library
// ignore set: `.git` and `__MACOSX` are handled elsewhere (a `.git` directory is
// never proposed for removal, and `__MACOSX` is its own finding), so this is only
// the loose metadata files.
var osJunkGlobs = []string{".DS_Store", "Thumbs.db", "desktop.ini", "._*"}

// Scan walks the configured roots and returns the report. It never modifies
// anything.
func Scan(opts Options) (*Report, error) {
	report := &Report{}

	matcher, err := library.NewMatcher(osJunkGlobs)
	if err != nil {
		return nil, fmt.Errorf("junk globs: %w", err)
	}

	// Collect items per kind, then assemble findings in a stable order.
	items := map[Kind][]Item{}
	add := func(k Kind, it Item) { items[k] = append(items[k], it) }

	if opts.LibraryRoot != "" {
		report.LibraryScanned = true
		if err := scanLibrary(opts.LibraryRoot, matcher, add); err != nil {
			return nil, err
		}
	}

	// Orphan detection needs the live-hash set: a nil map means the caller did not
	// supply it, and without it every derivative would look orphaned, so the check
	// is skipped entirely rather than reporting the whole cache as removable. An
	// empty (non-nil) map is different — it means there genuinely are no assets, so
	// every derivative really is an orphan.
	if opts.DataRoot != "" && opts.KnownHashes != nil {
		report.DataScanned = true
		if err := scanOrphans(opts.DataRoot, opts.KnownHashes, add); err != nil {
			return nil, err
		}
	}

	for _, k := range []Kind{KindMacOSX, KindOSJunk, KindZeroByte, KindEmptyDir, KindOrphanDerivative} {
		list := items[k]
		if len(list) == 0 {
			continue
		}
		// Deterministic within a finding: by path.
		sort.Slice(list, func(i, j int) bool { return list[i].Path < list[j].Path })
		var total int64
		for _, it := range list {
			total += it.Bytes
		}
		report.Findings = append(report.Findings, Finding{Kind: k, Items: list, TotalBytes: total})
	}

	// Largest win first (§9.1).
	sort.SliceStable(report.Findings, func(i, j int) bool {
		return report.Findings[i].TotalBytes > report.Findings[j].TotalBytes
	})
	return report, nil
}

// scanLibrary walks the library once, classifying junk without descending into a
// `__MACOSX` tree (its size is summed and reported as one item).
func scanLibrary(root string, matcher *library.Matcher, add func(Kind, Item)) error {
	return filepath.WalkDir(root, func(absPath string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable directory is skipped, not fatal: one bad directory must
			// not abandon the whole scan (§16).
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		rel, relErr := filepath.Rel(root, absPath)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		name := d.Name()

		if d.IsDir() {
			// A `__MACOSX` tree is reported whole and never descended into, so its
			// contents are not double-counted as individual junk files.
			if strings.EqualFold(name, "__MACOSX") {
				bytes, files := treeSize(absPath)
				add(KindMacOSX, Item{
					Path:   rel,
					Bytes:  bytes,
					Detail: fmt.Sprintf("%d file(s)", files),
				})
				return fs.SkipDir
			}
			// Ambar's own reserved directories (_inbox, _trash, _archives,
			// _quarantine) at the top level are working areas, not junk — the trash
			// especially must never be reported as clutter to clean up.
			if !strings.Contains(rel, "/") && library.IsReserved(name) {
				return fs.SkipDir
			}
			// Symlinked directories are not descended into or reported as content.
			if d.Type()&os.ModeSymlink != 0 {
				return fs.SkipDir
			}
			// An empty directory: no entries at all. A directory that merely holds a
			// `.DS_Store` is not reported here — the file is the finding, and removing
			// it is what makes the directory empty on a later scan.
			entries, readErr := os.ReadDir(absPath)
			if readErr == nil && len(entries) == 0 {
				add(KindEmptyDir, Item{Path: rel})
			}
			return nil
		}

		// --- a file ---

		info, statErr := d.Info()
		if statErr != nil {
			return nil
		}

		if matcher.Ignored(name) {
			add(KindOSJunk, Item{Path: rel, Bytes: info.Size()})
			return nil
		}
		// A zero-byte regular file. The sidecar is metadata, not junk, even when a
		// broken writer left it empty — deleting it would lose a pack's provenance
		// anchor, so it is never proposed here.
		if info.Size() == 0 && name != library.SidecarName {
			add(KindZeroByte, Item{Path: rel})
		}
		return nil
	})
}

// scanOrphans finds derivative directories whose content hash is no longer
// referenced by any asset (§9.1).
func scanOrphans(dataRoot string, known map[string]struct{}, add func(Kind, Item)) error {
	derivRoot := filepath.Join(dataRoot, "derivatives")
	shardEntries, err := os.ReadDir(derivRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // no derivatives yet is not an error
		}
		return fmt.Errorf("read derivatives: %w", err)
	}

	for _, shard := range shardEntries {
		if !shard.IsDir() {
			continue
		}
		shardDir := filepath.Join(derivRoot, shard.Name())
		shaEntries, err := os.ReadDir(shardDir)
		if err != nil {
			continue
		}
		for _, e := range shaEntries {
			if !e.IsDir() {
				continue
			}
			sha := e.Name()
			if !isHashDir(sha) {
				// Not a content-hash directory; leave anything unexpected alone
				// rather than guess it is removable.
				continue
			}
			if _, ok := known[strings.ToLower(sha)]; ok {
				continue // still referenced
			}
			absDir := filepath.Join(shardDir, sha)
			bytes, files := treeSize(absDir)
			rel := filepath.ToSlash(filepath.Join("derivatives", shard.Name(), sha))
			add(KindOrphanDerivative, Item{
				Path:   rel,
				Bytes:  bytes,
				Detail: fmt.Sprintf("%d file(s)", files),
			})
		}
	}
	return nil
}

// treeSize sums the sizes of every regular file beneath a directory, and counts
// them. An unreadable entry contributes nothing rather than aborting the walk.
func treeSize(dir string) (bytes int64, files int) {
	filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error { //nolint:errcheck
		if err != nil || d.IsDir() {
			return nil
		}
		if info, statErr := d.Info(); statErr == nil && info.Mode().IsRegular() {
			bytes += info.Size()
			files++
		}
		return nil
	})
	return bytes, files
}

// isHashDir reports whether a directory name is a 64-character hex content hash,
// the only shape derive.Dir produces.
func isHashDir(name string) bool {
	if len(name) != 64 {
		return false
	}
	for _, r := range name {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}
