package aseprite

import (
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The builder tests in aseprite_test.go construct files to the documented layout,
// which verifies the decoder against the spec but not against what Aseprite
// actually writes. This test closes that gap without vendoring anything: it decodes
// every .aseprite under a directory named by AMBAR_ASEPRITE_CORPUS and skips when
// the variable is unset.
//
// Nothing is copied into the repository on purpose. The real corpus available here
// is CraftPix free-licence artwork, which does not permit redistribution — so the
// files stay in the library where they were downloaded, and this test is run against
// them locally:
//
//	AMBAR_ASEPRITE_CORPUS=/volume2/game/assets go test ./internal/aseprite/
//
// A failure here is a decoder bug. A file that decodes with a Note is fine: the
// point of Notes is that an approximation is flagged rather than hidden.
//
// Where the pack also ships the vendor's own PNG export of the same artwork, the
// decoded frames are compared against it pixel by pixel — see compareWithVendorSheet.
func TestDecodeRealCorpus(t *testing.T) {
	root := strings.TrimSpace(os.Getenv("AMBAR_ASEPRITE_CORPUS"))
	if root == "" {
		t.Skip("set AMBAR_ASEPRITE_CORPUS to a directory of real .aseprite files to run this")
	}
	// WalkDir does not follow a symlink, including the root one — and a library root
	// is very often a symlink to the volume it really lives on.
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}

	var files []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // an unreadable corner of a library is not this test's problem
		}
		if d.IsDir() {
			// __MACOSX holds AppleDouble stubs of the same names, which are not
			// Aseprite files at all (§5.1) and would only produce noise.
			if strings.EqualFold(d.Name(), "__MACOSX") {
				return filepath.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(d.Name()))
		if ext != ".aseprite" && ext != ".ase" {
			return nil
		}
		if strings.HasPrefix(d.Name(), "._") {
			return nil
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if len(files) == 0 {
		t.Skipf("no .aseprite files under %s", root)
	}

	var frames, notes, sheets int
	depths := map[int]int{}
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		file, err := Decode(data, Options{MaxPixels: DefaultMaxPixels})
		if err != nil {
			t.Errorf("%s: decode failed: %v", filepath.Base(path), err)
			continue
		}

		// The things §6 depends on: a canvas, at least one composited frame, and a
		// frame image whose bounds match the canvas.
		if file.Width <= 0 || file.Height <= 0 {
			t.Errorf("%s: canvas is %dx%d", filepath.Base(path), file.Width, file.Height)
		}
		if len(file.Frames) == 0 {
			t.Errorf("%s: decoded no frames", filepath.Base(path))
			continue
		}
		for i, f := range file.Frames {
			if f.Image == nil {
				t.Errorf("%s: frame %d has no image", filepath.Base(path), i)
				continue
			}
			if b := f.Image.Bounds(); b.Dx() != file.Width || b.Dy() != file.Height {
				t.Errorf("%s: frame %d is %dx%d, want the %dx%d canvas",
					filepath.Base(path), i, b.Dx(), b.Dy(), file.Width, file.Height)
			}
			if f.Duration <= 0 {
				t.Errorf("%s: frame %d has duration %s", filepath.Base(path), i, f.Duration)
			}
		}

		frames += len(file.Frames)
		notes += len(file.Notes)
		depths[file.ColorDepth]++
		for _, note := range file.Notes {
			t.Logf("%s: %s", filepath.Base(path), note)
		}

		if compared := compareWithVendorSheet(t, path, file); compared {
			sheets++
		}
	}

	t.Logf("decoded %d file(s), %d frame(s), depths %v, %d note(s), %d compared against a vendor sheet",
		len(files), frames, depths, notes, sheets)
	if sheets == 0 {
		t.Log("no vendor PNG sheets found beside the corpus; only structural checks ran")
	}
}

