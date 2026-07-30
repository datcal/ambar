package removal

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// --- the script export (§9.1) -----------------------------------------------

func (f *fixture) script(p *Plan, linkMode string) string {
	f.t.Helper()
	var b strings.Builder
	err := WriteScript(&b, p, ScriptOptions{
		LibraryRoot: f.libraryRoot, DataRoot: f.dataRoot, TrashDir: f.trashDir,
		LinkMode: linkMode, GeneratedAt: f.now, Actor: "burak",
	})
	if err != nil {
		f.t.Fatalf("write script: %v", err)
	}
	return b.String()
}

func TestScriptMovesToTrashAndNeverDeletes(t *testing.T) {
	f := newFixture(t)
	f.writeAsset("a.png", "same-bytes")
	f.writeAsset("b.png", "same-bytes")
	f.writeFile(packPrefix+"/.DS_Store", "junk")

	script := f.script(f.plan(
		trashTarget(packPrefix+"/a.png"),
		trashTarget(packPrefix+"/.DS_Store"),
	), "off")

	if !strings.HasPrefix(script, "#!/bin/sh") {
		t.Errorf("script must start with a shebang, got %q", script[:20])
	}
	if !strings.Contains(script, "set -eu") {
		t.Error("script must stop on the first error")
	}
	// The script mirrors the in-app path: mv into a trash batch, never rm.
	for _, forbidden := range []string{"rm -", "rm ", "shred", "unlink "} {
		if strings.Contains(script, forbidden) {
			t.Errorf("script must never delete; found %q", forbidden)
		}
	}
	if strings.Count(script, "mv -n --") != 2 {
		t.Errorf("want one guarded move per operation:\n%s", script)
	}
	// Every operation carries the reasoning that proposed it.
	if !strings.Contains(script, "finding: test") {
		t.Error("each operation must name its finding")
	}
}

func TestScriptQuotesAwkwardPaths(t *testing.T) {
	f := newFixture(t)
	// Real library names: §5.1's `PNG_Parts&Spriter_Animation`, plus a quote and a
	// space, which a naive generator would turn into a command.
	awkward := `PNG_Parts&Spriter_Animation/it's here/$(touch pwned).png`
	f.writeFile(packPrefix+"/"+awkward, "junk")

	script := f.script(f.plan(trashTarget(packPrefix+"/"+awkward)), "off")

	if strings.Contains(script, "$(touch pwned)'") && !strings.Contains(script, `'\''`) {
		t.Error("a single quote in a path must be escaped")
	}
	// The dangerous parts only ever appear inside single quotes.
	for _, line := range strings.Split(script, "\n") {
		if strings.HasPrefix(line, "#") || !strings.Contains(line, "touch pwned") {
			continue
		}
		if !strings.Contains(line, `'`) {
			t.Errorf("unquoted path in command line: %q", line)
		}
		// A command substitution must not be left unquoted anywhere.
		if strings.Contains(line, `"$(touch`) {
			t.Errorf("path is interpolated rather than quoted: %q", line)
		}
	}
}

func TestScriptRecordsWhatWasRefused(t *testing.T) {
	f := newFixture(t)
	id := f.writeAsset("hero.png", "same-bytes")
	f.writeAsset("hero-copy.png", "same-bytes")
	f.writeFile(packPrefix+"/Thumbs.db", "junk")
	f.useInProject(id, "Dungeon Game")

	plan := f.plan(trashTarget(packPrefix+"/hero.png"), trashTarget(packPrefix+"/Thumbs.db"))
	script := f.script(plan, "off")

	if !strings.Contains(script, "Refused by Ambar") {
		t.Fatal("the script must list what was refused, so it is a complete record")
	}
	if !strings.Contains(script, "Dungeon Game") {
		t.Error("the refusal must keep its reason")
	}
	// And the refused path is not in a command.
	for _, line := range strings.Split(script, "\n") {
		if strings.HasPrefix(line, "mv") && strings.Contains(line, "hero.png") {
			t.Errorf("a refused path must never appear as an operation: %q", line)
		}
	}
}

