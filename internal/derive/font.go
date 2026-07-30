package derive

import (
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/fixed"
)

// A specimen for a font (M15).
//
// §4 has had a `font` kind since M1 and §6 never said what to do with one, so a
// library of typefaces showed as a wall of extension chips: "how do the fonts look"
// had no answer anywhere in the UI. A font's preview is the font itself, so this
// renders one — the family name set in the face, and a pangram-ish specimen line
// under it.
//
// Pure Go: golang.org/x/image/font/sfnt parses the file and opentype rasterises it,
// so invariant 6 holds and no external tool is involved.

// fontThumb is the specimen tile's size, matching the image thumbnails' longest edge.
const (
	fontThumbW = 512
	fontThumbH = 512
)

// specimenLines are what a type specimen has to show: the alphabet in both cases, the
// digits, and enough punctuation to judge the shapes that trip a font up.
var specimenLines = []string{
	"ABCDEFGHIJKLM",
	"NOPQRSTUVWXYZ",
	"abcdefghijklm",
	"nopqrstuvwxyz",
	"0123456789",
	"& ? ! @ # % — “ ”",
}

// isFontExt reports whether Generate should take the font path.
func isFontExt(ext string) bool {
	switch strings.ToLower(ext) {
	case "ttf", "otf", "ttc":
		return true
	default:
		// woff/woff2 are compressed wrappers sfnt cannot read, and .fnt is a bitmap
		// format from another era. Both are recorded as unsupported with a reason
		// rather than guessed at (§6).
		return false
	}
}

// deriveFont renders a specimen tile for a font file.
func deriveFont(opts GenerateOptions) (*Result, error) {
	data, err := os.ReadFile(opts.AbsPath)
	if err != nil {
		return nil, fmt.Errorf("read font: %w", err)
	}

	sf, err := sfnt.Parse(data)
	if err != nil {
		// A collection (.ttc) holds several faces; take the first, which is what every
		// preview does.
		if coll, collErr := sfnt.ParseCollection(data); collErr == nil && coll.NumFonts() > 0 {
			if first, faceErr := coll.Font(0); faceErr == nil {
				sf = first
			} else {
				return nil, fmt.Errorf("%w: %v", ErrUnsupported, faceErr)
			}
		} else {
			return nil, fmt.Errorf("%w: %v", ErrUnsupported, err)
		}
	}

	name, _ := sf.Name(nil, sfnt.NameIDTypographicFamily)
	if name == "" {
		name, _ = sf.Name(nil, sfnt.NameIDFamily)
	}

	relDir, err := Dir(opts.SHA256)
	if err != nil {
		return nil, err
	}
	outDir := filepath.Join(opts.DataRoot, relDir)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, fmt.Errorf("create derivative directory: %w", err)
	}

	img, err := renderSpecimen(sf, name)
	if err != nil {
		return nil, err
	}
	if err := encodeWebP(filepath.Join(outDir, FileThumb), img); err != nil {
		return nil, err
	}
	// The same image serves as the larger preview: a specimen is already the whole
	// point, and re-rendering it at another size would say nothing new.
	if err := encodeWebP(filepath.Join(outDir, FilePreview), img); err != nil {
		return nil, err
	}

	notes := []string{}
	if name != "" {
		notes = append(notes, "family: "+name)
	}
	return &Result{
		Files: []string{FileThumb, FilePreview},
		Notes: notes,
	}, nil
}

// renderSpecimen draws the family name and the specimen lines using the font itself.
func renderSpecimen(sf *sfnt.Font, family string) (image.Image, error) {
	img := image.NewRGBA(image.Rect(0, 0, fontThumbW, fontThumbH))
	background := color.RGBA{R: 0x2a, G: 0x2e, B: 0x36, A: 0xff}
	draw.Draw(img, img.Bounds(), &image.Uniform{background}, image.Point{}, draw.Src)

	ink := image.NewUniform(color.RGBA{R: 0xe4, G: 0xe6, B: 0xeb, A: 0xff})
	muted := image.NewUniform(color.RGBA{R: 0x9a, G: 0xa3, B: 0xb2, A: 0xff})

	// The big "Aa" first: it is what a person scans a font list for.
	if err := drawText(img, sf, ink, "Aa", 150, 30, 150); err != nil {
		return nil, err
	}
	// Then the family name, in the font, so the name and its shapes agree.
	if family != "" {
		if err := drawText(img, sf, muted, family, 26, 30, 200); err != nil {
			return nil, err
		}
	}

	y := 250
	for _, line := range specimenLines {
		if err := drawText(img, sf, ink, line, 30, 30, y); err != nil {
			// A line the font cannot set (missing glyphs) is skipped rather than fatal:
			// an icon font has no lower case, and that is still a useful specimen.
			continue
		}
		y += 42
		if y > fontThumbH-10 {
			break
		}
	}
	return img, nil
}

// drawText sets one line with the font's own face.
func drawText(dst draw.Image, sf *sfnt.Font, src image.Image, text string, size float64, x, y int) error {
	face, err := opentype.NewFace(sf, &opentype.FaceOptions{
		Size:    size,
		DPI:     72,
		Hinting: font.HintingFull,
	})
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnsupported, err)
	}
	defer face.Close() //nolint:errcheck

	d := font.Drawer{
		Dst:  dst,
		Src:  src,
		Face: face,
		Dot:  fixed.P(x, y),
	}
	// A font with no glyph for any of these characters draws nothing at all, which
	// would leave an empty tile claiming to be a specimen.
	if d.MeasureString(text) == 0 {
		return errors.New("the font has no glyphs for this text")
	}
	d.DrawString(text)
	return nil
}
