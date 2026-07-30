package derive

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	stdpalette "image/color/palette"
	"image/draw"
	"image/gif"
	"os"
	"path/filepath"

	"github.com/HugoSmits86/nativewebp"
	xdraw "golang.org/x/image/draw"

	"github.com/datcal/ambar/internal/audio"
	"github.com/datcal/ambar/internal/model"
	"github.com/datcal/ambar/internal/palette"
	"github.com/datcal/ambar/internal/spritesheet"
)

// SheetInfo is a proposed spritesheet grid (§6). Source is "detected" here; the
// UI promotes it to "manual" on confirmation or correction.
type SheetInfo struct {
	Cols       int
	Rows       int
	FrameW     int
	FrameH     int
	FrameCount int
	Source     string
	Confident  bool
}

// Version is the derivative algorithm version.
//
// §4: "when the thumbnail algorithm improves, bump the version and only stale
// derivatives regenerate. Without it, every improvement means manually re-triggering
// twenty thousand files." Bump this whenever the output of this package changes in a
// way that should be visible on existing assets.
//
// v2 (M5): audio assets, previously recorded unsupported, now derive peaks and
// metadata — so every asset is reconsidered once.
// v3 (M6): glTF/GLB models derive preview.glb and metadata.
// v4 (M7): still images are checked for a spritesheet grid.
// v5 (M11.5): still images extract a colour palette (palette_json, palette_kind).
const Version = 5

// Thumbnail sizes. 512 is what §3's layout specifies; @2x covers a high-DPI display.
const (
	ThumbSize   = 512
	Thumb2xSize = 1024
	// PreviewSize bounds the image the 2D viewer loads. Large enough that §8's 800%
	// zoom on a sprite is still looking at real pixels, small enough not to ship a
	// 4096px texture to the browser.
	PreviewSize = 2048
)

// Derivative filenames within a content-hash directory (§3).
const (
	FileThumb      = "thumb.webp"
	FileThumb2x    = "thumb@2x.webp"
	FileThumbAlpha = "thumb-alpha.webp"
	FilePreview    = "preview.webp"
	FileAnimation  = "anim.gif"
	FileSheet      = "sheet.gif" // §6 spritesheet animation preview
)

// SheetFPS is the frame rate the spritesheet preview plays at when the source
// carries none (§6). 12 fps reads as animation without being frantic.
const SheetFPS = 12

// Dir returns the derivative directory for a content hash, relative to the data root:
// derivatives/<sha[0:2]>/<sha>/ exactly as §3 lays out.
//
// Keyed on content rather than on asset id, so two identical files share one set of
// derivatives and a moved file keeps its thumbnail.
func Dir(sha256hex string) (string, error) {
	// Validated because this becomes a filesystem path. A hash is 64 hex characters
	// and nothing else, so anything failing that is either a bug or tampering.
	if len(sha256hex) != 64 {
		return "", fmt.Errorf("content hash %q is not 64 characters", sha256hex)
	}
	if _, err := hex.DecodeString(sha256hex); err != nil {
		return "", fmt.Errorf("content hash %q is not hexadecimal", sha256hex)
	}
	return filepath.Join("derivatives", sha256hex[:2], sha256hex), nil
}

// Result is what generating derivatives produced.
type Result struct {
	Analysis Analysis
	// PHash is the 64-bit perceptual hash as 16 hex characters. Written in M2 and
	// read by M13's near-duplicate view; see docs/decisions.md §15.4 for why it is
	// computed here rather than later.
	PHash string

	FrameCount int
	FPS        float64
	// AnimationNames comes from Aseprite frame tags (§6).
	AnimationNames []string

	// Audio is set only for sound files (§6): the deriver takes a separate path
	// that writes peaks.json and fills the §4 audio columns instead of an image.
	Audio *audio.Info

	// Model is set only for 3D assets (§6): a separate path that writes preview.glb
	// and fills the §4 model columns.
	Model *model.Info

	// Sheet is set when a still image is detected as a spritesheet (§6): the
	// proposed grid, filling the §4 frame columns. Confident guesses also get a
	// sheet.gif preview.
	Sheet *SheetInfo

	// Palette is the extracted colour palette (§8), set for every decoded still
	// image. It is analysis, not a file, so it fills palette_json / palette_kind
	// rather than living under the derivative directory.
	Palette *palette.Palette

	// Files written, relative to the derivative directory.
	Files []string
	// Notes from the decoder, worth surfacing rather than swallowing.
	Notes []string
}

