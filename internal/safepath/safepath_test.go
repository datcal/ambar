package safepath

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// newLibrary builds a small tree to resolve against, including the symlinks that
// are the interesting cases.
//
//	root/
//	  pack/sprite.png
//	  pack/nested/deep.png
//	  outside-target.txt        (a sibling of root, i.e. NOT in the library)
//	  root/link-inside     -> pack/sprite.png
//	  root/link-outside    -> ../outside-target.txt
//	  root/link-dir-out    -> ..
//	  root/link-loop-a     -> link-loop-b
//	  root/link-loop-b     -> link-loop-a
//	  root/link-roundtrip  -> ../root/pack        (leaves and returns)
func newLibrary(t *testing.T) string {
	t.Helper()

	base := t.TempDir()
	root := filepath.Join(base, "root")

	for _, dir := range []string{
		filepath.Join(root, "pack", "nested"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{
		filepath.Join(root, "pack", "sprite.png"),
		filepath.Join(root, "pack", "nested", "deep.png"),
		filepath.Join(base, "outside-target.txt"),
	} {
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	symlink := func(target, name string) {
		t.Helper()
		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			t.Fatalf("symlink %s -> %s: %v", name, target, err)
		}
	}
	symlink(filepath.Join("pack", "sprite.png"), "link-inside")
	symlink(filepath.Join("..", "outside-target.txt"), "link-outside")
	symlink("..", "link-dir-out")
	symlink("link-loop-b", "link-loop-a")
	symlink("link-loop-a", "link-loop-b")
	symlink(filepath.Join("..", "root", "pack"), "link-roundtrip")

	return root
}

func TestResolveAcceptsPathsInsideTheRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX symlink semantics")
	}
	root := newLibrary(t)

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"file", "pack/sprite.png", "pack/sprite.png"},
		{"nested file", "pack/nested/deep.png", "pack/nested/deep.png"},
		{"directory", "pack", "pack"},
		{"redundant separators", "pack//sprite.png", "pack/sprite.png"},
		{"dot segments", "pack/./sprite.png", "pack/sprite.png"},
		// ".." that stays inside is legitimate, if odd.
		{"parent that stays inside", "pack/nested/../sprite.png", "pack/sprite.png"},
		{"leading dot slash", "./pack/sprite.png", "pack/sprite.png"},
		// A symlink pointing at something inside the root is fine.
		{"symlink inside", "link-inside", "pack/sprite.png"},
		// A symlink that leaves and comes back is also fine: the destination is
		// what matters, not the route.
		{"symlink leaving and returning", "link-roundtrip/sprite.png", "pack/sprite.png"},
		// Not-yet-existing paths must resolve, so callers can create files.
		{"new file in existing dir", "pack/new-sidecar.ambar.json", "pack/new-sidecar.ambar.json"},
		{"new nested path", "pack/a/b/c.png", "pack/a/b/c.png"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Resolve(root, tc.input)
			if err != nil {
				t.Fatalf("Resolve(%q) errored: %v", tc.input, err)
			}
			want := filepath.Join(root, tc.want)
			if got != want {
				t.Errorf("Resolve(%q) = %q, want %q", tc.input, got, want)
			}
		})
	}
}

// TestResolveRejectsEscapes is the table this package exists for. Every entry
// must fail; a pass here is a path-traversal vulnerability.
func TestResolveRejectsEscapes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX symlink semantics")
	}
	root := newLibrary(t)

	tests := []struct {
		name  string
		input string
	}{
		{"simple parent", ".."},
		{"parent with child", "../outside-target.txt"},
		{"deep parent chain", "../../../../../../etc/passwd"},
		{"parent after a real directory", "pack/../../outside-target.txt"},
		{"parent buried mid-path", "pack/nested/../../../outside-target.txt"},
		{"absolute path", "/etc/passwd"},
		{"absolute inside root", "/tmp"},
		{"empty", ""},
		{"NUL byte", "pack/sprite.png\x00/../../etc/passwd"},
		{"bare NUL", "\x00"},
		{"backslash separator", `..\outside-target.txt`},
		{"backslash mid-path", `pack\..\..\outside-target.txt`},
		{"windows drive", `C:\Windows\System32`},
		{"windows drive lowercase", "c:/windows"},

		// The cases string cleaning alone cannot catch.
		{"symlink to a file outside", "link-outside"},
		{"symlink to a directory outside", "link-dir-out"},
		{"path through a symlink to outside", "link-dir-out/outside-target.txt"},
		{"symlink loop", "link-loop-a"},
		{"path through a symlink loop", "link-loop-a/anything"},

		// URL-encoded traversal must NOT be decoded here. %2e%2e is a literal
		// filename; if a caller decodes before calling, that is the caller's bug,
		// and the containment check still catches the decoded form.
		{"double dot as a real directory name is fine but this one escapes", "pack/../.."},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Resolve(root, tc.input)
			if err == nil {
				t.Fatalf("Resolve(%q) succeeded, returning %q — this is a traversal", tc.input, got)
			}
			if !errors.Is(err, ErrEscapes) && !errors.Is(err, ErrUnsafeInput) &&
				!errors.Is(err, os.ErrNotExist) && !strings.Contains(err.Error(), "resolve") {
				t.Errorf("Resolve(%q) failed with an unclassified error: %v", tc.input, err)
			}
		})
	}
}

