package library

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

// DefaultBucketNames are the organisational parents §5.1 says must not be treated
// as packs, plus `audio` from §17's observed library layout.
//
// This is the one place the code recognises the human-facing bucket layout, and it
// exists because pure structure cannot distinguish the two shapes:
//
//	3d/kenney-sci-fi/Models/turret.glb    <- 3d is a bucket, kenney-sci-fi a pack
//	pack/Models/turret.glb                <- pack is the pack, Models a subfolder
//
// Both are "a directory whose children are directories that contain assets". §5.1
// resolves it by naming the buckets, so that is what this does — as configuration
// rather than policy, which keeps §17's "must not depend on this layout" true: if
// the buckets are renamed the list changes, and if they are abandoned entirely the
// packs sit at the top level and are detected directly.
//
// A `.ambar.json` marker always wins over this heuristic, which is why §5.1 lists
// it first and why M4's sidecar writing makes the whole question moot.
var DefaultBucketNames = []string{"2d", "3d", "mix", "raw", "audio"}

// Pack is a detected pack (§5.1).
type Pack struct {
	// Name is the directory's own name, used as the display name.
	Name string
	// Slug is the URL-safe form. §10 uses it for res://assets/<kind>/<pack-slug>/.
	Slug string
	// Kind is "folder" for a real pack directory, "standalone" for the synthetic
	// pack holding loose files that have no pack of their own.
	Kind string
	// RelPath is slash-separated and relative to the library root. "." means the
	// library root itself, which only happens for loose files sitting at the top.
	RelPath string
}

// File is one indexed file.
type File struct {
	// PackRelPath identifies the owning pack.
	PackRelPath string
	// RelPath is slash-separated and relative to the PACK, matching
	// assets.rel_path.
	RelPath  string
	Filename string
	Info     FileInfo
	Size     int64
	ModTime  int64 // Unix seconds
	AbsPath  string
}

// WalkResult is what one library walk produced.
type WalkResult struct {
	Packs []Pack
	Files []File

	// Buckets names the organisational parents that were recursed into rather
	// than treated as packs. Reported so a human can see how the library was
	// interpreted, since a wrong guess here reshapes the whole grid.
	Buckets []string
	// IgnoredCount is how many entries the §5.1 junk rules excluded. Reported by
	// the scan summary because a surprisingly large number usually means a
	// `__MACOSX` tree, which is worth telling the human about.
	IgnoredCount int
	// SkippedNonAssets is how many files were present but are not artwork —
	// readmes, licences, archives, dotfiles. Counted separately from junk because
	// these are legitimate files that simply do not belong in a grid of assets.
	SkippedNonAssets int
	// ReservedSkipped names the underscore-prefixed directories skipped (§17).
	ReservedSkipped []string
	// Errors are per-file problems. §16 wants deliberately broken files handled,
	// and one unreadable file must never abandon a 20,000-file walk.
	Errors []error
}

// WalkOptions configures a walk.
type WalkOptions struct {
	// Root is the absolute, already-validated library root.
	Root string
	// Ignore supplies the junk rules. Defaults to MustMatcher() if nil.
	Ignore *Matcher
	// Buckets overrides DefaultBucketNames. An explicitly empty slice means no
	// bucket recognition at all, which makes every top-level directory a pack.
	Buckets []string
	// ReadDimensions controls whether image headers are read for width/height.
	// Off makes a walk purely metadata-driven, which the 20k-scale test uses.
	ReadDimensions bool
}

