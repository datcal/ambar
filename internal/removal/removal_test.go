package removal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/datcal/ambar/internal/db"
)

// fixture is a library tree plus the index rows that describe it. Every test here
// works against real files, because the whole point of this package is what it does
// to a filesystem.
type fixture struct {
	t           *testing.T
	db          *db.DB
	libraryRoot string
	dataRoot    string
	trashDir    string
	planner     *Planner
	exec        *Executor
	packID      int64
	now         time.Time
}

// packPrefix is the one pack every fixture asset lives in, so a library-relative
// path is always "pack/<rel>".
const packPrefix = "pack"

func newFixture(t *testing.T) *fixture {
	t.Helper()

	base := t.TempDir()
	f := &fixture{
		t:           t,
		libraryRoot: filepath.Join(base, "library"),
		dataRoot:    filepath.Join(base, "data"),
		now:         time.Unix(1_700_000_000, 0),
	}
	f.trashDir = filepath.Join(f.libraryRoot, "_trash")
	for _, dir := range []string{f.libraryRoot, f.dataRoot, filepath.Join(f.libraryRoot, packPrefix)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	database, err := db.Open(filepath.Join(f.dataRoot, "ambar.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	if _, err := database.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.db = database

	unix := f.now.Unix()
	res, err := database.Writer.Exec(`
		INSERT INTO packs (name, slug, kind, library_rel_path, first_seen_at, last_seen_at, created_at, updated_at)
		VALUES ('Pack', 'pack', 'folder', ?, ?, ?, ?, ?)`, packPrefix, unix, unix, unix, unix)
	if err != nil {
		t.Fatal(err)
	}
	if f.packID, err = res.LastInsertId(); err != nil {
		t.Fatal(err)
	}

	f.planner = NewPlanner(database, f.libraryRoot, f.dataRoot, f.trashDir)
	f.exec = NewExecutor(database, f.libraryRoot, f.dataRoot, f.trashDir, "hardlink", nil, nil).
		WithClock(func() time.Time { return f.now })
	return f
}

// writeAsset creates a file inside the pack and indexes it. relPath is
// pack-relative; the returned id is the asset row.
func (f *fixture) writeAsset(relPath, content string) int64 {
	f.t.Helper()
	f.writeFile(packPrefix+"/"+relPath, content)

	sum := sha256.Sum256([]byte(content))
	unix := f.now.Unix()
	res, err := f.db.Writer.Exec(`
		INSERT INTO assets (pack_id, rel_path, filename, ext, kind, size, mtime, sha256,
		                    first_seen_at, last_verified_at, created_at, updated_at)
		VALUES (?, ?, ?, 'png', 'image', ?, ?, ?, ?, ?, ?, ?)`,
		f.packID, relPath, filepath.Base(relPath), len(content), unix,
		hex.EncodeToString(sum[:]), unix, unix, unix, unix)
	if err != nil {
		f.t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		f.t.Fatal(err)
	}
	return id
}

// writeFile creates a library file without indexing it — junk, as far as the
// index is concerned.
func (f *fixture) writeFile(relPath, content string) string {
	f.t.Helper()
	abs := filepath.Join(f.libraryRoot, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
	return abs
}

// useInProject records an active project_uses row (invariant 5).
func (f *fixture) useInProject(assetID int64, projectName string) {
	f.t.Helper()
	unix := f.now.Unix()
	res, err := f.db.Writer.Exec(`
		INSERT INTO projects (uuid, name, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		"uuid-"+projectName, projectName, unix, unix)
	if err != nil {
		f.t.Fatal(err)
	}
	projectID, _ := res.LastInsertId()
	if _, err := f.db.Writer.Exec(`
		INSERT INTO project_uses (project_id, asset_id, res_path, asset_sha256, added_at)
		VALUES (?, ?, ?, '', ?)`, projectID, assetID, "res://assets/x.png", unix); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) exists(relPath string) bool {
	f.t.Helper()
	_, err := os.Lstat(filepath.Join(f.libraryRoot, filepath.FromSlash(relPath)))
	return err == nil
}

func (f *fixture) missingSince(assetID int64) *int64 {
	f.t.Helper()
	var at *int64
	if err := f.db.Reader.QueryRow(`SELECT missing_since FROM assets WHERE id = ?`, assetID).Scan(&at); err != nil {
		f.t.Fatal(err)
	}
	return at
}

// plan is Plan with the boilerplate removed.
func (f *fixture) plan(targets ...Target) *Plan {
	f.t.Helper()
	p, err := f.planner.Plan(context.Background(), "test", targets)
	if err != nil {
		f.t.Fatalf("plan: %v", err)
	}
	return p
}

// trashTarget is the common case: remove one library path.
func trashTarget(path string) Target {
	return Target{Root: RootLibrary, Path: path, Action: ActionTrash, Finding: "test"}
}

// blockedReason returns the reason recorded for a path, or "" if it was allowed.
func blockedReason(p *Plan, path string) string {
	for _, b := range p.Blocked {
		if b.Path == path {
			return b.Reason
		}
	}
	return ""
}

func allowed(p *Plan, path string) bool {
	for _, op := range p.Ops {
		if op.Path == path {
			return true
		}
	}
	return false
}

// --- invariant 5: anything a project uses is never a candidate ---------------

func TestPlanBlocksAssetUsedByProject(t *testing.T) {
	f := newFixture(t)
	id := f.writeAsset("hero.png", "hero-bytes")
	f.writeAsset("hero-copy.png", "hero-bytes") // a second copy, so invariant 4 is not what blocks
	f.useInProject(id, "Dungeon Game")

	p := f.plan(trashTarget(packPrefix + "/hero.png"))

	reason := blockedReason(p, packPrefix+"/hero.png")
	if reason == "" {
		t.Fatalf("an asset used by a project must be blocked; plan = %+v", p)
	}
	if !strings.Contains(reason, "Dungeon Game") {
		t.Errorf("the block reason must name the project, got %q", reason)
	}
	if len(p.Ops) != 0 {
		t.Errorf("nothing may be planned, got %d ops", len(p.Ops))
	}
}

func TestPlanBlocksDirectoryContainingAProjectUsedAsset(t *testing.T) {
	f := newFixture(t)
	id := f.writeAsset("sprites/hero.png", "hero-bytes")
	f.writeAsset("sprites-copy/hero.png", "hero-bytes")
	f.useInProject(id, "Dungeon Game")

	p := f.plan(trashTarget(packPrefix + "/sprites"))

	if reason := blockedReason(p, packPrefix+"/sprites"); reason == "" {
		t.Fatalf("a directory holding a project-used asset must be blocked; plan = %+v", p)
	}
}

func TestPlanAllowsRemovalWhenTheProjectUseWasWithdrawn(t *testing.T) {
	f := newFixture(t)
	id := f.writeAsset("hero.png", "hero-bytes")
	f.writeAsset("hero-copy.png", "hero-bytes")
	f.useInProject(id, "Dungeon Game")
	if _, err := f.db.Writer.Exec(`UPDATE project_uses SET removed_at = ?`, f.now.Unix()); err != nil {
		t.Fatal(err)
	}

	p := f.plan(trashTarget(packPrefix + "/hero.png"))
	if !allowed(p, packPrefix+"/hero.png") {
		t.Errorf("a soft-removed use is history, not a block: %+v", p)
	}
}

// --- invariant 4: never the last copy ---------------------------------------

func TestPlanRefusesTheOnlyCopy(t *testing.T) {
	f := newFixture(t)
	f.writeAsset("unique.png", "one-of-a-kind")

	p := f.plan(trashTarget(packPrefix + "/unique.png"))

	reason := blockedReason(p, packPrefix+"/unique.png")
	if reason == "" {
		t.Fatalf("the only copy of a content hash must be refused; plan = %+v", p)
	}
	if !strings.Contains(reason, "last remaining copy") {
		t.Errorf("reason should say why, got %q", reason)
	}
}

func TestPlanKeepsOneWhenEveryCopyIsSelected(t *testing.T) {
	f := newFixture(t)
	f.writeAsset("a/dup.png", "same-bytes")
	f.writeAsset("b/dup.png", "same-bytes")
	f.writeAsset("c/dup.png", "same-bytes")

	p := f.plan(
		trashTarget(packPrefix+"/a/dup.png"),
		trashTarget(packPrefix+"/b/dup.png"),
		trashTarget(packPrefix+"/c/dup.png"),
	)

	// Three copies selected: two may go, one must stay. Never zero.
	if len(p.Ops) != 2 {
		t.Errorf("want 2 ops (three copies minus the one kept), got %d: %+v", len(p.Ops), p.Ops)
	}
	if len(p.Blocked) != 1 {
		t.Fatalf("want exactly one blocked copy, got %d: %+v", len(p.Blocked), p.Blocked)
	}
	if !strings.Contains(p.Blocked[0].Reason, "last remaining copy") {
		t.Errorf("unexpected block reason %q", p.Blocked[0].Reason)
	}
}

func TestPlanCountsCopiesInsideASelectedDirectory(t *testing.T) {
	f := newFixture(t)
	// Both copies live under one directory, so selecting the directory selects both.
	f.writeAsset("dupes/one.png", "same-bytes")
	f.writeAsset("dupes/two.png", "same-bytes")

	p := f.plan(trashTarget(packPrefix + "/dupes"))

	if len(p.Ops) != 0 || len(p.Blocked) != 1 {
		t.Fatalf("removing a directory holding every copy must be refused; ops=%+v blocked=%+v", p.Ops, p.Blocked)
	}
}

func TestPlanIgnoresMissingAssetsWhenCountingCopies(t *testing.T) {
	f := newFixture(t)
	live := f.writeAsset("live.png", "same-bytes")
	gone := f.writeAsset("gone.png", "same-bytes")
	// The second copy was already trashed earlier: its row is missing, so it is not
	// a live copy and cannot be what keeps the content alive.
	if _, err := f.db.Writer.Exec(`UPDATE assets SET missing_since = ? WHERE id = ?`, f.now.Unix(), gone); err != nil {
		t.Fatal(err)
	}

	p := f.plan(trashTarget(packPrefix + "/live.png"))

	if blockedReason(p, packPrefix+"/live.png") == "" {
		t.Errorf("asset %d is the last *live* copy and must be refused; plan = %+v", live, p)
	}
}

func TestPlanNoteStatesHowManyCopiesRemain(t *testing.T) {
	f := newFixture(t)
	f.writeAsset("a.png", "same-bytes")
	f.writeAsset("b.png", "same-bytes")
	f.writeAsset("c.png", "same-bytes")

	p := f.plan(trashTarget(packPrefix + "/a.png"))
	if len(p.Ops) != 1 {
		t.Fatalf("want 1 op, got %+v", p.Ops)
	}
	if !strings.Contains(p.Ops[0].Note, "2 copies") {
		t.Errorf("note should state the remaining copies, got %q", p.Ops[0].Note)
	}
}

// --- invariant 9 and the structural refusals --------------------------------

func TestPlanRejectsUnsafePaths(t *testing.T) {
	f := newFixture(t)
	f.writeFile("pack/junk/.DS_Store", "junk")
	// A file outside the library, which a traversal would reach.
	outside := filepath.Join(filepath.Dir(f.libraryRoot), "outside.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		path string
		want string
	}{
		{"parent traversal", "../outside.txt", "path rejected"},
		{"deep traversal", "pack/../../outside.txt", "path rejected"},
		{"absolute path", "/etc/passwd", "path rejected"},
		{"nul byte", "pack/junk/.DS_Store\x00.png", "path rejected"},
		{"backslash", `pack\junk`, "path rejected"},
		{"empty", "", "empty path"},
		{"nonexistent", "pack/nope.png", "path rejected"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := f.plan(trashTarget(tc.path))
			if len(p.Ops) != 0 {
				t.Fatalf("%q must not produce an op, got %+v", tc.path, p.Ops)
			}
			if len(p.Blocked) != 1 || !strings.Contains(p.Blocked[0].Reason, tc.want) {
				t.Fatalf("%q: want a block containing %q, got %+v", tc.path, tc.want, p.Blocked)
			}
		})
	}

	if _, err := os.Stat(outside); err != nil {
		t.Errorf("the file outside the library must be untouched: %v", err)
	}
}

func TestPlanRefusesReservedDirectoriesAndSidecars(t *testing.T) {
	f := newFixture(t)
	f.writeFile("_trash/20260101T000000Z-aaaa/library/pack/old.png", "trashed")
	f.writeFile("_inbox/pack.zip", "archive")
	f.writeFile("pack/.ambar.json", `{"schema":1}`)

	cases := []struct {
		path string
		want string
	}{
		{"_trash", "trash"},
		{"_trash/20260101T000000Z-aaaa", "trash"},
		{"_inbox", "reserved"},
		{"_inbox/pack.zip", "reserved"},
		{"pack/.ambar.json", "sidecar"},
	}
	for _, tc := range cases {
		p := f.plan(trashTarget(tc.path))
		reason := blockedReason(p, tc.path)
		if reason == "" {
			t.Errorf("%s must be refused, plan = %+v", tc.path, p)
			continue
		}
		if !strings.Contains(reason, tc.want) {
			t.Errorf("%s: reason %q should mention %q", tc.path, reason, tc.want)
		}
	}
}

func TestPlanRefusesSymlinks(t *testing.T) {
	f := newFixture(t)
	f.writeAsset("real.png", "bytes")
	f.writeAsset("real-copy.png", "bytes")
	link := filepath.Join(f.libraryRoot, packPrefix, "alias.png")
	if err := os.Symlink(filepath.Join(f.libraryRoot, packPrefix, "real.png"), link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	p := f.plan(trashTarget(packPrefix + "/alias.png"))
	if reason := blockedReason(p, packPrefix+"/alias.png"); !strings.Contains(reason, "symlink") {
		t.Errorf("a symlink must be refused with a clear reason, got %q", reason)
	}
	if !f.exists(packPrefix + "/real.png") {
		t.Error("the link target must be untouched")
	}
}

func TestPlanAcceptsUnindexedJunkAndDataRootOrphans(t *testing.T) {
	f := newFixture(t)
	// Junk the indexer ignores: no asset rows, so no copy counting applies.
	f.writeFile("pack/.DS_Store", "junk")
	macosx := filepath.Join(f.libraryRoot, packPrefix, "__MACOSX")
	if err := os.MkdirAll(macosx, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(macosx, "._hero.png"), []byte("cruft"), 0o644); err != nil {
		t.Fatal(err)
	}
	// An orphaned derivative under the data root.
	orphan := filepath.Join(f.dataRoot, "derivatives", "ab", strings.Repeat("a", 64))
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphan, "thumb.webp"), []byte("thumbnail"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := f.plan(
		trashTarget(packPrefix+"/.DS_Store"),
		trashTarget(packPrefix+"/__MACOSX"),
		Target{Root: RootData, Path: "derivatives/ab/" + strings.Repeat("a", 64), Action: ActionTrash},
	)

	if len(p.Blocked) != 0 {
		t.Fatalf("junk and orphans must be removable, got blocks %+v", p.Blocked)
	}
	if len(p.Ops) != 3 {
		t.Fatalf("want 3 ops, got %d: %+v", len(p.Ops), p.Ops)
	}
	for _, op := range p.Ops {
		if op.Path == packPrefix+"/__MACOSX" {
			if !op.IsDir || op.Files != 1 || op.Bytes != int64(len("cruft")) {
				t.Errorf("directory op should report its tree: %+v", op)
			}
		}
	}
	if p.TotalBytes() != int64(len("junk")+len("cruft")+len("thumbnail")) {
		t.Errorf("total bytes = %d, want the sum of all three", p.TotalBytes())
	}
}

func TestPlanDeduplicatesRepeatedTargets(t *testing.T) {
	f := newFixture(t)
	f.writeFile("pack/Thumbs.db", "junk")

	p := f.plan(trashTarget(packPrefix+"/Thumbs.db"), trashTarget(packPrefix+"/Thumbs.db"))
	if len(p.Ops) != 1 {
		t.Errorf("a double-submitted checkbox is one selection, got %d ops", len(p.Ops))
	}
}

func TestPlanRejectsUnknownRootAndAction(t *testing.T) {
	f := newFixture(t)
	f.writeFile("pack/Thumbs.db", "junk")

	p := f.plan(
		Target{Root: "elsewhere", Path: "pack/Thumbs.db", Action: ActionTrash},
		Target{Root: RootLibrary, Path: "pack/Thumbs.db", Action: "delete"},
	)
	if len(p.Ops) != 0 || len(p.Blocked) != 2 {
		t.Fatalf("unknown root and action must both be refused: %+v / %+v", p.Ops, p.Blocked)
	}
}

// --- the link half ----------------------------------------------------------

func TestPlanLinkRequiresIdenticalIndexedContent(t *testing.T) {
	f := newFixture(t)
	f.writeAsset("a.png", "same-bytes")
	f.writeAsset("b.png", "same-bytes")
	f.writeAsset("other.png", "different")
	f.writeFile("pack/unindexed.png", "same-bytes")

	cases := []struct {
		name, path, keep, want string
	}{
		{"identical copies", packPrefix + "/a.png", packPrefix + "/b.png", ""},
		{"different content", packPrefix + "/a.png", packPrefix + "/other.png", "content differs"},
		{"itself", packPrefix + "/a.png", packPrefix + "/a.png", "cannot be linked to itself"},
		{"no keep path", packPrefix + "/a.png", "", "needs the path of the copy to keep"},
		{"unindexed source", packPrefix + "/unindexed.png", packPrefix + "/a.png", "not in the index"},
		{"unindexed keep", packPrefix + "/a.png", packPrefix + "/unindexed.png", "is not in the index"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := f.plan(Target{Root: RootLibrary, Path: tc.path, Action: ActionLink, KeepPath: tc.keep})
			if tc.want == "" {
				if !allowed(p, tc.path) {
					t.Fatalf("should be allowed, got %+v", p.Blocked)
				}
				return
			}
			if reason := blockedReason(p, tc.path); !strings.Contains(reason, tc.want) {
				t.Fatalf("want reason containing %q, got %q", tc.want, reason)
			}
		})
	}
}

func TestPlanLinkDoesNotConsumeACopy(t *testing.T) {
	f := newFixture(t)
	f.writeAsset("a.png", "same-bytes")
	f.writeAsset("b.png", "same-bytes")

	// Linking keeps the bytes reachable at both paths, so it is allowed even though
	// only two copies exist — nothing is being removed.
	p := f.plan(Target{Root: RootLibrary, Path: packPrefix + "/a.png", Action: ActionLink, KeepPath: packPrefix + "/b.png"})
	if !allowed(p, packPrefix+"/a.png") {
		t.Fatalf("linking must not be refused by the last-copy rule: %+v", p.Blocked)
	}
	if len(p.LinkOps()) != 1 || len(p.TrashOps()) != 0 {
		t.Errorf("plan should hold one link op and no trash op: %+v", p.Ops)
	}
}

func TestPlanLinkIsAllowedForProjectUsedAssets(t *testing.T) {
	f := newFixture(t)
	id := f.writeAsset("a.png", "same-bytes")
	f.writeAsset("b.png", "same-bytes")
	f.useInProject(id, "Dungeon Game")

	// A link keeps the path working and the content byte-identical, so an imported
	// asset is not endangered. Removal of the same file stays blocked.
	p := f.plan(Target{Root: RootLibrary, Path: packPrefix + "/a.png", Action: ActionLink, KeepPath: packPrefix + "/b.png"})
	if !allowed(p, packPrefix+"/a.png") {
		t.Errorf("linking a project-used asset should be allowed: %+v", p.Blocked)
	}

	p = f.plan(trashTarget(packPrefix + "/a.png"))
	if blockedReason(p, packPrefix+"/a.png") == "" {
		t.Error("trashing the same asset must still be blocked")
	}
}
