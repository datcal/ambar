package library

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// IgnoreFileName is the per-folder hide list: a `.ambarignore` in any directory of the
// library names files and folders beneath it that should not be indexed.
//
// The junk list (DefaultIgnoreGlobs, AMBAR_IGNORE_GLOBS) answers "what is never
// anybody's content". This answers a different question — "what in *this* folder do I
// not want to see" — and that belongs beside the folder rather than in the server's
// environment, for the same reason the sidecar does: invariant 2 makes the filesystem
// the source of truth, and a copied folder should carry its own rules with it.
//
// The syntax is the part of .gitignore people actually write:
//
//	# a comment
//	*.unitypackage      a name, at any depth below this folder
//	Unreal/             a trailing slash: directories only
//	Source/Previews     a slash elsewhere: a path relative to this folder
//	**/Demo_Models      ** stands for any number of folders
//
// Matching is case-insensitive, like the junk list, because the packs arrive from
// Windows and macOS. Negation (`!`) is not supported; a line using it is reported and
// skipped rather than half-honoured.
const IgnoreFileName = ".ambarignore"

// ignoreRule is one line of a .ambarignore.
type ignoreRule struct {
	// segments is the pattern split on "/", lowercased. An unanchored rule has exactly
	// one segment, matched against the last component of a path.
	segments []string
	anchored bool
	dirOnly  bool
}

// matches reports whether rel — slash-separated and relative to the directory holding
// the ignore file — is hidden by this rule.
func (r ignoreRule) matches(rel string, isDir bool) bool {
	if r.dirOnly && !isDir {
		return false
	}
	rel = strings.ToLower(rel)
	if !r.anchored {
		ok, _ := path.Match(r.segments[0], path.Base(rel))
		return ok
	}
	return matchSegments(r.segments, strings.Split(rel, "/"))
}

// matchSegments matches a pattern against a path one component at a time, with `**`
// standing for zero or more whole components.
func matchSegments(pattern, names []string) bool {
	for len(pattern) > 0 {
		if pattern[0] == "**" {
			for i := 0; i <= len(names); i++ {
				if matchSegments(pattern[1:], names[i:]) {
					return true
				}
			}
			return false
		}
		if len(names) == 0 {
			return false
		}
		if ok, _ := path.Match(pattern[0], names[0]); !ok {
			return false
		}
		pattern, names = pattern[1:], names[1:]
	}
	return len(names) == 0
}

// parseIgnoreLine turns one line into a rule. ok is false for a blank line or a comment.
func parseIgnoreLine(line string) (rule ignoreRule, ok bool, err error) {
	// Trimmed on both sides: a file edited on Windows ends every line in \r, and a
	// stray space after a folder name is never what anyone meant.
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return ignoreRule{}, false, nil
	}
	if strings.HasPrefix(line, "!") {
		return ignoreRule{}, false, errors.New("negation (!) is not supported")
	}

	pattern := strings.ToLower(filepath.ToSlash(line))
	if strings.HasSuffix(pattern, "/") {
		rule.dirOnly = true
		pattern = strings.TrimRight(pattern, "/")
	}
	// A slash anywhere but the end anchors the pattern to this folder, as in .gitignore.
	if strings.Contains(pattern, "/") {
		rule.anchored = true
		pattern = strings.TrimLeft(pattern, "/")
	}
	if pattern == "" {
		return ignoreRule{}, false, errors.New("an empty pattern would hide the whole folder")
	}

	for _, segment := range strings.Split(pattern, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return ignoreRule{}, false, fmt.Errorf("%q is not a usable path segment", segment)
		}
		if _, err := path.Match(segment, "probe"); err != nil {
			return ignoreRule{}, false, fmt.Errorf("not a valid pattern: %w", err)
		}
		rule.segments = append(rule.segments, segment)
	}
	return rule, true, nil
}

// IgnoreFiles holds the .ambarignore files one walk has read, keyed by the
// library-relative directory they sit in.
//
// Loaded lazily as the walk enters each directory, so a folder's rules are known before
// anything inside it is considered, and a hidden folder's own ignore file is never read
// at all — nothing beneath it is visited.
type IgnoreFiles struct {
	root string
	// files maps a directory to its rules. A directory that was looked at and has no
	// ignore file maps to nil, so it is not stat'ed twice.
	files map[string][]ignoreRule
	// errs are the lines that could not be used, reported through the walk's errors so
	// a typo is visible in the scan summary instead of silently matching nothing.
	errs []error
}

func newIgnoreFiles(root string) *IgnoreFiles {
	return &IgnoreFiles{root: root, files: map[string][]ignoreRule{}}
}

// load reads relDir's ignore file, once. relDir is "." for the library root.
func (s *IgnoreFiles) load(relDir string) {
	if _, done := s.files[relDir]; done {
		return
	}
	s.files[relDir] = nil

	absPath := filepath.Join(s.root, filepath.FromSlash(relDir), IgnoreFileName)
	f, err := os.Open(absPath)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			s.errs = append(s.errs, fmt.Errorf("read %s: %w", s.display(relDir), err))
		}
		return
	}
	defer f.Close()

	var rules []ignoreRule
	lines := bufio.NewScanner(f)
	for n := 1; lines.Scan(); n++ {
		rule, ok, err := parseIgnoreLine(lines.Text())
		if err != nil {
			s.errs = append(s.errs, fmt.Errorf("%s line %d: %w; the line was skipped",
				s.display(relDir), n, err))
			continue
		}
		if ok {
			rules = append(rules, rule)
		}
	}
	if err := lines.Err(); err != nil {
		s.errs = append(s.errs, fmt.Errorf("read %s: %w", s.display(relDir), err))
	}
	s.files[relDir] = rules
}

func (s *IgnoreFiles) display(relDir string) string {
	if relDir == "." {
		return IgnoreFileName
	}
	return relDir + "/" + IgnoreFileName
}

// hidden reports whether the entry at relPath is hidden by the ignore file of any
// directory above it. Only that one entry is tested: the walk never descends into a
// hidden directory, so its ancestors have already passed.
func (s *IgnoreFiles) hidden(relPath string, isDir bool) bool {
	if s == nil {
		return false
	}
	dir := parentDir(relPath)
	for {
		if rules := s.files[dir]; len(rules) > 0 {
			rel := relPath
			if dir != "." {
				rel = strings.TrimPrefix(relPath, dir+"/")
			}
			for _, r := range rules {
				if r.matches(rel, isDir) {
					return true
				}
			}
		}
		if dir == "." {
			return false
		}
		dir = parentDir(dir)
	}
}

// HiddenPath reports whether a library-relative file path, or any directory on the way
// to it, is hidden by an ignore file this walk read. It is what the scan asks about a
// row that has stopped appearing in the walk.
func (s *IgnoreFiles) HiddenPath(relPath string) bool {
	if s == nil {
		return false
	}
	parts := strings.Split(filepath.ToSlash(relPath), "/")
	for i := range parts {
		isDir := i < len(parts)-1
		if s.hidden(strings.Join(parts[:i+1], "/"), isDir) {
			return true
		}
	}
	return false
}
