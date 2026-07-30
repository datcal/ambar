package derive

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/image/font/gofont/goregular"
)

// TestDeriveFontRendersASpecimen: a font's preview is the font itself (M15). The
// fixture is Go's own embedded typeface, so the test needs no vendored font file.
func TestDeriveFontRendersASpecimen(t *testing.T) {
	dir := t.TempDir()
	fontPath := filepath.Join(dir, "Go-Regular.ttf")
	if err := os.WriteFile(fontPath, goregular.TTF, 0o644); err != nil {
		t.Fatal(err)
	}

	const sha = "cafebabe00000000000000000000000000000000000000000000000000000001"
	res, err := Generate(GenerateOptions{
		AbsPath:  fontPath,
		Ext:      "ttf",
		SHA256:   sha,
		DataRoot: dir,
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	relDir, err := Dir(sha)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{FileThumb, FilePreview} {
		info, err := os.Stat(filepath.Join(dir, relDir, name))
		if err != nil {
			t.Errorf("%s was not written: %v", name, err)
			continue
		}
		// A specimen that rendered nothing would still encode, so assert it is not a
		// trivially small image.
		if info.Size() < 400 {
			t.Errorf("%s is %d bytes; the specimen looks empty", name, info.Size())
		}
	}

	// The family name is reported, which is what the detail page shows beside it.
	var sawFamily bool
	for _, note := range res.Notes {
		if len(note) > 8 && note[:8] == "family: " {
			sawFamily = true
		}
	}
	if !sawFamily {
		t.Errorf("notes = %v, want the family name", res.Notes)
	}
}

// TestDeriveFontRefusesWrappedFormats: woff/woff2 are compressed wrappers sfnt cannot
// read. §6 wants an honest "unsupported, and here is why" rather than a guess.
func TestDeriveFontRefusesWrappedFormats(t *testing.T) {
	for _, ext := range []string{"woff", "woff2", "fnt"} {
		if isFontExt(ext) {
			t.Errorf("%s should not take the font path", ext)
		}
	}
	for _, ext := range []string{"ttf", "otf", "ttc"} {
		if !isFontExt(ext) {
			t.Errorf("%s should take the font path", ext)
		}
	}
}

// TestDeriveFontRejectsGarbage: a file that claims to be a font but is not must be
// recorded as unsupported, never as a crash.
func TestDeriveFontRejectsGarbage(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "broken.ttf")
	if err := os.WriteFile(bad, []byte("this is not a font at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Generate(GenerateOptions{
		AbsPath: bad, Ext: "ttf", DataRoot: dir,
		SHA256: "cafebabe00000000000000000000000000000000000000000000000000000002",
	})
	if err == nil {
		t.Fatal("a garbage font was accepted")
	}
	if !errors.Is(err, ErrUnsupported) {
		t.Errorf("error = %v, want it to wrap ErrUnsupported", err)
	}
}
