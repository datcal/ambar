// Package removal is the only code in Ambar that touches user data
// destructively (§9.1). Everything else in the codebase is built on "originals
// are never modified"; this package is the single deliberate exception, and it is
// written to be read suspiciously.
//
// The shape is two halves that never blur into one another:
//
//   - The Planner takes human-selected Targets and answers what *would* happen.
//     It removes nothing. It resolves every path, expands directories, works out
//     the bytes, and refuses whatever must be refused — with a reason a person can
//     read. A Plan is the preview §9.1 demands before anything moves.
//   - The Executor takes a Plan and carries it out. It performs no policy of its
//     own: if the Planner did not put an Op in the Plan, the Executor never sees
//     it. Nothing is deleted in place — files move to the trash with a JSON record
//     of where they came from and why (trash.go).
//
// The safety rules of §9.1 and ARCHITECTURE.md rules 3–5, 9 live in Plan and are
// enforced as code, not as a UI flow:
//
//   - Nothing is ever selected by this package. Targets come from a human.
//   - An asset referenced by an active project_uses row is a hard block, naming
//     the project.
//   - The last live copy of a content hash can never be removed, including when a
//     selection covers every copy at once.
//   - Every path is resolved under its configured root before it is touched, and
//     the trash itself is never a target.
package removal

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/datcal/ambar/internal/db"
	"github.com/datcal/ambar/internal/library"
	"github.com/datcal/ambar/internal/safepath"
)

// Root names which configured root a Target's path is relative to. Library
// originals and generated derivatives are both removable (§9.1 lists orphaned
// derivatives under junk cleanup), but they live under different roots and must
// never be resolved against the wrong one.
type Root string

const (
	// RootLibrary is AMBAR_LIBRARY_ROOT: originals, the high-risk case.
	RootLibrary Root = "library"
	// RootData is AMBAR_DATA_ROOT: generated data such as derivatives/.
	RootData Root = "data"
)

// Valid reports whether r is a known root.
func (r Root) Valid() bool { return r == RootLibrary || r == RootData }

// Action is what the human asked for on one path.
type Action string

const (
	// ActionTrash moves the path into the trash, preserving its relative path.
	ActionTrash Action = "trash"
	// ActionLink replaces a redundant copy with a reflink or hardlink to the kept
	// copy (§9.1 "Prefer linking over deleting"). The path stays valid and the
	// bytes are shared, so nothing is removed at all — which is why it is the
	// recommended action rather than the fallback.
	ActionLink Action = "link"
)

// Valid reports whether a is a known action.
func (a Action) Valid() bool { return a == ActionTrash || a == ActionLink }

// Target is one candidate as selected by a human. Nothing constructs a Target
// from a computed set: the web flow builds them from checkboxes the user ticked,
// and the CLI from paths the user typed.
type Target struct {
	Root Root   `json:"root"`
	Path string `json:"path"`
	// Action defaults to ActionTrash when empty.
	Action Action `json:"action,omitempty"`
	// KeepPath is the library-relative path of the copy to link to. Required for
	// ActionLink, meaningless otherwise.
	KeepPath string `json:"keep_path,omitempty"`
	// Finding records which finding motivated this target, e.g. "junk:os_junk" or
	// "dupes:exact:<sha>". It goes into the trash record and the audit log, because
	// §9.1 wants every removal traceable back to the reasoning that proposed it.
	Finding string `json:"finding,omitempty"`
}

// Op is one Target the Planner has accepted, with everything the Executor and the
// preview need worked out in advance.
type Op struct {
	Root     Root   `json:"root"`
	Path     string `json:"path"`
	Action   Action `json:"action"`
	KeepPath string `json:"keep_path,omitempty"`
	Finding  string `json:"finding,omitempty"`

	// Bytes is what this Op reclaims: the file's size, or the recursive total for a
	// directory. For ActionLink it is what the shared extents save.
	Bytes int64 `json:"bytes"`
	// Files is how many regular files are involved — 1 for a file, the recursive
	// count for a directory, so a preview can say "142 files" rather than "1 path".
	Files int `json:"files"`
	// IsDir distinguishes a `__MACOSX` tree from a single `.DS_Store`.
	IsDir bool `json:"is_dir"`
	// AssetIDs are the live index rows this Op affects, so applying it can mark them
	// missing and restoring it can bring them back. Empty for paths the indexer
	// ignores, which is the normal case for junk.
	AssetIDs []int64 `json:"asset_ids,omitempty"`
	// Note is a fact worth showing in the preview, e.g. how many copies remain.
	Note string `json:"note,omitempty"`
}