// GenerateOptions configures Generate.
type GenerateOptions struct {
	// AbsPath is the original file, already resolved through safepath.
	AbsPath string
	// Ext is the lowercase extension without a dot.
	Ext string
	// SHA256 keys the output directory.
	SHA256 string
	// DataRoot is where derivatives/ lives.
	DataRoot string
	// MaxPixels guards against a decompression bomb.
	MaxPixels int64
	// BlenderBin is the optional external Blender CLI for FBX/.blend (§6). Empty
	// means Blender is unavailable, and those formats stay needs_blender.
	BlenderBin string
}

// Generate decodes an asset and writes its derivatives.
//
// Returns an error wrapping ErrUnsupported when there is no decoder, which the caller
// records as derive_state=unsupported rather than as a failure.
func Generate(opts GenerateOptions) (*Result, error) {
	// Audio and 3D each take a wholly separate path: no image is decoded, and the
	// peaks.json / preview.glb plus their §4 columns are produced instead (§6).
	if isAudioExt(opts.Ext) {
		return deriveAudio(opts)
	}
	if isModelExt(opts.Ext) {
		return deriveModel(opts)
	}

	src, err := Decode(opts.AbsPath, opts.Ext, DecodeOptions{MaxPixels: opts.MaxPixels})
	if err != nil {
		return nil, err
	}

	relDir, err := Dir(opts.SHA256)
	if err != nil {
		return nil, err
	}
	outDir := filepath.Join(opts.DataRoot, relDir)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, fmt.Errorf("create derivative directory: %w", err)
	}

	first := src.First()
	analysis := Analyse(first)

	// The colour palette (§8), from the first frame — the same image the still
	// thumbnail shows. Exact for a sprite's hand-picked colours, quantized for a
	// photographic texture; the UI labels which.
	pal := palette.Extract(first, palette.Options{})

	result := &Result{
		Analysis:       analysis,
		PHash:          PerceptualHash(first),
		FrameCount:     len(src.Frames),
		FPS:            src.FPS(),
		AnimationNames: src.AnimationNames,
		Palette:        &pal,
		Notes:          src.Notes,
	}

	// The thumbnail set. thumb.webp is composited over mid-grey and thumb-alpha.webp
	// keeps transparency — §6 wants both, "so alpha-heavy sprites are visible in a
	// dark UI, with the transparent version also available".
	for _, want := range []struct {
		name        string
		size        int
		compositeBG bool
	}{
		{FileThumb, ThumbSize, true},
		{FileThumb2x, Thumb2xSize, true},
		{FileThumbAlpha, ThumbSize, false},
		{FilePreview, PreviewSize, false},
	} {
		scaled := Fit(first, want.size, analysis.IsPixelArt)
		if want.compositeBG {
			scaled = CompositeOver(scaled, MidGrey)
		}
		if err := writeWebP(filepath.Join(outDir, want.name), scaled); err != nil {
			return nil, err
		}
		result.Files = append(result.Files, want.name)
	}

	// §6: "Keep animation in GIF/WebP thumbnails, capped at a sane frame count."
	// GIF because nativewebp is lossless-still only, and §6 explicitly allows it.
	if src.Animated() {
		if err := writeAnimatedGIF(filepath.Join(outDir, FileAnimation), src, analysis.IsPixelArt); err != nil {
			// A failed animation must not lose the still thumbnails that already
			// worked, so this is a note rather than an error.
			result.Notes = append(result.Notes, "animated preview could not be written: "+err.Error())
		} else {
			result.Files = append(result.Files, FileAnimation)
		}
	}

	// §6 spritesheet detection: for a still image whose name or dimensions suggest
	// a sheet, propose a frame grid; a confident guess also gets an animated
	// preview built from its cells. A guess is never trusted silently — it is
	// recorded as frame_source=detected for the UI to confirm (§6).
	if !src.Animated() {
		b := first.Bounds()
		if spritesheet.IsCandidate(filepath.Base(opts.AbsPath), b.Dx(), b.Dy()) {
			if g, ok := spritesheet.Detect(first); ok {
				result.Sheet = &SheetInfo{
					Cols: g.Cols, Rows: g.Rows, FrameW: g.FrameW, FrameH: g.FrameH,
					FrameCount: g.FrameCount, Source: "detected", Confident: g.Confident,
				}
				if g.Confident {
					if err := writeSheetGIF(filepath.Join(outDir, FileSheet), first, g, analysis.IsPixelArt); err != nil {
						result.Notes = append(result.Notes, "sheet preview could not be written: "+err.Error())
					} else {
						result.Files = append(result.Files, FileSheet)
					}
				}
			}
		}
	}
	return result, nil
}

