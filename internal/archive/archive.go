// Package archive inspects and safely extracts the archive formats §5 ingests:
// zip, RAR5 and 7z, all through pure-Go decoders (invariant 6). "Safely" is the
// whole point of the package: every entry path is sanitised against traversal
// through internal/safepath, symlinks and other non-regular entries are refused,
// and total-size and entry-count caps defend against zip bombs (§5 step 2).
//
// Nothing here writes outside destRoot. A single traversal attempt aborts the
// whole extraction rather than skipping the one entry: an archive that tries it
// is hostile, and the ingest pipeline (§5) routes the failure to _quarantine.
package archive

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/bodgit/sevenzip"
	"github.com/datcal/ambar/internal/safepath"
	rardecode "github.com/nwaples/rardecode/v2"
)

// zipOpen wraps the stdlib zip reader so the two zip helpers share one error style.
func zipOpen(archivePath string) (*zip.ReadCloser, error) {
	r, err := zip.OpenReader(archivePath)
	if err != nil {
		return nil, fmt.Errorf("open zip: %w", err)
	}
	return r, nil
}

// Kind is a supported archive format.
type Kind string

const (
	KindZip      Kind = "zip"
	KindRar      Kind = "rar"
	KindSevenZip Kind = "7z"
	KindUnknown  Kind = ""
)

// Default caps, used when Options leaves them zero. Deliberately generous — a
// real pack can be large — but finite, so a bomb cannot exhaust the disk.
const (
	DefaultMaxTotalBytes int64 = 8 << 30 // 8 GiB uncompressed
	DefaultMaxEntries          = 200_000
)

var (
	// ErrUnsupported means the bytes are not a format we extract.
	ErrUnsupported = errors.New("unsupported archive format")
	// ErrUnsafeEntry means an entry tried to escape destRoot — the archive is hostile.
	ErrUnsafeEntry = errors.New("archive entry escapes the destination")
	// ErrTooLarge means extraction exceeded the uncompressed-size cap (zip bomb).
	ErrTooLarge = errors.New("archive exceeds the uncompressed size cap")
	// ErrTooManyEntries means extraction exceeded the entry-count cap (zip bomb).
	ErrTooManyEntries = errors.New("archive exceeds the entry count cap")
)

// Entry is one member of an archive, as Inspect reports it.
type Entry struct {
	Name  string // slash-separated, as stored in the archive
	IsDir bool
	Mode  fs.FileMode
	Size  int64 // uncompressed bytes
}

// Info is the result of inspecting an archive.
type Info struct {
	Kind      Kind
	Entries   []Entry
	TotalSize int64 // sum of uncompressed entry sizes as the archive claims
	FileCount int   // non-directory entries
}

// Options tunes extraction. Zero values take the package defaults.
type Options struct {
	MaxTotalBytes int64
	MaxEntries    int
}

// Result summarises what an extraction did.
type Result struct {
	FilesWritten int
	BytesWritten int64
	// Flattened is the redundant top-level folder that was stripped, or "".
	Flattened string
	// Skipped lists entries refused as non-regular (symlinks, devices).
	Skipped []string
}

// DetectKind identifies an archive by magic bytes, falling back to extension.
// Magic first, because a mislabelled `.zip` that is really a 7z should still work.
func DetectKind(archivePath string) Kind {
	if k := detectByMagic(archivePath); k != KindUnknown {
		return k
	}
	switch strings.ToLower(filepath.Ext(archivePath)) {
	case ".zip":
		return KindZip
	case ".rar":
		return KindRar
	case ".7z":
		return KindSevenZip
	}
	return KindUnknown
}

func detectByMagic(archivePath string) Kind {
	f, err := os.Open(archivePath)
	if err != nil {
		return KindUnknown
	}
	defer f.Close()
	var b [8]byte
	n, _ := io.ReadFull(f, b[:])
	head := b[:n]
	switch {
	case len(head) >= 4 && string(head[:4]) == "PK\x03\x04":
		return KindZip
	case len(head) >= 4 && string(head[:4]) == "PK\x05\x06": // empty zip
		return KindZip
	case len(head) >= 7 && string(head[:7]) == "Rar!\x1a\x07\x00": // RAR 4.x
		return KindRar
	case len(head) >= 8 && string(head[:8]) == "Rar!\x1a\x07\x01\x00": // RAR 5.x
		return KindRar
	case len(head) >= 6 && string(head[:6]) == "7z\xbc\xaf\x27\x1c":
		return KindSevenZip
	}
	return KindUnknown
}

