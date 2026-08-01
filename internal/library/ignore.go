// Package library holds the filesystem knowledge from spec §5.1: what counts as
// junk, what a pack is, which folders are format variants, and how a file maps to
// a kind.
//
// It is deliberately free of SQL. Every rule in §5.1 is "derived from the actual
// target library and not hypothetical", and §5.1 warns that getting them wrong
// "makes the grid view unusable on day one" — so they need to be testable against
// a fixture tree without a database in the way.
package library

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"
)

// DefaultIgnoreGlobs is the §5.1 junk list.
//
// `__MACOSX` is the one that matters most: it duplicates the entire tree of a
// pack, so failing to exclude it does not merely add noise, it doubles the
// apparent asset count. It is present in the target library at arbitrary depth,
// which is why matching is not anchored to the top level.
// The rest is what a shared network volume accumulates. None of it is the user's
// content and all of it is real: `.Trash-1000` appeared on the target library with
// a deleted pack inside it, which would otherwise have been indexed as a pack —
// deleted files coming back as search results is the opposite of useful.
var DefaultIgnoreGlobs = []string{
	"__MACOSX",
	".DS_Store",
	"Thumbs.db",
	"desktop.ini",
	"._*", // AppleDouble sidecars
	".git",

	// Trash. The freedesktop spec puts a per-user trash at the root of every
	// non-home filesystem, named for the uid — `.Trash-1000` on this library, and a
	// different number on a colleague's machine, so it has to be a glob.
	".Trash-*",
	".Trash",

	// Synology. `@eaDir` is the big one: DSM writes a thumbnail and index directory
	// beside *every* media file, at every depth, so on a NAS it is easily the largest
	// source of noise in the tree. `#recycle` is the share-level recycle bin, which is
	// trash by another name, and `#snapshot` is Btrfs snapshots — the same files again,
	// frozen, which would read as thousands of duplicates (§9.1).
	"@eaDir",
	"#recycle",
	"#snapshot",
	".@__thumb",

	// Windows, which the packs arrive from.
	"$RECYCLE.BIN",
	"System Volume Information",

	// macOS, on network volumes.
	".Spotlight-V100",
	".fseventsd",
	".TemporaryItems",
	".DocumentRevisions-V100",
}

// Matcher decides whether a path component is junk.
type Matcher struct {
	globs []string
}

// NewMatcher compiles a glob list, validating each pattern up front so a typo in
// AMBAR_IGNORE_GLOBS is a startup error rather than a silently-matching-nothing
// rule.
func NewMatcher(globs []string) (*Matcher, error) {
	cleaned := make([]string, 0, len(globs))
	for _, g := range globs {
		g = strings.TrimSpace(g)
		if g == "" {
			continue
		}
		// A trailing slash reads naturally for a directory rule ("__MACOSX/")
		// but path.Match would never match it.
		g = strings.TrimSuffix(g, "/")
		if _, err := path.Match(g, "probe"); err != nil {
			return nil, fmt.Errorf("ignore glob %q is not a valid pattern: %w", g, err)
		}
		cleaned = append(cleaned, g)
	}
	return &Matcher{globs: cleaned}, nil
}

// MustMatcher is NewMatcher for the compiled-in defaults, which cannot fail.
func MustMatcher() *Matcher {
	m, err := NewMatcher(DefaultIgnoreGlobs)
	if err != nil {
		panic("default ignore globs are invalid: " + err.Error())
	}
	return m
}

// Ignored reports whether a single path component — a file or directory name, not
// a path — should be skipped.
//
// Matching is case-insensitive. The junk names come from Windows and macOS, both
// of which have case-insensitive filesystems, so `thumbs.db` and `Thumbs.db` are
// the same file arriving from two machines.
func (m *Matcher) Ignored(name string) bool {
	lower := strings.ToLower(name)
	for _, g := range m.globs {
		if ok, _ := path.Match(strings.ToLower(g), lower); ok {
			return true
		}
	}
	return false
}

// IgnoredPath reports whether any component of a slash-separated relative path is
// junk, which is how a `__MACOSX` directory excludes everything beneath it.
func (m *Matcher) IgnoredPath(relPath string) bool {
	for _, component := range strings.Split(filepath.ToSlash(relPath), "/") {
		if component == "" {
			continue
		}
		if m.Ignored(component) {
			return true
		}
	}
	return false
}

// IsReserved reports whether a directory name is one of the underscore-prefixed
// names §17 reserves: `_inbox`, `_archives`, `_quarantine`, `_trash`.
//
// §17: "Underscore-prefixed directories are reserved and never treated as buckets
// or packs." Checked by prefix rather than against a list, so a future reserved
// directory needs no code change — and so a vendor pack named `_sprites` is
// excluded too, which is the conservative direction: better to leave something
// unindexed than to treat the trash as a pack.
func IsReserved(name string) bool {
	return strings.HasPrefix(name, "_")
}