func TestScriptLinkModes(t *testing.T) {
	f := newFixture(t)
	f.writeAsset("a.png", "same-bytes")
	f.writeAsset("b.png", "same-bytes")
	plan := f.plan(Target{Root: RootLibrary, Path: packPrefix + "/a.png",
		Action: ActionLink, KeepPath: packPrefix + "/b.png"})

	reflink := f.script(plan, "reflink")
	if !strings.Contains(reflink, "cp --reflink=always") {
		t.Errorf("reflink mode should use cp --reflink:\n%s", reflink)
	}
	// The same content check the in-app path makes by re-hashing.
	if !strings.Contains(reflink, "cmp -s --") {
		t.Error("the script must refuse to link files whose bytes differ")
	}

	hard := f.script(plan, "hardlink")
	if !strings.Contains(hard, "ln -f --") {
		t.Errorf("hardlink mode should use ln:\n%s", hard)
	}

	off := f.script(plan, "off")
	if strings.Contains(off, "ln -f") || strings.Contains(off, "cp --reflink") {
		t.Error("with linking off, link operations must be comments only")
	}
}

func TestScriptWarnsThatItCannotTransferCuration(t *testing.T) {
	f := newFixture(t)
	f.writeAsset("a.png", "same-bytes")
	f.writeAsset("b.png", "same-bytes")

	plan := f.plan(trashTarget(packPrefix + "/a.png"))
	plan.Transfers = []Transfer{{FromPackID: 1, ToPackID: 2, What: "3 pack tag(s), provenance"}}

	script := f.script(plan, "off")
	if !strings.Contains(script, "WARNING") || !strings.Contains(script, "3 pack tag(s), provenance") {
		t.Errorf("the script must say it cannot transfer curation:\n%s", script)
	}
}

// --- the queue path ---------------------------------------------------------

// runner builds a Runner over the fixture, with a transfer function that records
// what it was asked to do.
func (f *fixture) runner(transfer TransferFunc) *Runner {
	return NewRunner(f.planner, f.exec, transfer, nil)
}