// Inspect lists an archive's entries without extracting anything (§5's
// archive.inspect step).
func Inspect(archivePath string) (Info, error) {
	kind := DetectKind(archivePath)
	var (
		entries []Entry
		err     error
	)
	switch kind {
	case KindZip:
		entries, err = listZip(archivePath)
	case KindRar:
		entries, err = listRar(archivePath)
	case KindSevenZip:
		entries, err = listSevenZip(archivePath)
	default:
		return Info{}, fmt.Errorf("%w: %s", ErrUnsupported, filepath.Base(archivePath))
	}
	if err != nil {
		return Info{}, err
	}

	info := Info{Kind: kind, Entries: entries}
	for _, e := range entries {
		if e.IsDir {
			continue
		}
		info.FileCount++
		info.TotalSize += e.Size
	}
	return info, nil
}

// Extract unpacks an archive into destRoot, safely (§5 step 2). destRoot must
// already exist. It returns what it did, or an error — on which the caller must
// treat destRoot as tainted (the ingest pipeline moves it to _quarantine).
func Extract(archivePath, destRoot string, opts Options) (Result, error) {
	if opts.MaxTotalBytes <= 0 {
		opts.MaxTotalBytes = DefaultMaxTotalBytes
	}
	if opts.MaxEntries <= 0 {
		opts.MaxEntries = DefaultMaxEntries
	}

	info, err := Inspect(archivePath)
	if err != nil {
		return Result{}, err
	}
	if info.FileCount > opts.MaxEntries {
		return Result{}, fmt.Errorf("%w: %d entries (cap %d)", ErrTooManyEntries, info.FileCount, opts.MaxEntries)
	}

	flatten := flattenPrefix(info.Entries)
	res := Result{Flattened: strings.TrimSuffix(flatten, "/")}
	remaining := opts.MaxTotalBytes

	sink := &sink{
		destRoot:  destRoot,
		flatten:   flatten,
		remaining: &remaining,
		res:       &res,
	}

	switch info.Kind {
	case KindZip:
		err = extractZip(archivePath, sink)
	case KindRar:
		err = extractRar(archivePath, sink)
	case KindSevenZip:
		err = extractSevenZip(archivePath, sink)
	default:
		return Result{}, fmt.Errorf("%w: %s", ErrUnsupported, filepath.Base(archivePath))
	}
	if err != nil {
		return res, err
	}
	return res, nil
}

// sink writes sanitised entries under destRoot, enforcing the caps. It is the one
// place bytes reach the filesystem, so it is the one place the safety rules live.
type sink struct {
	destRoot  string
	flatten   string
	remaining *int64
	res       *Result
}

// write handles one entry. src is nil for directories.
func (s *sink) write(name string, mode fs.FileMode, isDir bool, src io.Reader) error {
	// Refuse anything that is not a plain file or directory: a symlink is a
	// traversal vector (§5), a device or fifo has no business in an asset pack.
	if !isDir && !mode.IsRegular() {
		s.res.Skipped = append(s.res.Skipped, name)
		return nil
	}

	rel := strings.TrimPrefix(name, s.flatten)
	rel = strings.TrimSpace(rel)
	if rel == "" || rel == "." {
		return nil // the flattened top-level directory itself
	}

	// The one gate: every destination is resolved against destRoot, and a path
	// that escapes aborts the whole extraction.
	target, err := safepath.Resolve(s.destRoot, rel)
	if err != nil {
		return fmt.Errorf("%w: %q: %v", ErrUnsafeEntry, name, err)
	}

	if isDir {
		return os.MkdirAll(target, 0o755)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("create parent of %q: %w", rel, err)
	}

	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("create %q: %w", rel, err)
	}
	defer f.Close()

	// Cap the copy at the remaining budget. LimitReader(+1) lets us detect an
	// entry that lies about its size and tries to overflow the cap.
	n, err := io.Copy(f, io.LimitReader(src, *s.remaining+1))
	if err != nil {
		return fmt.Errorf("write %q: %w", rel, err)
	}
	if n > *s.remaining {
		return fmt.Errorf("%w: at entry %q", ErrTooLarge, name)
	}
	*s.remaining -= n
	s.res.FilesWritten++
	s.res.BytesWritten += n
	return nil
}