// Blocked is a Target the Planner refused, with the reason to show the user.
// §9.1: "Surface *why* it is blocked, naming the project."
type Blocked struct {
	Root   Root   `json:"root"`
	Path   string `json:"path"`
	Action Action `json:"action,omitempty"`
	Reason string `json:"reason"`
}

// Plan is the complete preview: what would happen, what was refused, and the
// total. It is serialised into the job payload and into the trash record, so a
// removal that ran can always be traced back to the plan that described it.
type Plan struct {
	// Reason is the short human-readable motivation carried into the audit log,
	// e.g. "junk sweep: OS metadata files".
	Reason  string    `json:"reason"`
	Ops     []Op      `json:"ops"`
	Blocked []Blocked `json:"blocked,omitempty"`
	// Transfers are curation moves that must happen before any file moves (§9.1:
	// removing a subset pack "transfers its tags and provenance onto the superset
	// first"). This package does not know what curation is — the caller states the
	// instruction and supplies the function that carries it out.
	Transfers []Transfer `json:"transfers,omitempty"`
}

// Transfer is one pack-to-pack curation move to perform before removing anything.
type Transfer struct {
	FromPackID int64 `json:"from_pack_id"`
	ToPackID   int64 `json:"to_pack_id"`
	// What describes the move for the preview, e.g. "3 pack tag(s), provenance".
	What string `json:"what,omitempty"`
}

// TotalBytes is what the whole plan reclaims.
func (p *Plan) TotalBytes() int64 {
	var total int64
	for _, op := range p.Ops {
		total += op.Bytes
	}
	return total
}

// TotalFiles is how many regular files the plan touches.
func (p *Plan) TotalFiles() int {
	n := 0
	for _, op := range p.Ops {
		n += op.Files
	}
	return n
}

// Empty reports whether the plan would do nothing.
func (p *Plan) Empty() bool { return len(p.Ops) == 0 }

// TrashOps and LinkOps split the plan for a preview that shows the two very
// different risk profiles apart.
func (p *Plan) TrashOps() []Op { return p.opsWith(ActionTrash) }

// LinkOps is TrashOps for the linking half.
func (p *Plan) LinkOps() []Op { return p.opsWith(ActionLink) }

func (p *Plan) opsWith(a Action) []Op {
	var out []Op
	for _, op := range p.Ops {
		if op.Action == a {
			out = append(out, op)
		}
	}
	return out
}

// Planner turns Targets into a Plan. It is read-only: it queries the index and
// stats the filesystem, and that is all.
type Planner struct {
	db          *db.DB
	libraryRoot string
	dataRoot    string
	trashDir    string
}

// NewPlanner builds a Planner against the configured roots.
func NewPlanner(database *db.DB, libraryRoot, dataRoot, trashDir string) *Planner {
	return &Planner{db: database, libraryRoot: libraryRoot, dataRoot: dataRoot, trashDir: trashDir}
}

