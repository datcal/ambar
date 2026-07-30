package junk

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFile writes content under root, creating parent directories.
func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mkdir(t *testing.T, root, rel string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(rel)), 0o755); err != nil {
		t.Fatal(err)
	}
}

// findingOf returns the finding of a kind, or a zero Finding if absent.
func findingOf(r *Report, k Kind) Finding {
	for _, f := range r.Findings {
		if f.Kind == k {
			return f
		}
	}
	return Finding{}
}

func TestScanLibraryClassifiesEveryKind(t *testing.T) {
	lib := t.TempDir()

	// Real content, which must never be reported.
	writeFile(t, lib, "pack/hero.png", "real pixels")
	writeFile(t, lib, "pack/.ambar.json", "{}")

	// OS metadata junk.
	writeFile(t, lib, "pack/.DS_Store", "finder junk")
	writeFile(t, lib, "pack/Thumbs.db", "explorer junk")
	writeFile(t, lib, "pack/._hero.png", "appledouble")

	// A __MACOSX shadow tree: two files, must be reported as one item, never
	// descended into as individual junk.
	writeFile(t, lib, "pack/__MACOSX/._hero.png", "shadow1")
	writeFile(t, lib, "pack/__MACOSX/sub/._other.png", "shadow2")

	// A zero-byte file.
	writeFile(t, lib, "pack/empty.dat", "")

	// An empty directory.
	mkdir(t, lib, "pack/emptydir")

	r, err := Scan(Options{LibraryRoot: lib})
	if err != nil {
		t.Fatal(err)
	}

	if got := findingOf(r, KindOSJunk).Count(); got != 3 {
		t.Errorf("os_junk count = %d, want 3", got)
	}
	macosx := findingOf(r, KindMacOSX)
	if macosx.Count() != 1 {
		t.Fatalf("macosx items = %d, want 1 (the tree as a single item)", macosx.Count())
	}
	if macosx.Items[0].Path != "pack/__MACOSX" {
		t.Errorf("macosx path = %q, want pack/__MACOSX", macosx.Items[0].Path)
	}
	if zb := findingOf(r, KindZeroByte); zb.Count() != 1 || zb.Items[0].Path != "pack/empty.dat" {
		t.Errorf("zero_byte = %+v, want one pack/empty.dat", zb.Items)
	}
	if ed := findingOf(r, KindEmptyDir); ed.Count() != 1 || ed.Items[0].Path != "pack/emptydir" {
		t.Errorf("empty_dir = %+v, want one pack/emptydir", ed.Items)
	}

	// The sidecar and the real asset must not appear anywhere.
	for _, f := range r.Findings {
		for _, it := range f.Items {
			if it.Path == "pack/hero.png" || it.Path == "pack/.ambar.json" {
				t.Errorf("real content %q reported as junk under %s", it.Path, f.Kind)
			}
		}
	}
}

func TestScanDoesNotDescendMacOSX(t *testing.T) {
	lib := t.TempDir()
	// A .DS_Store *inside* __MACOSX must not also surface as os_junk.
	writeFile(t, lib, "pack/__MACOSX/.DS_Store", "junk")
	writeFile(t, lib, "pack/real.png", "x")

	r, err := Scan(Options{LibraryRoot: lib})
	if err != nil {
		t.Fatal(err)
	}
	if findingOf(r, KindOSJunk).Count() != 0 {
		t.Error("a file inside __MACOSX was double-counted as os_junk")
	}
	if findingOf(r, KindMacOSX).Count() != 1 {
		t.Error("the __MACOSX tree was not reported")
	}
}

func TestScanSkipsReservedDirs(t *testing.T) {
	lib := t.TempDir()
	// The trash and inbox are Ambar's own; their contents must never be reported.
	writeFile(t, lib, "_trash/pack/.DS_Store", "staged")
	writeFile(t, lib, "_inbox/dropme.zip", "")
	writeFile(t, lib, "pack/real.png", "x")

	r, err := Scan(Options{LibraryRoot: lib})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range r.Findings {
		for _, it := range f.Items {
			if strings.HasPrefix(it.Path, "_trash") || strings.HasPrefix(it.Path, "_inbox") {
				t.Errorf("reported %q inside a reserved directory", it.Path)
			}
		}
	}
}

func TestScanOrphanDerivatives(t *testing.T) {
	data := t.TempDir()
	live := "aa" + repeat62('0')   // 64 hex, referenced
	orphan := "bb" + repeat62('1') // 64 hex, not referenced

	// derivatives/<xx>/<sha>/thumb.webp
	writeFile(t, data, "derivatives/aa/"+live+"/thumb.webp", "live-thumb")
	writeFile(t, data, "derivatives/bb/"+orphan+"/thumb.webp", "orphan-thumb-bytes")
	writeFile(t, data, "derivatives/bb/"+orphan+"/preview.webp", "more")

	r, err := Scan(Options{
		DataRoot:    data,
		KnownHashes: map[string]struct{}{live: {}},
	})
	if err != nil {
		t.Fatal(err)
	}
	f := findingOf(r, KindOrphanDerivative)
	if f.Count() != 1 {
		t.Fatalf("orphan count = %d, want 1", f.Count())
	}
	if f.Items[0].Path != "derivatives/bb/"+orphan {
		t.Errorf("orphan path = %q", f.Items[0].Path)
	}
	if f.TotalBytes == 0 {
		t.Error("orphan bytes not summed")
	}
}

func TestScanNoKnownHashesSkipsOrphanCheck(t *testing.T) {
	data := t.TempDir()
	sha := "cc" + repeat62('2')
	writeFile(t, data, "derivatives/cc/"+sha+"/thumb.webp", "x")

	// With no KnownHashes supplied, every derivative would look orphaned; the scan
	// must not then report the whole cache as removable.
	r, err := Scan(Options{DataRoot: data})
	if err != nil {
		t.Fatal(err)
	}
	if findingOf(r, KindOrphanDerivative).Count() != 0 {
		t.Error("orphans reported without a known-hash set")
	}
}

func TestReportSortedByLargestWin(t *testing.T) {
	lib := t.TempDir()
	// One big __MACOSX tree and one tiny .DS_Store: the tree should sort first.
	writeFile(t, lib, "pack/__MACOSX/big.bin", string(make([]byte, 5000)))
	writeFile(t, lib, "pack/.DS_Store", "x")
	writeFile(t, lib, "pack/real.png", "y")

	r, err := Scan(Options{LibraryRoot: lib})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Findings) < 2 {
		t.Fatalf("want at least 2 findings, got %d", len(r.Findings))
	}
	if r.Findings[0].Kind != KindMacOSX {
		t.Errorf("largest finding = %s, want macosx first", r.Findings[0].Kind)
	}
	if r.TotalItems() < 2 {
		t.Errorf("TotalItems = %d, want >= 2", r.TotalItems())
	}
}

func TestScanEmptyLibraryIsCleanReport(t *testing.T) {
	r, err := Scan(Options{LibraryRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Empty() {
		t.Errorf("clean library produced findings: %+v", r.Findings)
	}
	if !r.LibraryScanned {
		t.Error("LibraryScanned should be true")
	}
}

// repeat62 builds a 62-rune string, so with a 2-char prefix the name is 64 hex.
func repeat62(r byte) string {
	b := make([]byte, 62)
	for i := range b {
		b[i] = r
	}
	return string(b)
}