func payloadFor(t *testing.T, plan *Plan) []byte {
	t.Helper()
	raw, err := json.Marshal(JobPayload{Plan: *plan, Actor: Actor{Username: "burak"}})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestJobAppliesAPlanThroughTheQueue(t *testing.T) {
	f := newFixture(t)
	id := f.writeAsset("hero.png", "same-bytes")
	f.writeAsset("hero-copy.png", "same-bytes")

	plan := f.plan(trashTarget(packPrefix + "/hero.png"))
	if err := f.runner(nil).Handle(context.Background(), payloadFor(t, plan)); err != nil {
		t.Fatalf("handle: %v", err)
	}

	if f.exists(packPrefix + "/hero.png") {
		t.Error("the file should have moved")
	}
	if f.missingSince(id) == nil {
		t.Error("the asset should be marked missing")
	}
	batches, err := f.exec.ListBatches()
	if err != nil || len(batches) != 1 {
		t.Fatalf("want one trash batch, got %+v / %v", batches, err)
	}
}

func TestJobRePlansAndRefusesWhatChangedSinceThePreview(t *testing.T) {
	f := newFixture(t)
	id := f.writeAsset("hero.png", "same-bytes")
	f.writeAsset("hero-copy.png", "same-bytes")

	plan := f.plan(trashTarget(packPrefix + "/hero.png"))

	// Between the preview and the worker, the asset was imported into a project. The
	// payload still says "remove it"; the re-check must win (invariant 5).
	f.useInProject(id, "Dungeon Game")

	err := f.runner(nil).Handle(context.Background(), payloadFor(t, plan))
	if err == nil {
		t.Fatal("a plan whose every target became blocked must fail, not proceed")
	}
	if !f.exists(packPrefix + "/hero.png") {
		t.Error("the file must still be there")
	}
}

func TestJobPayloadCannotWidenWhatGetsRemoved(t *testing.T) {
	f := newFixture(t)
	f.writeAsset("hero.png", "same-bytes")
	f.writeAsset("hero-copy.png", "same-bytes")
	f.writeAsset("unique.png", "one-of-a-kind")

	plan := f.plan(trashTarget(packPrefix + "/hero.png"))
	// A payload is data at rest. Someone edits it to include the only copy of
	// another file, and a traversal for good measure.
	plan.Ops = append(plan.Ops,
		Op{Root: RootLibrary, Path: packPrefix + "/unique.png", Action: ActionTrash},
		Op{Root: RootLibrary, Path: "../outside.txt", Action: ActionTrash})

	if err := f.runner(nil).Handle(context.Background(), payloadFor(t, plan)); err != nil {
		t.Fatalf("handle: %v", err)
	}

	if f.exists(packPrefix + "/hero.png") {
		t.Error("the legitimate target should have moved")
	}
	// The smuggled ones are refused by the re-plan, not by trust in the payload.
	if !f.exists(packPrefix + "/unique.png") {
		t.Error("the last copy of another file must survive an edited payload")
	}
}

func TestJobTransfersCurationBeforeRemoving(t *testing.T) {
	f := newFixture(t)
	f.writeAsset("free/hero.png", "same-bytes")
	f.writeAsset("full/hero.png", "same-bytes")

	var order []string
	transfer := func(_ context.Context, from, to int64) (string, error) {
		if f.exists(packPrefix + "/free/hero.png") {
			order = append(order, "transfer-before-move")
		} else {
			order = append(order, "transfer-after-move")
		}
		return "2 pack tag(s)", nil
	}

	plan := f.plan(trashTarget(packPrefix + "/free/hero.png"))
	plan.Transfers = []Transfer{{FromPackID: 1, ToPackID: 2, What: "2 pack tag(s)"}}

	if err := f.runner(transfer).Handle(context.Background(), payloadFor(t, plan)); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if len(order) != 1 || order[0] != "transfer-before-move" {
		t.Errorf("curation must be transferred before anything moves, got %v", order)
	}
}

func TestJobAbortsWhenTheTransferFails(t *testing.T) {
	f := newFixture(t)
	f.writeAsset("free/hero.png", "same-bytes")
	f.writeAsset("full/hero.png", "same-bytes")

	plan := f.plan(trashTarget(packPrefix + "/free/hero.png"))
	plan.Transfers = []Transfer{{FromPackID: 1, ToPackID: 2}}

	failing := func(context.Context, int64, int64) (string, error) {
		return "", errors.New("database is locked")
	}
	err := f.runner(failing).Handle(context.Background(), payloadFor(t, plan))
	if err == nil {
		t.Fatal("a failed transfer must abort the removal")
	}
	// Nothing moved, so nothing curated was lost.
	if !f.exists(packPrefix + "/free/hero.png") {
		t.Error("the file must still be there when the transfer failed")
	}
	entries, _ := os.ReadDir(f.trashDir)
	if len(entries) != 0 {
		t.Errorf("no trash batch should exist: %v", entries)
	}
}

func TestJobRefusesATransferItCannotPerform(t *testing.T) {
	f := newFixture(t)
	f.writeAsset("free/hero.png", "same-bytes")
	f.writeAsset("full/hero.png", "same-bytes")

	plan := f.plan(trashTarget(packPrefix + "/free/hero.png"))
	plan.Transfers = []Transfer{{FromPackID: 1, ToPackID: 2}}

	// No transfer function wired: removing now would silently lose the curation.
	if err := f.runner(nil).Handle(context.Background(), payloadFor(t, plan)); err == nil {
		t.Fatal("a plan needing a transfer must be refused when no transfer is available")
	}
	if !f.exists(packPrefix + "/free/hero.png") {
		t.Error("the file must still be there")
	}
}

func TestJobRejectsMalformedPayloads(t *testing.T) {
	f := newFixture(t)
	runner := f.runner(nil)

	if err := runner.Handle(context.Background(), []byte("not json")); err == nil {
		t.Error("a malformed payload must be an error")
	}
	if err := runner.Handle(context.Background(), []byte(`{"plan":{"ops":[]}}`)); err == nil {
		t.Error("an empty plan must be an error")
	}
}

func TestScriptNeedsATimestamp(t *testing.T) {
	f := newFixture(t)
	f.writeFile(packPrefix+"/Thumbs.db", "junk")
	plan := f.plan(trashTarget(packPrefix + "/Thumbs.db"))

	var b strings.Builder
	if err := WriteScript(&b, plan, ScriptOptions{
		LibraryRoot: f.libraryRoot, TrashDir: f.trashDir, GeneratedAt: time.Time{}}); err == nil {
		t.Error("a script with no timestamp would name a trash batch it cannot reproduce")
	}
	if err := WriteScript(&b, nil, ScriptOptions{GeneratedAt: f.now}); err == nil {
		t.Error("a nil plan must be an error")
	}
}