// writeSheetGIF renders a spritesheet's cells as an animated GIF playing at
// SheetFPS (§6: "so the grid view shows animation instead of forty tiny
// squares"). Cells are read row-major, the reading order of a sheet.
func writeSheetGIF(path string, sheet image.Image, g spritesheet.Grid, pixelArt bool) error {
	anim := &gif.GIF{LoopCount: 0}
	delay := 100 / SheetFPS
	origin := sheet.Bounds().Min

	for r := 0; r < g.Rows; r++ {
		for c := 0; c < g.Cols; c++ {
			cell := image.NewRGBA(image.Rect(0, 0, g.FrameW, g.FrameH))
			src := image.Pt(origin.X+c*g.FrameW, origin.Y+r*g.FrameH)
			xdraw.Draw(cell, cell.Bounds(), sheet, src, xdraw.Src)

			scaled := Fit(cell, ThumbSize, pixelArt)
			flattened := CompositeOver(scaled, MidGrey)
			bounds := flattened.Bounds()
			paletted := image.NewPaletted(bounds, nil)
			paletted.Palette = stdpalette.Plan9
			xdraw.Draw(paletted, bounds, flattened, bounds.Min, xdraw.Src)

			anim.Image = append(anim.Image, paletted)
			anim.Delay = append(anim.Delay, delay)
		}
	}
	if len(anim.Image) == 0 {
		return fmt.Errorf("no frames to write")
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, anim); err != nil {
		return fmt.Errorf("encode sheet GIF: %w", err)
	}
	return writeFileAtomic(path, buf.Bytes())
}

// Fit scales an image so its longest edge is at most size, preserving aspect ratio.
//
// This is the line §6 is emphatic about: "Nearest-neighbour resize when the image is
// pixel art. ... Bilinear-downscaling pixel art into mush is the single most annoying
// failure of every existing tool; get this right."
//
// An image already within the bound is returned untouched — upscaling a 32x32 sprite to
// 512x512 on the server would waste 250x the bytes to show the same information, and
// the viewer can scale it with image-rendering: pixelated instead (§8).
func Fit(src image.Image, size int, pixelArt bool) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return src
	}
	if w <= size && h <= size {
		return src
	}

	scale := float64(size) / float64(max(w, h))
	outW := max(1, int(float64(w)*scale))
	outH := max(1, int(float64(h)*scale))

	dst := image.NewRGBA(image.Rect(0, 0, outW, outH))

	if pixelArt {
		// NearestNeighbor keeps every surviving pixel exactly as authored. Any
		// smoothing filter averages neighbours, which on pixel art destroys both the
		// palette and the hard edges that define it.
		xdraw.NearestNeighbor.Scale(dst, dst.Bounds(), src, b, xdraw.Over, nil)
	} else {
		// CatmullRom for photographic content, where it is clearly the best of the
		// x/image filters for downscaling.
		xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, b, xdraw.Over, nil)
	}
	return dst
}

// CompositeOver flattens an image onto a solid background.
//
// §6: "Composite thumbnails over mid-grey so alpha-heavy sprites are visible in a dark
// UI." A white sprite on a dark theme is otherwise invisible, and so is a black one on
// a light theme.
func CompositeOver(src image.Image, bg color.Color) image.Image {
	b := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(dst, dst.Bounds(), &image.Uniform{C: bg}, image.Point{}, draw.Src)
	draw.Draw(dst, dst.Bounds(), src, b.Min, draw.Over)
	return dst
}

// writeWebP encodes losslessly with nativewebp.
//
// Lossless is the right default here rather than a compromise: for pixel art, lossy
// compression destroys exactly the hard edges §6 cares about, and at thumbnail sizes
// the file is small either way. §15's encoder decision is recorded in
// docs/decisions.md.
//
// Written to a temporary file and renamed, so a crash mid-write cannot leave a
// half-encoded thumbnail that later looks valid.
func writeWebP(path string, img image.Image) error {
	var buf bytes.Buffer
	if err := nativewebp.Encode(&buf, img, nil); err != nil {
		return fmt.Errorf("encode WebP %s: %w", filepath.Base(path), err)
	}
	return writeFileAtomic(path, buf.Bytes())
}