// Walk traverses the library and returns the packs and files it found.
//
// Pack detection is §5.1's algorithm. It is depth-agnostic: the library contains
// both `./craftpix-net-695666-.../` and `./raw/craftpix-385863-.../`, so nothing
// may assume a fixed depth. A directory becomes a pack if it is not the root and
// either
//
//  1. it contains a `.ambar.json` sidecar, or
//  2. it contains asset files directly, or
//  3. a majority of its child directories are format/variant folders, or
//  4. it sits at pack level — directly under the library root or under a
//     bucket — and has any indexable file beneath it.
//
// Rule 4 is the addition that makes `3d/kenney-sci-fi/Models/turret.glb` attribute
// to `kenney-sci-fi` rather than to `Models`; see DefaultBucketNames.
//
// The shallowest match wins and everything beneath belongs to it, so packs never
// nest. A candidate that turns out to hold no indexable file is dropped, which is
// how a directory of nothing but junk avoids becoming an empty pack.
func Walk(opts WalkOptions) (*WalkResult, error) {
	if opts.Root == "" {
		return nil, fmt.Errorf("walk: no root given")
	}
	ignore := opts.Ignore
	if ignore == nil {
		ignore = MustMatcher()
	}
	buckets := opts.Buckets
	if buckets == nil {
		buckets = DefaultBucketNames
	}
	isBucketName := func(name string) bool {
		lower := strings.ToLower(name)
		for _, b := range buckets {
			if lower == strings.ToLower(b) {
				return true
			}
		}
		return false
	}

	result := &WalkResult{}

	// §5.1 lists the `.ambar.json` marker first because it must beat every
	// heuristic. Finding it needs a look-ahead: a sidecar can sit deeper than the
	// bucket rule would otherwise place the pack, which happens exactly when
	// someone copies a pack folder somewhere new — §3's "a copied folder carries
	// its own metadata with it". This pre-pass reads no file contents, only
	// directory entries.
	sidecarBelow, err := findSidecarAncestors(opts.Root, ignore)
	if err != nil {
		return nil, err
	}

	var (
		// packOf maps a directory to the pack that owns it, filled in on the way
		// down so no second pass is needed.
		packOf = map[string]string{}
		// candidates holds packs that have been detected but not yet confirmed by
		// finding an asset. A candidate with no assets is never reported.
		candidates = map[string]Pack{}
		// assetCount counts only files IsAssetFile accepts. A directory holding
		// nothing but a README and a licence has no assets in it, so it is neither
		// a pack nor indexed — counting every file would let documentation alone
		// confirm a pack, which rule 4 would then apply to every top-level folder.
		assetCount = map[string]int{}
		isBucket   = map[string]bool{}
		// standaloneOf is kept apart from packOf on purpose. packOf means "this
		// directory is inside a real pack", and the directory branch below inherits
		// from it — so registering the synthetic standalone pack there would make a
		// loose file at the root claim every sibling directory as part of itself.
		//
		// That was a real bug, and an ordering-dependent one: WalkDir visits entries
		// lexically, so a loose `TileSet.png` at the root (uppercase sorts first)
		// collapsed the whole library into one pack, while the same file named
		// `zz.png` left pack detection intact. The target library has exactly such a
		// file.
		standaloneOf = map[string]string{}
	)

	err = filepath.WalkDir(opts.Root, func(absPath string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable directory is reported and skipped, not fatal.
			result.Errors = append(result.Errors, fmt.Errorf("walk %s: %w", absPath, err))
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		relPath, relErr := filepath.Rel(opts.Root, absPath)
		if relErr != nil {
			result.Errors = append(result.Errors, fmt.Errorf("relativise %s: %w", absPath, relErr))
			return nil
		}
		relPath = filepath.ToSlash(relPath)

		if d.IsDir() {
			if relPath == "." {
				return nil
			}
			name := d.Name()

			// §5.1 junk. Skipping the directory excludes its whole subtree, which
			// is the point for `__MACOSX`.
			if ignore.Ignored(name) {
				result.IgnoredCount++
				return fs.SkipDir
			}
			// §17: underscore-prefixed directories are reserved. Checked only at
			// the top level, where _inbox/_trash/_archives/_quarantine live —
			// deeper down, a vendor folder called `_parts` is real content.
			if !strings.Contains(relPath, "/") && IsReserved(name) {
				result.ReservedSkipped = append(result.ReservedSkipped, relPath)
				return fs.SkipDir
			}
			// WalkDir does not follow symlinks; this stops a symlinked directory
			// being reported as content in its own right.
			if d.Type()&os.ModeSymlink != 0 {
				return nil
			}

			parent := parentDir(relPath)

			// Everything under a pack belongs to that pack.
			if owner, ok := packOf[parent]; ok {
				packOf[relPath] = owner
				return nil
			}

			// Pack level: directly under the root, or directly under a bucket.
			atPackLevel := parent == "." || isBucket[parent]

			// A bucket is organisational, never a pack, and only recognised at the
			// top level so a pack legitimately named "3d" deeper in still works.
			if parent == "." && isBucketName(name) {
				isBucket[relPath] = true
				result.Buckets = append(result.Buckets, relPath)
				return nil
			}

			// A directory with a sidecar of its own is a pack, full stop.
			// A directory with a sidecar somewhere beneath it is never a pack, so
			// the marker deeper down wins over both the format-folder heuristic
			// and the bucket rule.
			switch {
			case hasSidecar(absPath):
			case sidecarBelow[relPath]:
				return nil
			case isPack(absPath, ignore) || atPackLevel:
			default:
				return nil
			}

			pack := newPack(relPath, "folder")
			candidates[pack.RelPath] = pack
			packOf[relPath] = pack.RelPath
			return nil
		}

		// --- a file ---

		name := d.Name()
		if ignore.Ignored(name) {
			result.IgnoredCount++
			return nil
		}
		// Not artwork, so not indexed: readmes, licences, PDFs, downloaded archives,
		// and dotfiles like .gitkeep or .gdignore. This check used to gate only pack
		// *confirmation* further down, which is why the grid filled up with
		// documentation — a real bug, visible on the first real library it met.
		//
		// The sidecar is excluded by the same rule (it is a dotfile): metadata about
		// assets, read by §3's importer rather than indexed as one.
		if !IsAssetFile(name) {
			result.SkippedNonAssets++
			return nil
		}
		// Symlinked files are skipped: the target is either already indexed under
		// its real path, or outside the root where safepath would refuse it.
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}

		dir := parentDir(relPath)
		owner, ok := packOf[dir]
		if !ok {
			owner, ok = standaloneOf[dir]
		}
		if !ok {
			// No pack ancestor: a loose file at the library root or directly inside
			// a bucket. §5.1 forbids treating a bucket as a pack, and §4 provides
			// `standalone` for exactly this. Recorded in standaloneOf rather than
			// packOf so it owns the loose files and nothing else.
			pack := newPack(dir, "standalone")
			candidates[pack.RelPath] = pack
			standaloneOf[dir] = pack.RelPath
			owner = pack.RelPath
		}

		info, statErr := d.Info()
		if statErr != nil {
			result.Errors = append(result.Errors, fmt.Errorf("stat %s: %w", relPath, statErr))
			return nil
		}

		fileInfo := FileInfo{Kind: KindForExt(Ext(name)), Ext: Ext(name)}
		if opts.ReadDimensions {
			fileInfo = Classify(absPath)
		}

		// Only real assets reach this point, so every one of them confirms its pack.
		assetCount[owner]++
		result.Files = append(result.Files, File{
			PackRelPath: owner,
			RelPath:     relativeTo(owner, relPath),
			Filename:    name,
			Info:        fileInfo,
			Size:        info.Size(),
			ModTime:     info.ModTime().Unix(),
			AbsPath:     absPath,
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", opts.Root, err)
	}

	// Only packs holding at least one asset are real. A directory of nothing but
	// junk or documentation is not a pack, and its files are dropped with it —
	// there is nothing there worth indexing.
	confirmed := make(map[string]bool, len(candidates))
	for relPath, pack := range candidates {
		if assetCount[relPath] > 0 {
			confirmed[relPath] = true
			result.Packs = append(result.Packs, pack)
		}
	}
	kept := result.Files[:0]
	for _, f := range result.Files {
		if confirmed[f.PackRelPath] {
			kept = append(kept, f)
		}
	}
	result.Files = kept

	// Stable output makes scan summaries and test assertions deterministic.
	sort.Slice(result.Packs, func(i, j int) bool {
		return result.Packs[i].RelPath < result.Packs[j].RelPath
	})
	sort.Slice(result.Files, func(i, j int) bool {
		if result.Files[i].PackRelPath != result.Files[j].PackRelPath {
			return result.Files[i].PackRelPath < result.Files[j].PackRelPath
		}
		return result.Files[i].RelPath < result.Files[j].RelPath
	})
	sort.Strings(result.Buckets)
	return result, nil
}

// SidecarName is the per-pack metadata file from §3.
const SidecarName = ".ambar.json"

// hasSidecar reports whether a directory carries its own sidecar.
func hasSidecar(absDir string) bool {
	info, err := os.Lstat(filepath.Join(absDir, SidecarName))
	return err == nil && info.Mode().IsRegular()
}

// findSidecarAncestors returns the set of directories that have a sidecar
// somewhere strictly beneath them.
//
// A directory in this set is never a pack: something deeper is, and §5.1 makes the
// marker authoritative over every heuristic.
func findSidecarAncestors(root string, ignore *Matcher) (map[string]bool, error) {
	below := map[string]bool{}

	err := filepath.WalkDir(root, func(absPath string, d fs.DirEntry, err error) error {
		if err != nil {
			// Unreadable directories are the main walk's problem to report; here
			// they just mean no sidecar was found beneath them.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			rel, relErr := filepath.Rel(root, absPath)
			if relErr != nil {
				return nil
			}
			if rel == "." {
				return nil
			}
			if ignore.Ignored(name) {
				return fs.SkipDir
			}
			if !strings.Contains(filepath.ToSlash(rel), "/") && IsReserved(name) {
				return fs.SkipDir
			}
			return nil
		}
		if name != SidecarName {
			return nil
		}

		// Mark every ancestor, excluding the sidecar's own directory.
		rel, relErr := filepath.Rel(root, filepath.Dir(absPath))
		if relErr != nil {
			return nil
		}
		for dir := parentDir(filepath.ToSlash(rel)); ; dir = parentDir(dir) {
			if dir == "." || dir == "" {
				break
			}
			below[dir] = true
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan for sidecars under %s: %w", root, err)
	}
	return below, nil
}

// isPack applies §5.1's first three tests to one directory. Rule 4 is positional
// and lives in Walk, which is the only place that knows about buckets.
func isPack(absDir string, ignore *Matcher) bool {
	entries, err := os.ReadDir(absDir)
	if err != nil {
		return false
	}

	var (
		hasAssetFile  bool
		subdirs       int
		formatSubdirs int
	)
	for _, e := range entries {
		name := e.Name()
		if ignore.Ignored(name) {
			continue
		}
		// Test 1: an explicit sidecar is the strongest signal and needs no
		// heuristics at all.
		if !e.IsDir() && name == ".ambar.json" {
			return true
		}
		if e.IsDir() {
			subdirs++
			if IsFormatFolder(name) {
				formatSubdirs++
			}
			continue
		}
		if IsAssetFile(name) {
			hasAssetFile = true
		}
	}

	// Test 2: contains asset files directly.
	if hasAssetFile {
		return true
	}

	// Test 3: its children are recognisable format/variant folders. A majority
	// rather than all of them, because real packs mix `PNG/` and `PSD/` with a
	// `Preview/` or `Docs/` folder and one such sibling should not stop the parent
	// being recognised.
	return subdirs > 0 && formatSubdirs*2 > subdirs
}

// newPack builds a Pack from a library-relative directory path.
func newPack(relPath, kind string) Pack {
	name := baseName(relPath)
	return Pack{
		Name:    name,
		Slug:    Slugify(name),
		Kind:    kind,
		RelPath: relPath,
	}
}

// baseName is filepath.Base for slash paths, with "." mapped to a usable name.
// The library root as a pack only happens for loose files at the top level.
func baseName(relPath string) string {
	if relPath == "." || relPath == "" {
		return "library-root"
	}
	if i := strings.LastIndex(relPath, "/"); i >= 0 {
		return relPath[i+1:]
	}
	return relPath
}

// parentDir returns the slash-path parent, with "." for a top-level entry.
func parentDir(relPath string) string {
	if i := strings.LastIndex(relPath, "/"); i >= 0 {
		return relPath[:i]
	}
	return "."
}

// relativeTo re-bases a library-relative path onto its pack.
func relativeTo(packRelPath, fileRelPath string) string {
	if packRelPath == "." || packRelPath == "" {
		return fileRelPath
	}
	if trimmed := strings.TrimPrefix(fileRelPath, packRelPath+"/"); trimmed != fileRelPath {
		return trimmed
	}
	return fileRelPath
}

// Slugify produces a URL-safe identifier from a directory name.
//
// §10 builds `res://assets/<kind>/<pack-slug>/` from this, so it has to be safe as
// a single path segment: lowercase, ASCII alphanumerics and dashes only.
func Slugify(name string) string {
	var b strings.Builder
	lastDash := true // suppresses a leading dash
	for _, r := range strings.ToLower(name) {
		switch {
		case r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			b.WriteRune(r)
			lastDash = false
		case !lastDash:
			b.WriteByte('-')
			lastDash = true
		}
	}
	slug := strings.Trim(b.String(), "-")
	if slug == "" {
		// A name of nothing but non-ASCII characters still needs an identifier.
		return "pack"
	}
	return slug
}
