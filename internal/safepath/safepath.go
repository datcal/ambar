// Package safepath contains the path-traversal defence required by CLAUDE.md
// invariant 9 and spec §11: never build a filesystem path from user input
// without resolving it and confirming it stays under the configured root.
//
// The rule this package exists to enforce is that *no other package* joins a
// root with an untrusted path segment itself. Everything that touches the
// library — the scan walker, the download handler, and later the ingest
// extractor and the derivative writer — goes through Resolve.
//
// Every function returns an error rather than a best-effort path. A caller that
// gets an error must abandon the operation; there is no partially-safe result to
// fall back on.
package safepath

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrEscapes means the resolved path is outside the root. It is the error every
// traversal attempt produces, whatever form the attempt took.
var ErrEscapes = errors.New("path escapes the root directory")

// ErrUnsafeInput means the input was rejected before the filesystem was
// consulted at all — a NUL byte, an absolute path, a Windows drive prefix.
var ErrUnsafeInput = errors.New("unsafe path input")

// Resolve joins an untrusted relative path onto root and returns the absolute
// result, guaranteed to be inside root.
//
// It resolves symlinks, so a link inside the library pointing at /etc/passwd is
// rejected rather than followed. The target need not exist: a path whose parent
// directories are safe is accepted so callers can create files (the sidecars of
// §3, the derivatives of §6). Only the parts that do exist are link-resolved,
// which is the only way to check a path that is about to be created.
func Resolve(root, untrusted string) (string, error) {
	cleanRoot, err := resolveRoot(root)
	if err != nil {
		return "", err
	}
	if err := checkInput(untrusted); err != nil {
		return "", err
	}

	// filepath.Join cleans, which collapses any ".." that stays within the
	// string. That alone is not enough — Join("/lib", "../etc") gives "/etc",
	// which is exactly why the containment check below is not optional.
	candidate := filepath.Join(cleanRoot, filepath.FromSlash(untrusted))

	resolved, err := resolveExistingPrefix(candidate)
	if err != nil {
		return "", err
	}
	if !within(cleanRoot, resolved) {
		return "", fmt.Errorf("%w: %q resolves to %q, outside %q",
			ErrEscapes, untrusted, resolved, cleanRoot)
	}
	return resolved, nil
}

// ResolveExisting is Resolve for a path that must already exist. Use it when
// reading; use Resolve when the target may be about to be created.
func ResolveExisting(root, untrusted string) (string, error) {
	resolved, err := Resolve(root, untrusted)
	if err != nil {
		return "", err
	}
	if _, err := os.Lstat(resolved); err != nil {
		return "", fmt.Errorf("resolve %q under %q: %w", untrusted, root, err)
	}
	return resolved, nil
}

// RelUnder is the reverse direction: given an absolute path known to be inside
// root, return the slash-separated relative path to store in the database.
//
// The stored form always uses forward slashes regardless of host separator, so
// an index built on one platform reads correctly on another.
func RelUnder(root, abs string) (string, error) {
	cleanRoot, err := resolveRoot(root)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(abs) {
		return "", fmt.Errorf("%w: %q is not absolute", ErrUnsafeInput, abs)
	}

	// Resolve both sides before comparing: on macOS /var is a symlink to
	// /private/var, so an unresolved root and a resolved path never match.
	resolved, err := resolveExistingPrefix(filepath.Clean(abs))
	if err != nil {
		return "", err
	}
	if !within(cleanRoot, resolved) {
		return "", fmt.Errorf("%w: %q is not under %q", ErrEscapes, resolved, cleanRoot)
	}

	rel, err := filepath.Rel(cleanRoot, resolved)
	if err != nil {
		return "", fmt.Errorf("relativise %q against %q: %w", resolved, cleanRoot, err)
	}
	if rel == "." {
		return "", fmt.Errorf("%w: %q is the root itself", ErrUnsafeInput, abs)
	}
	return filepath.ToSlash(rel), nil
}