// writeAnimatedGIF writes the animated preview.
func writeAnimatedGIF(path string, src *Source, pixelArt bool) error {
	anim := &gif.GIF{LoopCount: 0} // 0 means loop forever

	for i, frame := range src.Frames {
		scaled := Fit(frame, ThumbSize, pixelArt)
		// GIF has no partial alpha, so every frame is composited over mid-grey —
		// consistent with the still thumbnail rather than showing black fringes.
		flattened := CompositeOver(scaled, MidGrey)

		bounds := flattened.Bounds()
		paletted := image.NewPaletted(bounds, nil)
		// A fixed 256-colour palette rather than per-frame quantisation, which would
		// make the animation shimmer as colours were reassigned between frames.
		paletted.Palette = stdpalette.Plan9
		xdraw.Draw(paletted, bounds, flattened, bounds.Min, xdraw.Src)

		anim.Image = append(anim.Image, paletted)

		delay := 10 // hundredths of a second
		if i < len(src.Delays) {
			if d := int(src.Delays[i].Milliseconds() / 10); d > 0 {
				delay = d
			}
		}
		anim.Delay = append(anim.Delay, delay)
	}

	if len(anim.Image) == 0 {
		return fmt.Errorf("no frames to write")
	}

	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, anim); err != nil {
		return fmt.Errorf("encode GIF: %w", err)
	}
	return writeFileAtomic(path, buf.Bytes())
}

// writeFileAtomic writes via a temporary file and a rename.
//
// Not for the library's sake — derivatives live under the data root, and invariant 1
// is about originals — but because a partially written thumbnail is indistinguishable
// from a complete one, and would be served as a broken image forever.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(path)+"-*")
	if err != nil {
		return fmt.Errorf("create temporary file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) //nolint:errcheck // no-op once renamed

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", tmpName, err)
	}
	// fsync before rename: a NAS losing power mid-write should not leave a renamed
	// but empty file.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename into place %s: %w", path, err)
	}
	return nil
}

// PerceptualHash computes a 64-bit dHash, returned as 16 hex characters.
//
// dHash rather than a DCT-based pHash: it compares each pixel with its right-hand
// neighbour on a 9x8 greyscale reduction, which makes it robust to scaling and mild
// recompression — the "same sprite pack downloaded twice at different resolutions"
// case §5 describes — while being about thirty lines instead of a dependency.
//
// Written here rather than as a library call because it is small enough to test
// properly and one fewer dependency matters more than the code saved.
func PerceptualHash(img image.Image) string {
	const (
		w = 9 // one extra column: 8 comparisons per row
		h = 8
	)

	// Reduce to 9x8 greyscale. Smoothing is wanted here, even for pixel art: the
	// point is a perceptual summary, not a faithful thumbnail.
	small := image.NewRGBA(image.Rect(0, 0, w, h))
	xdraw.CatmullRom.Scale(small, small.Bounds(), img, img.Bounds(), xdraw.Over, nil)

	var grey [h][w]uint32
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			r, g, b, _ := small.At(x, y).RGBA()
			// Rec. 601 luma, which is what human brightness perception approximates.
			grey[y][x] = (299*r + 587*g + 114*b) / 1000
		}
	}

	var hash uint64
	bit := 0
	for y := 0; y < h; y++ {
		for x := 0; x < w-1; x++ {
			if grey[y][x] > grey[y][x+1] {
				hash |= 1 << uint(63-bit)
			}
			bit++
		}
	}

	var out [8]byte
	for i := 0; i < 8; i++ {
		out[i] = byte(hash >> uint(56-8*i))
	}
	return hex.EncodeToString(out[:])
}

// HammingDistance counts differing bits between two hex perceptual hashes.
//
// Unused in M2 — M13's near-duplicate view is its consumer — but it lives beside
// PerceptualHash because the two only make sense together, and having it tested now
// means M13 inherits a verified primitive.
func HammingDistance(a, b string) (int, error) {
	ab, err := hex.DecodeString(a)
	if err != nil || len(ab) != 8 {
		return 0, fmt.Errorf("%q is not a 64-bit hex hash", a)
	}
	bb, err := hex.DecodeString(b)
	if err != nil || len(bb) != 8 {
		return 0, fmt.Errorf("%q is not a 64-bit hex hash", b)
	}

	distance := 0
	for i := range ab {
		diff := ab[i] ^ bb[i]
		for diff != 0 {
			distance += int(diff & 1)
			diff >>= 1
		}
	}
	return distance, nil
}

// ContentHash is sha256 of a byte slice as hex, for tests and for callers that already
// hold the bytes.
func ContentHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