// TestResolveRejectsSiblingWithSharedPrefix is the classic off-by-one: a plain
// strings.HasPrefix check would accept /library-backup as being inside /library.
func TestResolveRejectsSiblingWithSharedPrefix(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "library")
	sibling := filepath.Join(base, "library-backup")
	for _, dir := range []string{root, sibling} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	secret := filepath.Join(sibling, "secret.txt")
	if err := os.WriteFile(secret, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Resolve(root, "../library-backup/secret.txt"); err == nil {
		t.Error("a sibling directory sharing the root's name prefix was accepted")
	}
	if IsWithin(root, secret) {
		t.Error("IsWithin accepted a sibling sharing the root's name prefix")
	}
}

func TestResolveRejectsBadRoots(t *testing.T) {
	for _, root := range []string{"", "relative/path", "./also-relative"} {
		if _, err := Resolve(root, "file.png"); err == nil {
			t.Errorf("root %q was accepted", root)
		}
	}
	// A root that does not exist cannot be resolved. config.Load refuses to
	// start on this, so reaching it means the mount vanished at runtime.
	if _, err := Resolve(filepath.Join(t.TempDir(), "gone"), "file.png"); err == nil {
		t.Error("a nonexistent root was accepted")
	}
}

func TestResolveExisting(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX symlink semantics")
	}
	root := newLibrary(t)

	if _, err := ResolveExisting(root, "pack/sprite.png"); err != nil {
		t.Errorf("an existing file was rejected: %v", err)
	}
	// The difference from Resolve: a path that is safe but absent is an error.
	if _, err := ResolveExisting(root, "pack/not-here.png"); err == nil {
		t.Error("a nonexistent file was accepted by ResolveExisting")
	}
	if _, err := ResolveExisting(root, "../outside-target.txt"); err == nil {
		t.Error("a traversal was accepted by ResolveExisting")
	}
}

func TestRelUnder(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX symlink semantics")
	}
	root := newLibrary(t)

	tests := []struct {
		name string
		abs  string
		want string
	}{
		{"file", filepath.Join(root, "pack", "sprite.png"), "pack/sprite.png"},
		{"nested", filepath.Join(root, "pack", "nested", "deep.png"), "pack/nested/deep.png"},
		{"directory", filepath.Join(root, "pack"), "pack"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := RelUnder(root, tc.abs)
			if err != nil {
				t.Fatalf("RelUnder(%q): %v", tc.abs, err)
			}
			if got != tc.want {
				t.Errorf("RelUnder(%q) = %q, want %q", tc.abs, got, tc.want)
			}
			// The stored form is always slash-separated, whatever the host uses.
			if strings.Contains(got, `\`) {
				t.Errorf("RelUnder returned a backslash path: %q", got)
			}
		})
	}
}

func TestRelUnderRejects(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX symlink semantics")
	}
	root := newLibrary(t)
	outside := filepath.Join(filepath.Dir(root), "outside-target.txt")

	for _, tc := range []struct {
		name string
		abs  string
	}{
		{"outside the root", outside},
		{"the root itself", root},
		{"relative input", "pack/sprite.png"},
		{"empty", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := RelUnder(root, tc.abs); err == nil {
				t.Errorf("RelUnder(%q) succeeded, returning %q", tc.abs, got)
			}
		})
	}
}

// TestResolveIsIdempotent: feeding a result back in as a relative path must not
// drift, which matters because rel_path round-trips through the database.
func TestResolveIsIdempotent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX symlink semantics")
	}
	root := newLibrary(t)

	first, err := Resolve(root, "pack/nested/deep.png")
	if err != nil {
		t.Fatal(err)
	}
	rel, err := RelUnder(root, first)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Resolve(root, rel)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Errorf("round trip drifted: %q then %q", first, second)
	}
}

// TestResolveHandlesAwkwardButLegalNames: real vendor packs contain spaces,
// ampersands and unicode, and none of them are a security problem.
func TestResolveHandlesAwkwardButLegalNames(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX symlink semantics")
	}
	root := newLibrary(t)

	// Names taken from the shapes §5.1 describes.
	for _, name := range []string{
		"PNG_Parts&Spriter_Animation",
		"2 Objects",
		"Tiled_files",
		"Café Ambiance",
		"file with spaces.png",
		"...leading-dots.png",
		"emoji-🎮.png",
		"-leading-dash.png",
	} {
		t.Run(name, func(t *testing.T) {
			got, err := Resolve(root, "pack/"+name)
			if err != nil {
				t.Errorf("Resolve rejected the legal name %q: %v", name, err)
				return
			}
			if !strings.HasPrefix(got, root) {
				t.Errorf("Resolve(%q) = %q, outside the root", name, got)
			}
		})
	}
}

// TestIsWithin covers the cheap check used on paths from trusted sources.
func TestIsWithin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX symlink semantics")
	}
	root := newLibrary(t)

	if !IsWithin(root, filepath.Join(root, "pack", "sprite.png")) {
		t.Error("a file inside the root was reported as outside")
	}
	if !IsWithin(root, root) {
		t.Error("the root was reported as outside itself")
	}
	if IsWithin(root, filepath.Join(filepath.Dir(root), "outside-target.txt")) {
		t.Error("a file outside the root was reported as inside")
	}
	if IsWithin(root, "/etc/passwd") {
		t.Error("/etc/passwd was reported as inside the root")
	}
	// A symlink inside the root pointing out must be reported as outside.
	if IsWithin(root, filepath.Join(root, "link-outside")) {
		t.Error("a symlink to outside the root was reported as inside")
	}
}
