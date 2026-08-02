package derive

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"image/gif"

	// Registered for image.Decode / image.DecodeConfig.
	_ "image/jpeg"
	_ "image/png"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"

	"github.com/oov/psd"
	"github.com/srwiley/oksvg"
	"github.com/srwiley/rasterx"

	"github.com/datcal/ambar/internal/aseprite"
)

// ErrUnsupported means there is no decoder for this input. It is not a failure: the
// asset is recorded as derive_state=unsupported, which the UI shows plainly, and
// retrying it without a code change would be pointless.
var ErrUnsupported = errors.New("no decoder for this format")

// Source is a decoded image, possibly animated.
type Source struct {
	// Frames always holds at least one image.
	Frames []image.Image
	// Delays is per-frame, empty for a still image.
	Delays []time.Duration
	// AnimationNames comes from Aseprite frame tags (§6 maps them onto
	// animation_names). Empty for every other format.
	AnimationNames []string
	// LayerNames is informational, from Aseprite and PSD.
	LayerNames []string
	// Notes records decisions worth telling the user about — an unsupported blend
	// mode, a frame cap that was hit.
	Notes []string
}

// First is the representative frame, used for thumbnails and analysis.
func (s *Source) First() image.Image { return s.Frames[0] }

// Animated reports whether an animated preview is worth generating.
func (s *Source) Animated() bool { return len(s.Frames) > 1 }

// FPS derives a frame rate from the recorded delays. Zero when unknown.
//
// The mean rather than the first delay: Aseprite lets every frame have its own
// duration, and a single long hold frame should not decide the whole rate.
func (s *Source) FPS() float64 {
	if len(s.Delays) == 0 {
		return 0
	}
	var total time.Duration
	for _, d := range s.Delays {
		total += d
	}
	if total <= 0 {
		return 0
	}
	mean := total / time.Duration(len(s.Delays))
	if mean <= 0 {
		return 0
	}
	return float64(time.Second) / float64(mean)
}

// MaxFrames caps animated output. §6: "Keep animation in GIF/WebP thumbnails, capped
// at a sane frame count." A 400-frame cutscene export must not become a 400-frame GIF.
const MaxFrames = 60

// DecodeOptions configures Decode.
type DecodeOptions struct {
	// MaxPixels refuses an image whose header advertises more pixels than this.
	// Decoding allocates roughly width*height*4 bytes, so a 30000x30000 PNG is an
	// out-of-memory on a NAS — the image equivalent of §5's zip-bomb caps. Checked
	// through DecodeConfig, before anything is allocated.
	MaxPixels int64
}

// DefaultMaxPixels is 50 megapixels: comfortably above any real 8K texture, far below
// what would exhaust a NAS.
const DefaultMaxPixels int64 = 50_000_000

// Decode reads an asset into frames, dispatching on extension.
//
// absPath must already have been through safepath. ext is lowercase without a dot,
// as stored in assets.ext.
func Decode(absPath, ext string, opts DecodeOptions) (*Source, error) {
	if opts.MaxPixels <= 0 {
		opts.MaxPixels = DefaultMaxPixels
	}

	switch ext {
	case "png", "jpg", "jpeg", "bmp", "tif", "tiff", "webp":
		return decodeStill(absPath, opts)
	case "gif":
		return decodeGIF(absPath, opts)
	case "psd":
		return decodePSD(absPath, opts)
	case "kra":
		return decodeKRA(absPath, opts)
	case "svg":
		return decodeSVG(absPath, opts)
	case "aseprite", "ase":
		return decodeAseprite(absPath, opts)

	case "xcf":
		// §6: "mark unsupported gracefully. Low priority."
		return nil, fmt.Errorf("%w: .xcf (GIMP) has no pure-Go decoder", ErrUnsupported)
	case "tga":
		// §6 lists tga among the image formats, but x/image has no TGA decoder and
		// the only pure-Go options were last touched in 2015. Surfaced as
		// unsupported rather than silently skipped; a minimal decoder is a contained
		// follow-up. See docs/spec.md.
		return nil, fmt.Errorf("%w: .tga decoding is not implemented yet", ErrUnsupported)
	case "hdr", "exr":
		// §6 groups HDRI tone-mapping with the 3D work, which is M6.
		return nil, fmt.Errorf("%w: HDRI tone-mapping arrives in M6", ErrUnsupported)
	}
	return nil, fmt.Errorf("%w: .%s", ErrUnsupported, ext)
}