// Plan resolves and checks every Target and returns the preview. The returned
// error is for failures that make the answer unknowable (the index is
// unreadable); a Target that must not be acted on is a Blocked entry, not an
// error, because the user needs to see the reason next to the path.
func (p *Planner) Plan(ctx context.Context, reason string, targets []Target) (*Plan, error) {
	snap, err := p.loadSnapshot(ctx)
	if err != nil {
		return nil, err
	}

	plan := &Plan{Reason: strings.TrimSpace(reason)}
	seen := make(map[string]struct{}, len(targets))

	// Pass 1: structural and project-use checks, which do not depend on what else
	// is in the selection.
	var candidates []candidate

	for _, t := range targets {
		t.Path = strings.Trim(strings.TrimSpace(filepath.ToSlash(t.Path)), "/")
		if t.Root == "" {
			t.Root = RootLibrary
		}
		if t.Action == "" {
			t.Action = ActionTrash
		}
		key := string(t.Root) + "\x00" + t.Path + "\x00" + string(t.Action)
		if _, dup := seen[key]; dup {
			// A double-submitted checkbox is not an error; it is one selection.
			continue
		}
		seen[key] = struct{}{}

		op, sha, why := p.check(t, snap)
		if why != "" {
			plan.Blocked = append(plan.Blocked, Blocked{Root: t.Root, Path: t.Path, Action: t.Action, Reason: why})
			continue
		}
		candidates = append(candidates, candidate{op: op, sha: sha})
	}

	// Deterministic order, and the order the last-copy rule resolves ties in.
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].op.Root != candidates[j].op.Root {
			return candidates[i].op.Root < candidates[j].op.Root
		}
		return candidates[i].op.Path < candidates[j].op.Path
	})

	// Pass 2: invariant 4, which is a property of the selection as a whole rather
	// than of any single target. A selection that covers every live copy of a hash
	// must lose one member, not all of them: "the duplicate finder may reduce
	// copies to one; it may never reduce them to zero".
	//
	// Only ActionTrash is counted. A link keeps the bytes reachable at the same
	// path, so it removes no copy at all.
	//
	// surviving[sha] is how many live copies would still exist if every candidate
	// went ahead. It starts as "copies nobody selected" and grows again whenever a
	// candidate is refused, because a refused copy stays where it is.
	surviving := map[string]int{}
	for _, c := range candidates {
		if c.op.Action != ActionTrash {
			continue
		}
		for sha, n := range c.sha {
			if _, seenSha := surviving[sha]; !seenSha {
				surviving[sha] = snap.copies[sha]
			}
			surviving[sha] -= n
		}
	}

	for _, c := range candidates {
		if c.op.Action != ActionTrash || len(c.sha) == 0 {
			plan.Ops = append(plan.Ops, c.op)
			continue
		}
		if lastCopy := lastCopyOf(c, snap, surviving); lastCopy != "" {
			// Refusing it puts its copies back among the survivors, so the next
			// candidate for the same hash sees the copy this refusal just preserved
			// and is allowed. That is what turns "every copy selected" into "all but
			// one removed" rather than "nothing removed".
			for sha, n := range c.sha {
				surviving[sha] += n
			}
			plan.Blocked = append(plan.Blocked, Blocked{
				Root: c.op.Root, Path: c.op.Path, Action: c.op.Action, Reason: lastCopy,
			})
			continue
		}
		c.op.Note = remainingNote(c, surviving)
		plan.Ops = append(plan.Ops, c.op)
	}

	sort.Slice(plan.Blocked, func(i, j int) bool { return plan.Blocked[i].Path < plan.Blocked[j].Path })
	return plan, nil
}

// candidate is a Target that passed the single-target checks, paired with how
// many live copies of each content hash it would remove.
type candidate struct {
	op  Op
	sha map[string]int // content hash -> live copies this op removes
}

// lastCopyOf returns a block reason when nothing would survive the selection for
// one of the content hashes this op removes, and "" when every hash it touches
// still has a copy left afterwards.
//
// The refused copy is the first in the sorted candidate order. Which one is
// arbitrary; that it is deterministic and named in the preview is not.
func lastCopyOf(c candidate, snap *snapshot, surviving map[string]int) string {
	for sha := range c.sha {
		if surviving[sha] >= 1 {
			continue
		}
		total := snap.copies[sha]
		return fmt.Sprintf("this is the last remaining copy of content %s (%d live cop%s, all of them selected) — "+
			"Ambar never reduces a content hash to zero copies", shortSha(sha), total, plural(total))
	}
	return ""
}

// remainingNote states how many copies of the content survive, which is the fact
// a reviewer actually wants next to a duplicate. Only stated for a single-hash
// op: for a directory covering many hashes there is no one number to give.
func remainingNote(c candidate, surviving map[string]int) string {
	if len(c.sha) != 1 {
		return ""
	}
	for sha := range c.sha {
		remaining := surviving[sha]
		if remaining < 0 {
			remaining = 0
		}
		return fmt.Sprintf("%d cop%s of this content remain after removal", remaining, plural(remaining))
	}
	return ""
}