// IsWithin reports whether abs is inside root. Both are resolved first. It is
// the cheap check for a path already obtained from a trusted source; prefer
// Resolve when the path came from outside.
func IsWithin(root, abs string) bool {
	cleanRoot, err := resolveRoot(root)
	if err != nil {
		return false
	}
	resolved, err := resolveExistingPrefix(filepath.Clean(abs))
	if err != nil {
		return false
	}
	return within(cleanRoot, resolved)
}

// checkInput rejects what can be rejected without touching the filesystem.
func checkInput(untrusted string) error {
	if untrusted == "" {
		return fmt.Errorf("%w: empty path", ErrUnsafeInput)
	}
	// A NUL truncates the path at the syscall boundary, so "safe.png\x00../..".
	if strings.ContainsRune(untrusted, 0) {
		return fmt.Errorf("%w: path contains a NUL byte", ErrUnsafeInput)
	}
	// An absolute input would make Join discard the root entirely on some
	// inputs, and in any case means the caller is confused about the contract.
	if filepath.IsAbs(untrusted) || strings.HasPrefix(untrusted, "/") {
		return fmt.Errorf("%w: %q is absolute; pass a path relative to the root", ErrUnsafeInput, untrusted)
	}
	// Backslashes and drive prefixes come from archives written on Windows
	// (§5 step 2 requires sanitising exactly these). On Linux a backslash is a
	// legal filename character, so this is a deliberate restriction rather than
	// a correctness fix: a library path containing one is refused rather than
	// silently reinterpreted.
	if strings.Contains(untrusted, `\`) {
		return fmt.Errorf("%w: %q contains a backslash", ErrUnsafeInput, untrusted)
	}
	if hasDrivePrefix(untrusted) {
		return fmt.Errorf("%w: %q has a Windows drive prefix", ErrUnsafeInput, untrusted)
	}
	return nil
}

// hasDrivePrefix matches "C:" and "c:/..." style prefixes.
func hasDrivePrefix(p string) bool {
	if len(p) < 2 || p[1] != ':' {
		return false
	}
	c := p[0]
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// resolveRoot cleans and link-resolves the configured root once.
func resolveRoot(root string) (string, error) {
	if root == "" {
		return "", fmt.Errorf("%w: empty root", ErrUnsafeInput)
	}
	if !filepath.IsAbs(root) {
		return "", fmt.Errorf("%w: root %q is not absolute", ErrUnsafeInput, root)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(root))
	if err != nil {
		// A missing root is a configuration problem, and config.Load already
		// refuses to start on one. Reaching here means it vanished at runtime.
		return "", fmt.Errorf("resolve root %q: %w", root, err)
	}
	return resolved, nil
}

// resolveExistingPrefix link-resolves the longest existing prefix of path and
// re-appends the rest.
//
// EvalSymlinks fails outright on a non-existent path, which would make it
// impossible to check a file that is about to be created. Walking up to the
// deepest existing ancestor, resolving that, and re-attaching the remainder
// gives a checkable answer either way — and because the remainder cannot contain
// a symlink (it does not exist), nothing is missed.
func resolveExistingPrefix(path string) (string, error) {
	remainder := ""
	current := path

	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			if remainder == "" {
				return resolved, nil
			}
			return filepath.Join(resolved, remainder), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			// ELOOP for a symlink cycle lands here, which is the right answer:
			// unresolvable means unsafe.
			return "", fmt.Errorf("resolve %q: %w", path, err)
		}

		parent := filepath.Dir(current)
		if parent == current {
			// Walked to the filesystem root without finding anything that
			// exists. Cannot happen for a path built on a validated root.
			return "", fmt.Errorf("resolve %q: no existing ancestor", path)
		}
		remainder = filepath.Join(filepath.Base(current), remainder)
		current = parent
	}
}

// within reports whether path is root or is inside it.
//
// The separator check is what stops the classic prefix bug: "/library-backup"
// has "/library" as a string prefix but is a different directory.
func within(root, path string) bool {
	if path == root {
		return true
	}
	return strings.HasPrefix(path, root+string(os.PathSeparator))
}