// checkPixelBudget reads only the header and refuses anything oversized.
func checkPixelBudget(r io.Reader, maxPixels int64) (io.Reader, error) {
	// The header has to be replayed for the real decode, so buffer what DecodeConfig
	// consumes rather than seeking — this also works for a non-seekable source.
	var head bytes.Buffer
	cfg, _, err := image.DecodeConfig(io.TeeReader(r, &head))
	if err != nil {
		// Unreadable header: let the real decoder produce the better message.
		return io.MultiReader(bytes.NewReader(head.Bytes()), r), nil //nolint:nilerr
	}
	if px := int64(cfg.Width) * int64(cfg.Height); px > maxPixels {
		return nil, fmt.Errorf("%w: %dx%d is %d pixels, over the %d limit (AMBAR_MAX_IMAGE_PIXELS)",
			ErrUnsupported, cfg.Width, cfg.Height, px, maxPixels)
	}
	return io.MultiReader(bytes.NewReader(head.Bytes()), r), nil
}

func decodeStill(absPath string, opts DecodeOptions) (*Source, error) {
	f, err := os.Open(absPath)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", filepath.Base(absPath), err)
	}
	defer f.Close()

	guarded, err := checkPixelBudget(f, opts.MaxPixels)
	if err != nil {
		return nil, err
	}

	img, _, err := image.Decode(guarded)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", filepath.Base(absPath), err)
	}
	return &Source{Frames: []image.Image{img}}, nil
}

// decodeGIF keeps animation, which §6 asks for.
//
// GIF frames are deltas over a persistent canvas, so each one is composited onto the
// accumulated result rather than used directly — otherwise every frame after the first
// is a fragment on transparency.
func decodeGIF(absPath string, opts DecodeOptions) (*Source, error) {
	f, err := os.Open(absPath)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", filepath.Base(absPath), err)
	}
	defer f.Close()

	guarded, err := checkPixelBudget(f, opts.MaxPixels)
	if err != nil {
		return nil, err
	}

	decoded, err := gif.DecodeAll(guarded)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", filepath.Base(absPath), err)
	}

	src := &Source{}
	canvas := image.NewRGBA(image.Rect(0, 0, decoded.Config.Width, decoded.Config.Height))

	for i, frame := range decoded.Image {
		if i >= MaxFrames {
			src.Notes = append(src.Notes,
				fmt.Sprintf("animation truncated to %d of %d frames", MaxFrames, len(decoded.Image)))
			break
		}
		draw.Draw(canvas, frame.Bounds(), frame, frame.Bounds().Min, draw.Over)

		// Snapshot: the canvas keeps mutating for later frames.
		snapshot := image.NewRGBA(canvas.Bounds())
		copy(snapshot.Pix, canvas.Pix)
		src.Frames = append(src.Frames, snapshot)

		// GIF delays are hundredths of a second, and 0 conventionally means 100ms.
		delay := time.Duration(decoded.Delay[i]) * 10 * time.Millisecond
		if delay <= 0 {
			delay = 100 * time.Millisecond
		}
		src.Delays = append(src.Delays, delay)
	}

	if len(src.Frames) == 0 {
		return nil, fmt.Errorf("%s contains no frames", filepath.Base(absPath))
	}
	return src, nil
}

// decodePSD reads a PSD. §6 names github.com/oov/psd for this.
//
// It used to read only the flattened composite (`SkipLayerImage: true`), and for the
// library that is here that produced a sprite sitting in an opaque white box: vendor PSDs
// — CraftPix's, for one — ship a filled `Background` layer at the bottom, so the composite
// Photoshop wrote has no alpha at all. On a checkerboard the result looks broken, and it
// makes the PSD variant of an asset look like a different, worse image than its PNG.
//
// So the layers are read and flattened here instead, skipping a bottom background layer,
// with the composite kept as the fallback for the files that have no layer data (a PSD
// saved flattened, or one where the layer section is unreadable).
func decodePSD(absPath string, opts DecodeOptions) (*Source, error) {
	f, err := os.Open(absPath)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", filepath.Base(absPath), err)
	}
	defer f.Close()

	// psd.Decode has no header-only mode, so the budget is enforced from the parsed
	// config instead — still before any resize or encode work.
	img, _, err := psd.Decode(f, &psd.DecodeOptions{})
	if err != nil {
		return nil, fmt.Errorf("decode PSD %s: %w", filepath.Base(absPath), err)
	}

	canvas := image.Rect(0, 0, img.Config.Rect.Dx(), img.Config.Rect.Dy())
	if px := int64(canvas.Dx()) * int64(canvas.Dy()); px > opts.MaxPixels {
		return nil, fmt.Errorf("%w: PSD is %dx%d, over the pixel limit",
			ErrUnsupported, canvas.Dx(), canvas.Dy())
	}

	src := &Source{}
	for _, layer := range img.Layer {
		if name := strings.TrimSpace(layer.Name); name != "" {
			src.LayerNames = append(src.LayerNames, name)
		}
	}

	if flat := flattenPSD(img.Layer, canvas); flat != nil {
		src.Frames = []image.Image{flat}
		return src, nil
	}

	if img.Picker == nil {
		return nil, fmt.Errorf("PSD %s has neither layers nor a composite image", filepath.Base(absPath))
	}
	src.Frames = []image.Image{img.Picker}
	src.Notes = append(src.Notes, "PSD has no usable layer data; the flattened composite was used")
	return src, nil
}

