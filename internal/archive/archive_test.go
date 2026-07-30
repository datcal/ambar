package archive

import (
	"archive/zip"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// zentry describes one entry to write into a test zip.
type zentry struct {
	name string
	mode fs.FileMode // 0 means a regular 0644 file
	data string
}

// makeZip writes a zip with the given entries and returns its path. Crafting the
// zip directly is how the malicious cases (traversal, absolute, symlink) get
// built — no real archive tool would produce them.
func makeZip(t *testing.T, entries []zentry) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "a.zip")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	for _, e := range entries {
		hdr := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		hdr.SetMode(mode)
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(e.data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

func destDir(t *testing.T) string {
	t.Helper()
	d := filepath.Join(t.TempDir(), "dest")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestDetectKind(t *testing.T) {
	write := func(magic string) string {
		p := filepath.Join(t.TempDir(), "f.bin")
		os.WriteFile(p, []byte(magic+"padding padding"), 0o644)
		return p
	}
	if k := DetectKind(write("PK\x03\x04")); k != KindZip {
		t.Errorf("zip magic => %q", k)
	}
	if k := DetectKind(write("Rar!\x1a\x07\x01\x00")); k != KindRar {
		t.Errorf("rar5 magic => %q", k)
	}
	if k := DetectKind(write("7z\xbc\xaf\x27\x1c")); k != KindSevenZip {
		t.Errorf("7z magic => %q", k)
	}
	// Extension fallback when there is no recognisable magic.
	noMagic := filepath.Join(t.TempDir(), "x.zip")
	os.WriteFile(noMagic, []byte("not really a zip"), 0o644)
	if k := DetectKind(noMagic); k != KindZip {
		t.Errorf("extension fallback => %q", k)
	}
}

func TestExtractHappyPath(t *testing.T) {
	src := makeZip(t, []zentry{
		{name: "sprites/hero.png", data: "PNG"},
		{name: "models/tree.glb", data: "GLB"},
	})
	dest := destDir(t)
	res, err := Extract(src, dest, Options{})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if res.FilesWritten != 2 {
		t.Errorf("files = %d, want 2", res.FilesWritten)
	}
	if res.Flattened != "" {
		t.Errorf("nothing to flatten across two top dirs, got %q", res.Flattened)
	}
	for _, rel := range []string{"sprites/hero.png", "models/tree.glb"} {
		if _, err := os.Stat(filepath.Join(dest, rel)); err != nil {
			t.Errorf("missing %s: %v", rel, err)
		}
	}
}

func TestExtractFlattensSingleTopFolder(t *testing.T) {
	src := makeZip(t, []zentry{
		{name: "pack/a.png", data: "a"},
		{name: "pack/sub/b.png", data: "b"},
	})
	dest := destDir(t)
	res, err := Extract(src, dest, Options{})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if res.Flattened != "pack" {
		t.Errorf("Flattened = %q, want pack", res.Flattened)
	}
	// Files land without the redundant wrapper.
	if _, err := os.Stat(filepath.Join(dest, "a.png")); err != nil {
		t.Errorf("a.png not flattened into place: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "sub", "b.png")); err != nil {
		t.Errorf("sub/b.png not flattened into place: %v", err)
	}
}

func TestExtractTraversalAborts(t *testing.T) {
	src := makeZip(t, []zentry{
		{name: "ok.png", data: "ok"},
		{name: "../evil.txt", data: "pwned"},
	})
	dest := destDir(t)
	_, err := Extract(src, dest, Options{})
	if !errors.Is(err, ErrUnsafeEntry) {
		t.Fatalf("err = %v, want ErrUnsafeEntry", err)
	}
	// The escaping file must not exist beside dest.
	if _, err := os.Stat(filepath.Join(filepath.Dir(dest), "evil.txt")); !os.IsNotExist(err) {
		t.Errorf("traversal wrote outside dest")
	}
}

// A lone traversal entry must still abort: flatten must never strip ".." and
// thereby neutralise the escape into an in-root path.
func TestExtractLoneTraversalAborts(t *testing.T) {
	src := makeZip(t, []zentry{{name: "../evil.txt", data: "pwned"}})
	dest := destDir(t)
	_, err := Extract(src, dest, Options{})
	if !errors.Is(err, ErrUnsafeEntry) {
		t.Fatalf("err = %v, want ErrUnsafeEntry", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dest), "evil.txt")); !os.IsNotExist(err) {
		t.Errorf("lone traversal escaped")
	}
}

func TestExtractAbsolutePathAborts(t *testing.T) {
	src := makeZip(t, []zentry{{name: "/etc/passwd", data: "x"}})
	_, err := Extract(src, destDir(t), Options{})
	if !errors.Is(err, ErrUnsafeEntry) {
		t.Errorf("absolute path err = %v, want ErrUnsafeEntry", err)
	}
}

func TestExtractBackslashAborts(t *testing.T) {
	src := makeZip(t, []zentry{{name: `..\evil.txt`, data: "x"}})
	_, err := Extract(src, destDir(t), Options{})
	if !errors.Is(err, ErrUnsafeEntry) {
		t.Errorf("backslash err = %v, want ErrUnsafeEntry", err)
	}
}

func TestExtractSkipsSymlink(t *testing.T) {
	src := makeZip(t, []zentry{
		{name: "real.png", data: "png"},
		{name: "link", mode: fs.ModeSymlink | 0o777, data: "/etc/passwd"},
	})
	dest := destDir(t)
	res, err := Extract(src, dest, Options{})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if res.FilesWritten != 1 {
		t.Errorf("files = %d, want 1 (symlink skipped)", res.FilesWritten)
	}
	if len(res.Skipped) != 1 || res.Skipped[0] != "link" {
		t.Errorf("Skipped = %v, want [link]", res.Skipped)
	}
	// The symlink must not exist on disk.
	if _, err := os.Lstat(filepath.Join(dest, "link")); !os.IsNotExist(err) {
		t.Errorf("symlink was written")
	}
}

func TestExtractEntryCountCap(t *testing.T) {
	src := makeZip(t, []zentry{
		{name: "a.png", data: "a"},
		{name: "b.png", data: "b"},
		{name: "c.png", data: "c"},
	})
	_, err := Extract(src, destDir(t), Options{MaxEntries: 2})
	if !errors.Is(err, ErrTooManyEntries) {
		t.Errorf("err = %v, want ErrTooManyEntries", err)
	}
}

func TestExtractTotalSizeCap(t *testing.T) {
	src := makeZip(t, []zentry{{name: "big.bin", data: strings.Repeat("x", 100)}})
	_, err := Extract(src, destDir(t), Options{MaxTotalBytes: 10})
	if !errors.Is(err, ErrTooLarge) {
		t.Errorf("err = %v, want ErrTooLarge", err)
	}
}

func TestInspect(t *testing.T) {
	src := makeZip(t, []zentry{
		{name: "a.png", data: "hello"},
		{name: "dir/", mode: fs.ModeDir | 0o755},
		{name: "dir/b.png", data: "world!"},
	})
	info, err := Inspect(src)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if info.Kind != KindZip {
		t.Errorf("kind = %q", info.Kind)
	}
	if info.FileCount != 2 {
		t.Errorf("file count = %d, want 2 (dir excluded)", info.FileCount)
	}
	if info.TotalSize != int64(len("hello")+len("world!")) {
		t.Errorf("total size = %d", info.TotalSize)
	}
}