// check performs every test that looks only at one Target, returning either the
// Op to run or a reason to refuse it.
func (p *Planner) check(t Target, snap *snapshot) (Op, map[string]int, string) {
	if !t.Root.Valid() {
		return Op{}, nil, fmt.Sprintf("unknown root %q", t.Root)
	}
	if !t.Action.Valid() {
		return Op{}, nil, fmt.Sprintf("unknown action %q", t.Action)
	}
	if t.Path == "" {
		return Op{}, nil, "empty path"
	}
	if t.Action == ActionLink && t.Root != RootLibrary {
		return Op{}, nil, "only library files can be linked"
	}

	root := p.rootFor(t.Root)
	if root == "" {
		return Op{}, nil, fmt.Sprintf("root %q is not configured", t.Root)
	}

	// Invariant 9. The path came from a form, so it is untrusted no matter which
	// of our own reports produced it a moment earlier.
	//
	// Lstat rather than Stat: a symlink and its target are different things here,
	// and moving the link would move the name while leaving the bytes — or worse,
	// move a link whose target is someone else's file.
	info, abs, err := safepath.LstatUnder(root, t.Path)
	if err != nil {
		return Op{}, nil, fmt.Sprintf("path rejected: %v", err)
	}

	// The trash is not a removal candidate. Re-trashing it would nest batches
	// inside each other and make a restore ambiguous, and §9.1 forbids purging it
	// as a side effect of anything.
	if p.trashDir != "" && safepath.IsWithin(p.trashDir, abs) {
		return Op{}, nil, "this path is inside the trash; use the trash view to restore or purge it"
	}
	if t.Root == RootLibrary {
		if reason := reservedReason(t.Path); reason != "" {
			return Op{}, nil, reason
		}
	}

	switch {
	case info.Mode()&os.ModeSymlink != 0:
		// Moving a symlink moves the link, not the content, and its target may be
		// outside the library entirely. Not worth the ambiguity.
		return Op{}, nil, "symlinks are not removed"
	case !info.IsDir() && !info.Mode().IsRegular():
		return Op{}, nil, "not a regular file"
	}

	op := Op{
		Root:    t.Root,
		Path:    t.Path,
		Action:  t.Action,
		Finding: t.Finding,
		IsDir:   info.IsDir(),
	}

	// Which live index rows does this cover? A file target covers at most one; a
	// directory target covers everything indexed beneath it.
	var affected []liveAsset
	if t.Root == RootLibrary {
		if op.IsDir {
			affected = snap.under(t.Path)
		} else if a, ok := snap.byPath[t.Path]; ok {
			affected = []liveAsset{a}
		}
	}

	if op.IsDir {
		op.Bytes, op.Files = treeSize(abs)
	} else {
		op.Bytes, op.Files = info.Size(), 1
	}

	// Invariant 5 / §9.1: a hard block, before anything else about the file
	// matters. Checked for removal only — a link keeps the path working and the
	// bytes identical, so an imported asset is not endangered by one.
	if t.Action == ActionTrash {
		if reason := snap.projectBlock(affected); reason != "" {
			return Op{}, nil, reason
		}
	}

	shaCount := map[string]int{}
	for _, a := range affected {
		op.AssetIDs = append(op.AssetIDs, a.ID)
		shaCount[a.Sha]++
	}
	sort.Slice(op.AssetIDs, func(i, j int) bool { return op.AssetIDs[i] < op.AssetIDs[j] })

	if t.Action == ActionLink {
		if reason := p.checkLink(&op, t, snap); reason != "" {
			return Op{}, nil, reason
		}
		// Linking removes no copy, so it contributes nothing to the last-copy count.
		return op, nil, ""
	}

	return op, shaCount, ""
}

// checkLink validates the link half: same content, two different live paths, and
// a kept copy that is not itself being removed.
func (p *Planner) checkLink(op *Op, t Target, snap *snapshot) string {
	keep := strings.Trim(strings.TrimSpace(filepath.ToSlash(t.KeepPath)), "/")
	if keep == "" {
		return "linking needs the path of the copy to keep"
	}
	if keep == t.Path {
		return "a file cannot be linked to itself"
	}
	if op.IsDir {
		return "only files can be linked, not directories"
	}

	redundant, ok := snap.byPath[t.Path]
	if !ok {
		return "not in the index; only indexed assets can be linked (run a scan first)"
	}
	kept, ok := snap.byPath[keep]
	if !ok {
		return fmt.Sprintf("the copy to keep (%s) is not in the index", keep)
	}
	if redundant.Sha != kept.Sha {
		// Without this, "dedupe" would silently replace one file's content with a
		// different file's content — the worst possible outcome in this package.
		return fmt.Sprintf("content differs from %s; only byte-identical copies are linked", keep)
	}
	if _, err := safepath.ResolveExisting(p.libraryRoot, keep); err != nil {
		return fmt.Sprintf("the copy to keep was rejected: %v", err)
	}

	op.KeepPath = keep
	if op.Note == "" {
		op.Note = "content shared with " + keep + "; the path keeps working"
	}
	return ""
}

// rootFor maps a Root to its absolute directory.
func (p *Planner) rootFor(r Root) string {
	switch r {
	case RootLibrary:
		return p.libraryRoot
	case RootData:
		return p.dataRoot
	default:
		return ""
	}
}

// reservedReason refuses paths that are structurally not removable: Ambar's own
// reserved directories, and the sidecars that carry a pack's provenance.
func reservedReason(relPath string) string {
	first := relPath
	if i := strings.Index(relPath, "/"); i >= 0 {
		first = relPath[:i]
	}
	if library.IsReserved(first) {
		return fmt.Sprintf("%s is one of Ambar's reserved directories and is never removed", first)
	}
	if filepath.Base(relPath) == library.SidecarName {
		return "a " + library.SidecarName + " sidecar holds a pack's provenance and is never removed"
	}
	return ""
}