// flattenPSD draws the visible layers over each other, bottom to top, and returns nil when
// there is nothing to draw.
//
// Deliberately simple: source-over with the layer's opacity, no blend modes, no clipping
// groups, no adjustment layers. That is the same compromise the Aseprite decoder documents
// — enough for the sprite work this library holds, and honest about what it skips rather
// than pretending to be Photoshop. A file that needs real blending still has its own
// composite, and opening it in Photoshop is one copy away.
func flattenPSD(layers []psd.Layer, canvas image.Rectangle) image.Image {
	out := image.NewNRGBA(canvas)
	drawn := false

	var walk func(ls []psd.Layer, depth int)
	walk = func(ls []psd.Layer, depth int) {
		for i := range ls {
			layer := &ls[i]
			if !layer.Visible() {
				continue
			}
			if layer.Folder() {
				// A group's own opacity is not applied to its children here; see the note
				// above about what this flattener deliberately does not do.
				walk(layer.Layer, depth+1)
				continue
			}
			if !layer.HasImage() || layer.Picker == nil {
				continue
			}
			// The bottom-most opaque layer that covers the whole canvas is a background,
			// and it is the reason the composite has no alpha. Skipping it is the whole
			// point of flattening here.
			if depth == 0 && i == 0 && isPSDBackground(layer, canvas) {
				continue
			}
			drawLayerOver(out, layer)
			drawn = true
		}
	}
	walk(layers, 0)

	if !drawn {
		return nil
	}
	return out
}

// isPSDBackground reports whether the bottom-most layer is a filled backdrop rather than
// artwork: it covers the whole canvas and is fully opaque across it.
//
// Measured rather than guessed from the name or from the channel list. The real files
// showed why both shortcuts fail: CraftPix's backdrop is a 1920x1080 leftover behind a
// 32x32 sprite — so its rect proves nothing on its own — and it carries a transparency
// channel like every other PSD layer, so "has no alpha channel" rejected it. What actually
// distinguishes a backdrop is that you cannot see through it.
//
// A false positive is cheap: skipping the only layer leaves nothing drawn, and decodePSD
// falls back to the flattened composite.
func isPSDBackground(layer *psd.Layer, canvas image.Rectangle) bool {
	if !layer.Rect.Union(canvas).Eq(layer.Rect) {
		return false // does not cover the canvas
	}
	if layer.Opacity < 0xff {
		return false
	}

	// Sample rather than read every pixel: a 1920x1080 backdrop behind a 32x32 sprite
	// would otherwise cost two million reads to answer a yes/no question.
	const samples = 32
	stepX := max(1, canvas.Dx()/samples)
	stepY := max(1, canvas.Dy()/samples)
	for y := canvas.Min.Y; y < canvas.Max.Y; y += stepY {
		for x := canvas.Min.X; x < canvas.Max.X; x += stepX {
			if _, _, _, a := layer.Picker.At(x, y).RGBA(); a < 0xffff {
				return false
			}
		}
	}
	return true
}

// drawLayerOver composites one layer onto dst with its opacity.
func drawLayerOver(dst *image.NRGBA, layer *psd.Layer) {
	if layer.Opacity == 0 {
		return
	}
	if layer.Opacity == 0xff {
		draw.Draw(dst, layer.Rect, layer.Picker, layer.Rect.Min, draw.Over)
		return
	}
	mask := image.NewUniform(color.Alpha{A: layer.Opacity})
	draw.DrawMask(dst, layer.Rect, layer.Picker, layer.Rect.Min, mask, image.Point{}, draw.Over)
}