// compareWithVendorSheet is the strongest check available: CraftPix-style packs ship
// the same artwork twice, as .aseprite sources and as a PNG spritesheet exported by
// Aseprite itself. Where both exist, our composite must reproduce that export — 70 of
// the 72 files in this corpus match it byte for byte, and the tolerance below says
// exactly what the other two are allowed to differ by and why.
//
// The sheet is laid out as frames across and directions down, and one .aseprite file
// is one direction, so the closest row is the one to compare against. This is what
// caught the straight-versus-premultiplied alpha bug: the opaque pixels matched and
// only the semi-transparent ones did not.
//
// Returns whether a comparison actually happened.
func compareWithVendorSheet(t *testing.T, asepritePath string, file *File) bool {
	t.Helper()

	sheetPath, ok := vendorSheetFor(asepritePath)
	if !ok {
		return false
	}
	f, err := os.Open(sheetPath)
	if err != nil {
		return false
	}
	defer f.Close() //nolint:errcheck

	sheet, err := png.Decode(f)
	if err != nil {
		t.Logf("%s: vendor sheet did not decode: %v", filepath.Base(sheetPath), err)
		return false
	}
	b := sheet.Bounds()
	if file.Width == 0 || file.Height == 0 ||
		b.Dx() < file.Width*len(file.Frames) || b.Dy() < file.Height {
		// A sheet with a different geometry (trimmed, packed, or a different
		// animation) is not something to guess about.
		return false
	}

	// Compare against every row and keep the closest: one .aseprite is one direction,
	// and which row that is is not encoded anywhere.
	rows := b.Dy() / file.Height
	best := comparison{exact: -1}
	bestRow := -1
	for row := 0; row < rows; row++ {
		c := compareRow(file, sheet, b, row)
		if bestRow < 0 || c.exact > best.exact {
			best, bestRow = c, row
		}
	}

	total := file.Width * file.Height * len(file.Frames)

	// What tolerance is defensible, and why it is this narrow:
	//
	//   - A per-channel difference of 1 with identical alpha is a rounding difference.
	//     Aseprite's blend arithmetic changed at some point, and this corpus proves it:
	//     within one pack, Plant1's export matches the current integer formula exactly
	//     while Plant2's and Plant3's match the older rounded one. Nothing can match
	//     both, so blendNormal follows the current source and this test accepts ±1.
	//   - Anything larger is a real disagreement. The bug this test was written for — a
	//     straight-alpha composite returned in a premultiplied image.RGBA — showed up as
	//     ~3% of pixels off by up to 94, in every file with a semi-transparent layer.
	//
	// A handful of pixels may still differ by more than 1 because the vendor's sheet is
	// not always a clean export: two files in this corpus have a stray pixel or a
	// shadow blended twice in the sheet but not in the source. That is capped tightly
	// and always reported, rather than being allowed to grow silently.
	const hardAllowance = 0.001 // 0.1% of pixels

	if float64(best.hard) > float64(total)*hardAllowance {
		t.Errorf("%s: row %d of %s differs by more than rounding: %d of %d pixel(s) off by >1 per channel "+
			"(%d off by exactly 1, %d alpha mismatches). Our composite disagrees with Aseprite's own export",
			filepath.Base(asepritePath), bestRow, filepath.Base(sheetPath),
			best.hard, total, best.rounding, best.alpha)
		return true
	}
	if best.hard > 0 || best.rounding > 0 {
		t.Logf("%s: row %d of %s within tolerance — %d exact, %d off by 1, %d off by more",
			filepath.Base(asepritePath), bestRow, filepath.Base(sheetPath),
			best.exact, best.rounding, best.hard)
	}
	return true
}

// comparison counts how one row of a sheet compares to the decoded frames.
type comparison struct {
	exact    int // pixels identical in all four channels
	rounding int // differ by at most 1 per colour channel, alpha identical
	hard     int // anything else
	alpha    int // pixels whose alpha differs at all
}

func compareRow(file *File, sheet image.Image, b image.Rectangle, row int) comparison {
	var c comparison
	for i, frame := range file.Frames {
		for y := 0; y < file.Height; y++ {
			for x := 0; x < file.Width; x++ {
				fr, fg, fb, fa := frame.Image.At(x, y).RGBA()
				sr, sg, sb, sa := sheet.At(b.Min.X+i*file.Width+x, b.Min.Y+row*file.Height+y).RGBA()
				dr, dg, db := diff8(fr, sr), diff8(fg, sg), diff8(fb, sb)
				da := diff8(fa, sa)
				switch {
				case dr == 0 && dg == 0 && db == 0 && da == 0:
					c.exact++
				case da == 0 && dr <= 1 && dg <= 1 && db <= 1:
					c.rounding++
				default:
					c.hard++
					if da != 0 {
						c.alpha++
					}
				}
			}
		}
	}
	return c
}

// diff8 compares two 16-bit premultiplied channels at 8-bit precision, which is what
// both sides actually store.
func diff8(a, b uint32) uint32 {
	x, y := a>>8, b>>8
	if x > y {
		return x - y
	}
	return y - x
}

// vendorSheetFor maps ASEPRITE/Plant1/Attack/Plant1_Attack_front.aseprite to
// PNG/Plant1/With_shadow/Plant1_Attack_with_shadow.png — the §5.1 format-folder
// layout, which is the one this library actually contains.
func vendorSheetFor(asepritePath string) (string, bool) {
	dir, base := filepath.Split(asepritePath)
	name := strings.TrimSuffix(base, filepath.Ext(base))

	// Plant1_Attack_front -> Plant1_Attack
	idx := strings.LastIndex(name, "_")
	if idx <= 0 {
		return "", false
	}
	stem := name[:idx]

	// .../ASEPRITE/Plant1/Attack/ -> .../PNG/Plant1/
	parts := strings.Split(filepath.ToSlash(strings.TrimSuffix(dir, string(filepath.Separator))), "/")
	aseIdx := -1
	for i, p := range parts {
		if strings.EqualFold(p, "ASEPRITE") {
			aseIdx = i
		}
	}
	if aseIdx < 0 || aseIdx+1 >= len(parts) {
		return "", false
	}
	pngDir := filepath.Join(append(append([]string{}, parts[:aseIdx]...), "PNG", parts[aseIdx+1])...)
	if !strings.HasPrefix(pngDir, string(filepath.Separator)) && strings.HasPrefix(asepritePath, string(filepath.Separator)) {
		pngDir = string(filepath.Separator) + pngDir
	}

	// The shadow layer is part of the composite, so the with-shadow export is the one
	// to compare against.
	candidate := filepath.Join(pngDir, "With_shadow", stem+"_with_shadow.png")
	if _, err := os.Stat(candidate); err == nil {
		return candidate, true
	}
	return "", false
}
