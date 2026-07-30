package removal

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/datcal/ambar/internal/safepath"
)

// §9.1 "Prefer linking over deleting": for exact duplicates, deletion is not the
// only way to reclaim the space, and it is the only irreversible one. Replacing a
// redundant copy with a link keeps every path in the library valid and working
// while the bytes are shared once.
//
// Two mechanisms, selected by AMBAR_DEDUPE_LINK_MODE:
//
//   - reflink — a btrfs/XFS copy-on-write clone. The best option: space is shared,
//     but editing one copy transparently diverges it from the other, so the
//     "originals are never modified" invariant is not quietly weakened. Synology
//     DSM 7 volumes are commonly btrfs.
//   - hardlink — works everywhere, reclaims the same space, but the two paths
//     genuinely become one inode. Acceptable here only because originals are never
//     edited, and the UI says so.
//
// Neither is ever triggered on its own; both need an explicit human selection, the
// same as a removal.
var errLinkModeOff = errors.New("linking is disabled (AMBAR_DEDUPE_LINK_MODE=off)")

// link replaces the redundant copy with a link to the kept copy.
//
// The content of both files is re-hashed first. The Planner already checked that
// the index says they are identical, but the index can be stale, and this is the
// one operation in Ambar that writes over an existing library path: if the bytes
// have diverged since the last scan, linking would silently replace one file's
// content with another's. Reading both files is cheap next to that risk.
func (e *Executor) link(entry *Entry) error {
	if e.linkMode == "off" {
		return errLinkModeOff
	}
	if entry.Root != RootLibrary {
		return errors.New("only library files can be linked")
	}

	redundant, err := safepath.ResolveExisting(e.libraryRoot, entry.Path)
	if err != nil {
		return err
	}
	kept, err := safepath.ResolveExisting(e.libraryRoot, entry.KeepPath)
	if err != nil {
		return err
	}
	if redundant == kept {
		return errors.New("a file cannot be linked to itself")
	}

	redundantInfo, err := os.Lstat(redundant)
	if err != nil {
		return err
	}
	keptInfo, err := os.Lstat(kept)
	if err != nil {
		return err
	}
	if !redundantInfo.Mode().IsRegular() || !keptInfo.Mode().IsRegular() {
		return errors.New("only regular files can be linked")
	}
	if redundantInfo.Size() != keptInfo.Size() {
		return fmt.Errorf("sizes differ (%d vs %d); refusing to link", redundantInfo.Size(), keptInfo.Size())
	}
	if os.SameFile(redundantInfo, keptInfo) {
		// Already one inode — a repeat of a hardlink dedupe that already ran. Not an
		// error, and nothing to do.
		return nil
	}

	redundantSum, err := hashFile(redundant)
	if err != nil {
		return err
	}
	keptSum, err := hashFile(kept)
	if err != nil {
		return err
	}
	if redundantSum != keptSum {
		return fmt.Errorf("content differs on disk (%s vs %s); refusing to link",
			shortSha(redundantSum), shortSha(keptSum))
	}

	// Build the replacement beside the original and rename it into place, so the
	// path is never briefly missing and a failure leaves the original untouched.
	tmp, err := os.CreateTemp(filepath.Dir(redundant), ".ambar-link-*")
	if err != nil {
		return fmt.Errorf("create link placeholder: %w", err)
	}
	tmpName := tmp.Name()
	tmp.Close()              //nolint:errcheck
	defer os.Remove(tmpName) //nolint:errcheck // no-op once renamed

	switch e.linkMode {
	case "reflink":
		if err := os.Remove(tmpName); err != nil {
			return err
		}
		if err := cloneFile(kept, tmpName, redundantInfo.Mode().Perm()); err != nil {
			return fmt.Errorf("reflink: %w", err)
		}
	case "hardlink":
		if err := os.Remove(tmpName); err != nil {
			return err
		}
		if err := os.Link(kept, tmpName); err != nil {
			return fmt.Errorf("hardlink: %w", err)
		}
	default:
		return fmt.Errorf("unknown link mode %q", e.linkMode)
	}

	// Keep the original's modification time so the next scan does not read the link
	// as a content change at a stable path (§12).
	if err := os.Chtimes(tmpName, redundantInfo.ModTime(), redundantInfo.ModTime()); err != nil {
		e.log.Warn("could not preserve mtime on linked file", "path", entry.Path, "error", err)
	}
	if err := os.Rename(tmpName, redundant); err != nil {
		return fmt.Errorf("replace with link: %w", err)
	}
	return nil
}

// LinkSupport is what the health endpoint reports (§9.1: "probe support at startup
// and report it in the health endpoint").
type LinkSupport struct {
	Mode string `json:"mode"`
	// OK is whether the configured mode actually works on this filesystem.
	OK bool `json:"ok"`
	// Detail explains a false OK, or names the fallback.
	Detail string `json:"detail,omitempty"`
}

// ProbeLinkSupport tests the configured link mode against the library filesystem
// by cloning a two-byte temporary file. It writes only inside dir and removes both
// files before returning.
//
// A failure here is not a startup error: linking is an optional way to reclaim
// space, and a library on ext4 simply cannot reflink. The answer is reported so the
// UI can recommend hardlinking or removal instead of offering an action that will
// fail at the last step.
func ProbeLinkSupport(mode, dir string) LinkSupport {
	support := LinkSupport{Mode: mode}
	switch mode {
	case "off":
		support.Detail = "linking is disabled; dedupe falls back to trash-based removal"
		return support
	case "reflink", "hardlink":
	default:
		support.Detail = fmt.Sprintf("unknown mode %q", mode)
		return support
	}

	src, err := os.CreateTemp(dir, ".ambar-linkprobe-src-*")
	if err != nil {
		support.Detail = fmt.Sprintf("cannot write to %s: %v", dir, err)
		return support
	}
	srcName := src.Name()
	defer os.Remove(srcName) //nolint:errcheck
	if _, err := src.Write([]byte("ok")); err != nil {
		src.Close() //nolint:errcheck
		support.Detail = err.Error()
		return support
	}
	if err := src.Close(); err != nil {
		support.Detail = err.Error()
		return support
	}

	destName := srcName + "-dest"
	defer os.Remove(destName) //nolint:errcheck

	if mode == "hardlink" {
		if err := os.Link(srcName, destName); err != nil {
			support.Detail = fmt.Sprintf("hardlinks are not supported here: %v", err)
			return support
		}
		support.OK = true
		support.Detail = "hardlinks work; note that linked copies become one inode"
		return support
	}

	if err := cloneFile(srcName, destName, 0o644); err != nil {
		support.Detail = fmt.Sprintf("reflinks are not supported here (%v); "+
			"set AMBAR_DEDUPE_LINK_MODE=hardlink or off", err)
		return support
	}
	support.OK = true
	support.Detail = "copy-on-write reflinks work; editing one copy diverges it from the other"
	return support
}

// hashFile is the same sha256 the indexer computes, recomputed at the moment of
// the write rather than trusted from the index.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close() //nolint:errcheck

	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}