// liveAsset is one indexed, present asset — the only kind that matters here. An
// asset already marked missing is not a live copy of anything.
type liveAsset struct {
	ID   int64
	Path string // library-relative, slash-separated
	Sha  string
	Size int64
}

// snapshot is the index as the Planner needs it, read once per plan. Twenty
// thousand rows is a few megabytes, and holding them makes the last-copy
// arithmetic ordinary Go instead of a query per candidate.
type snapshot struct {
	byPath map[string]liveAsset
	// paths is byPath's keys, sorted, so a directory target can find its members
	// with a binary search instead of a scan.
	paths []string
	// copies counts live copies per content hash — the number invariant 4 protects.
	copies map[string]int
	// uses maps an asset id to the projects that reference it (§10 project_uses).
	uses map[int64][]string
}

func (p *Planner) loadSnapshot(ctx context.Context) (*snapshot, error) {
	snap := &snapshot{
		byPath: map[string]liveAsset{},
		copies: map[string]int{},
		uses:   map[int64][]string{},
	}

	rows, err := p.db.Reader.QueryContext(ctx, `
		SELECT a.id,
		       CASE WHEN p.library_rel_path = '' THEN a.rel_path
		            ELSE p.library_rel_path || '/' || a.rel_path END AS lib_path,
		       a.sha256, a.size
		FROM assets a
		JOIN packs p ON p.id = a.pack_id
		WHERE a.missing_since IS NULL`)
	if err != nil {
		return nil, fmt.Errorf("load live assets: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var a liveAsset
		if err := rows.Scan(&a.ID, &a.Path, &a.Sha, &a.Size); err != nil {
			return nil, fmt.Errorf("scan live asset: %w", err)
		}
		snap.byPath[a.Path] = a
		snap.paths = append(snap.paths, a.Path)
		snap.copies[a.Sha]++
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load live assets: %w", err)
	}
	sort.Strings(snap.paths)

	// Active uses only: a soft-removed row is history, not a reason to block.
	useRows, err := p.db.Reader.QueryContext(ctx, `
		SELECT u.asset_id,
		       CASE WHEN pr.name != '' THEN pr.name ELSE pr.uuid END
		FROM project_uses u
		JOIN projects pr ON pr.id = u.project_id
		WHERE u.removed_at IS NULL`)
	if err != nil {
		return nil, fmt.Errorf("load project uses: %w", err)
	}
	defer useRows.Close()

	for useRows.Next() {
		var id int64
		var name string
		if err := useRows.Scan(&id, &name); err != nil {
			return nil, fmt.Errorf("scan project use: %w", err)
		}
		if !contains(snap.uses[id], name) {
			snap.uses[id] = append(snap.uses[id], name)
		}
	}
	if err := useRows.Err(); err != nil {
		return nil, fmt.Errorf("load project uses: %w", err)
	}
	return snap, nil
}

// under returns the live assets beneath a directory path.
func (s *snapshot) under(dir string) []liveAsset {
	prefix := dir + "/"
	i := sort.SearchStrings(s.paths, prefix)
	var out []liveAsset
	for ; i < len(s.paths); i++ {
		if !strings.HasPrefix(s.paths[i], prefix) {
			break
		}
		out = append(out, s.byPath[s.paths[i]])
	}
	return out
}

// projectBlock returns the invariant 5 refusal for any affected asset that a
// Godot project still uses, naming the projects.
func (s *snapshot) projectBlock(affected []liveAsset) string {
	var names []string
	blocked := 0
	for _, a := range affected {
		used := s.uses[a.ID]
		if len(used) == 0 {
			continue
		}
		blocked++
		for _, n := range used {
			if !contains(names, n) {
				names = append(names, n)
			}
		}
	}
	if blocked == 0 {
		return ""
	}
	sort.Strings(names)
	what := "this file is"
	if blocked > 1 {
		what = fmt.Sprintf("%d files beneath this path are", blocked)
	}
	return fmt.Sprintf("%s in use by project %s; assets imported into a project are never removal candidates",
		what, strings.Join(names, ", "))
}

// treeSize sums the regular files beneath a directory and counts them. An
// unreadable entry contributes nothing rather than aborting: a preview that is
// slightly under-reported is better than no preview.
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

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// shortSha is the first 12 hex characters, which is plenty to identify a hash in
// a sentence and short enough to read.
func shortSha(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}