// decodeKRA pulls Krita's pre-rendered composite out of the archive.
//
// §6: "it is a zip with mergedimage.png already inside. Trivial, no dependency."
func decodeKRA(absPath string, opts DecodeOptions) (*Source, error) {
	zr, err := zip.OpenReader(absPath)
	if err != nil {
		return nil, fmt.Errorf("open KRA %s: %w", filepath.Base(absPath), err)
	}
	defer zr.Close()

	// mergedimage.png is the flattened composite; preview.png is a small thumbnail
	// Krita also stores, used only as a fallback.
	for _, want := range []string{"mergedimage.png", "preview.png"} {
		for _, entry := range zr.File {
			if entry.Name != want {
				continue
			}
			rc, err := entry.Open()
			if err != nil {
				return nil, fmt.Errorf("open %s inside KRA: %w", want, err)
			}
			defer rc.Close()

			guarded, err := checkPixelBudget(rc, opts.MaxPixels)
			if err != nil {
				return nil, err
			}
			img, _, err := image.Decode(guarded)
			if err != nil {
				return nil, fmt.Errorf("decode %s inside KRA: %w", want, err)
			}
			return &Source{Frames: []image.Image{img}}, nil
		}
	}
	return nil, fmt.Errorf("%w: KRA %s contains no mergedimage.png",
		ErrUnsupported, filepath.Base(absPath))
}

// svgRasterSize is the longest edge an SVG is rasterised to. SVGs are resolution
// independent, so this is a choice rather than a property of the file: big enough for
// the 2D viewer to zoom into, small enough not to be wasteful.
const svgRasterSize = 1024

// decodeSVG rasterises with oksvg + rasterx, both named in §6.
func decodeSVG(absPath string, opts DecodeOptions) (*Source, error) {
	f, err := os.Open(absPath)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", filepath.Base(absPath), err)
	}
	defer f.Close()

	icon, err := oksvg.ReadIconStream(f)
	if err != nil {
		return nil, fmt.Errorf("parse SVG %s: %w", filepath.Base(absPath), err)
	}

	w, h := icon.ViewBox.W, icon.ViewBox.H
	if w <= 0 || h <= 0 {
		return nil, fmt.Errorf("%w: SVG %s has no usable viewBox",
			ErrUnsupported, filepath.Base(absPath))
	}

	// Scale the longest edge to svgRasterSize, preserving aspect ratio.
	scale := float64(svgRasterSize) / max(w, h)
	outW, outH := int(w*scale), int(h*scale)
	if outW < 1 {
		outW = 1
	}
	if outH < 1 {
		outH = 1
	}
	if px := int64(outW) * int64(outH); px > opts.MaxPixels {
		return nil, fmt.Errorf("%w: SVG rasterises to %dx%d, over the pixel limit",
			ErrUnsupported, outW, outH)
	}

	icon.SetTarget(0, 0, float64(outW), float64(outH))
	img := image.NewRGBA(image.Rect(0, 0, outW, outH))
	scanner := rasterx.NewScannerGV(outW, outH, img, img.Bounds())
	icon.Draw(rasterx.NewDasher(outW, outH, scanner), 1.0)

	return &Source{Frames: []image.Image{img}}, nil
}

// decodeAseprite uses the decoder written for §6's "first-class, not a nice-to-have"
// requirement. See internal/aseprite.
func decodeAseprite(absPath string, opts DecodeOptions) (*Source, error) {
	raw, err := os.ReadFile(absPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", filepath.Base(absPath), err)
	}

	file, err := aseprite.Decode(raw, aseprite.Options{MaxPixels: opts.MaxPixels})
	if err != nil {
		return nil, err
	}

	src := &Source{
		AnimationNames: file.TagNames(),
		LayerNames:     file.LayerNames(),
		Notes:          file.Notes,
	}
	for i, frame := range file.Frames {
		if i >= MaxFrames {
			src.Notes = append(src.Notes,
				fmt.Sprintf("animation truncated to %d of %d frames", MaxFrames, len(file.Frames)))
			break
		}
		src.Frames = append(src.Frames, frame.Image)
		src.Delays = append(src.Delays, frame.Duration)
	}
	if len(src.Frames) == 0 {
		return nil, fmt.Errorf("%s contains no frames", filepath.Base(absPath))
	}
	return src, nil
}