// flattenPrefix returns the single redundant top-level directory to strip, with a
// trailing slash, or "" when there is not exactly one (§5 "flatten a single
// redundant top-level folder if present").
func flattenPrefix(entries []Entry) string {
	top := ""
	for _, e := range entries {
		name := strings.Trim(e.Name, "/")
		if name == "" {
			continue
		}
		first := name
		if i := strings.IndexByte(name, '/'); i >= 0 {
			first = name[:i]
		} else if !e.IsDir {
			// A file sitting at the top level means there is no single wrapper dir.
			return ""
		}
		// Never treat "." or ".." as the wrapper: stripping them would neutralise a
		// traversal into a valid in-root path and rob safepath of the chance to
		// reject it. A hostile entry must reach the gate unflattened.
		if first == "." || first == ".." {
			return ""
		}
		if top == "" {
			top = first
		} else if top != first {
			return ""
		}
	}
	if top == "" {
		return ""
	}
	return top + "/"
}

// cleanEntryName canonicalises an archive entry name for the safety gate, using
// path.Clean to fold "./" and "//" — but deliberately WITHOUT anchoring at root.
//
// Anchoring (path.Clean("/"+name)) would silently rewrite "../evil" to "evil",
// neutralising a traversal into a valid in-root path. That is a defensible
// design, but this package's contract is stronger: a traversal attempt is
// hostile and must abort the whole extraction. So ".." and absolute prefixes are
// preserved here and left for safepath.Resolve to reject. Backslashes are left
// intact too — the three readers all emit '/' separators, so a backslash is
// anomalous, and safepath refuses it.
func cleanEntryName(name string) string {
	return path.Clean(name)
}

// --- per-format listing -----------------------------------------------------

func listZip(archivePath string) ([]Entry, error) {
	r, err := zipOpen(archivePath)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var out []Entry
	for _, f := range r.File {
		fi := f.FileInfo()
		out = append(out, Entry{
			Name:  cleanEntryName(f.Name),
			IsDir: fi.IsDir(),
			Mode:  f.Mode(),
			Size:  int64(f.UncompressedSize64),
		})
	}
	return out, nil
}

func listSevenZip(archivePath string) ([]Entry, error) {
	r, err := sevenzip.OpenReader(archivePath)
	if err != nil {
		return nil, fmt.Errorf("open 7z: %w", err)
	}
	defer r.Close()
	var out []Entry
	for _, f := range r.File {
		fi := f.FileInfo()
		out = append(out, Entry{
			Name:  cleanEntryName(f.Name),
			IsDir: fi.IsDir(),
			Mode:  f.Mode(),
			Size:  fi.Size(),
		})
	}
	return out, nil
}

func listRar(archivePath string) ([]Entry, error) {
	rc, err := rardecode.OpenReader(archivePath)
	if err != nil {
		return nil, fmt.Errorf("open rar: %w", err)
	}
	defer rc.Close()
	var out []Entry
	for {
		hdr, err := rc.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read rar header: %w", err)
		}
		out = append(out, Entry{
			Name:  cleanEntryName(hdr.Name),
			IsDir: hdr.IsDir,
			Mode:  hdr.Mode(),
			Size:  hdr.UnPackedSize,
		})
	}
	return out, nil
}

// --- per-format extraction --------------------------------------------------

func extractZip(archivePath string, s *sink) error {
	r, err := zipOpen(archivePath)
	if err != nil {
		return err
	}
	defer r.Close()
	for _, f := range r.File {
		name := cleanEntryName(f.Name)
		if f.FileInfo().IsDir() {
			if err := s.write(name, f.Mode(), true, nil); err != nil {
				return err
			}
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return fmt.Errorf("open zip entry %q: %w", name, err)
		}
		err = s.write(name, f.Mode(), false, rc)
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func extractSevenZip(archivePath string, s *sink) error {
	r, err := sevenzip.OpenReader(archivePath)
	if err != nil {
		return fmt.Errorf("open 7z: %w", err)
	}
	defer r.Close()
	for _, f := range r.File {
		name := cleanEntryName(f.Name)
		if f.FileInfo().IsDir() {
			if err := s.write(name, f.Mode(), true, nil); err != nil {
				return err
			}
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return fmt.Errorf("open 7z entry %q: %w", name, err)
		}
		err = s.write(name, f.Mode(), false, rc)
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func extractRar(archivePath string, s *sink) error {
	rc, err := rardecode.OpenReader(archivePath)
	if err != nil {
		return fmt.Errorf("open rar: %w", err)
	}
	defer rc.Close()
	for {
		hdr, err := rc.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read rar header: %w", err)
		}
		name := cleanEntryName(hdr.Name)
		if hdr.IsDir {
			if err := s.write(name, hdr.Mode(), true, nil); err != nil {
				return err
			}
			continue
		}
		// rc itself reads the current entry's contents.
		if err := s.write(name, hdr.Mode(), false, rc); err != nil {
			return err
		}
	}
	return nil
}
