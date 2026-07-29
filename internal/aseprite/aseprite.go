// Package aseprite decodes Aseprite's native binary format.
//
// §6 makes this first-class rather than a nice-to-have: "The target library already
// ships ASEPRITE/ folders and their share will grow, so this is a primary editable
// source format rather than an exotic case."
//
// Written rather than depended on because nothing reads the binary format. The one
// obvious candidate, github.com/solarlune/goaseprite, parses Aseprite's *JSON export*
// — which requires the user to have exported a spritesheet first, defeating the whole
// point. §6 is explicit about the goal: "an animated preview can be generated from the
// .aseprite itself without needing the exported PNG sequence".
//
// Implemented against the documented format (ase-file-specs). What is supported:
//
//   - Colour depths 32 (RGBA), 16 (grayscale+alpha) and 8 (indexed). Indexed matters
//     most — it is what pixel artists mostly work in.
//   - Cel types 0 (raw), 1 (linked) and 2 (zlib compressed).
//   - Layer visibility and opacity, cel opacity, and the Normal blend mode.
//   - Frame durations, which give a real fps.
//   - Frame tags, which §6 maps directly onto animation_names.
//
// What is not, and is reported through Notes rather than silently mishandled: blend
// modes other than Normal (treated as Normal), and tilemap cels and tilesets, which
// belong with §5.1's .tmx work.
//
// Every length read from the file is bounds-checked before use. A malformed file must
// produce an error, never a panic or a gigabyte allocation — this parser reads
// untrusted input from a library of files downloaded from the internet.
package aseprite

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"io"
	"time"
)

// ErrNotAseprite means the magic number is wrong: this is not an Aseprite file.
var ErrNotAseprite = errors.New("not an Aseprite file")

// ErrMalformed means the file is Aseprite-shaped but internally inconsistent.
var ErrMalformed = errors.New("malformed Aseprite file")

// ErrUnsupportedFeature means a valid file uses something this decoder does not
// implement, such as a tilemap cel.
var ErrUnsupportedFeature = errors.New("unsupported Aseprite feature")

// File format constants from ase-file-specs.
const (
	headerSize      = 128
	headerMagic     = 0xA5E0
	frameHeaderSize = 16
	frameMagic      = 0xF1FA

	chunkOldPalette4  = 0x0004
	chunkOldPalette11 = 0x0011
	chunkLayer        = 0x2004
	chunkCel          = 0x2005
	chunkCelExtra     = 0x2006
	chunkColorProfile = 0x2007
	chunkTags         = 0x2018
	chunkPalette      = 0x2019
	chunkUserData     = 0x2020
	chunkSlice        = 0x2022
	chunkTileset      = 0x2023

	celTypeRaw               = 0
	celTypeLinked            = 1
	celTypeCompressedImage   = 2
	celTypeCompressedTilemap = 3

	layerTypeNormal  = 0
	layerTypeGroup   = 1
	layerTypeTilemap = 2

	layerFlagVisible = 1

	blendModeNormal = 0
)

// Options configures Decode.
type Options struct {
	// MaxPixels refuses a file whose canvas exceeds this, before allocating for it.
	MaxPixels int64
	// MaxFrames stops decoding after this many frames. Zero means no limit.
	MaxFrames int
}

// DefaultMaxPixels matches the derive package's default.
const DefaultMaxPixels int64 = 50_000_000

// Frame is one composited frame.
type Frame struct {
	Image    image.Image
	Duration time.Duration
}

// Layer is one layer's metadata.
type Layer struct {
	Name       string
	Visible    bool
	Opacity    uint8
	BlendMode  uint16
	Type       uint16
	ChildLevel uint16
}

// Tag is an Aseprite frame tag. §6: "Frame tags map directly onto animation_names."
type Tag struct {
	Name string
	From int
	To   int
}

// File is a decoded Aseprite document.
type File struct {
	Width  int
	Height int
	// ColorDepth is 32, 16 or 8 bits per pixel.
	ColorDepth int
	Frames     []Frame
	Layers     []Layer
	Tags       []Tag

	// Notes records anything the caller should know about how faithfully this was
	// decoded — an approximated blend mode, a skipped tilemap layer. Surfaced rather
	// than swallowed, because a silently wrong composite is worse than a flagged one.
	Notes []string
}

// TagNames returns the frame-tag names, for assets.animation_names.
func (f *File) TagNames() []string {
	out := make([]string, 0, len(f.Tags))
	for _, t := range f.Tags {
		if t.Name != "" {
			out = append(out, t.Name)
		}
	}
	return out
}

// LayerNames returns the layer names, outermost first.
func (f *File) LayerNames() []string {
	out := make([]string, 0, len(f.Layers))
	for _, l := range f.Layers {
		if l.Name != "" {
			out = append(out, l.Name)
		}
	}
	return out
}

// cel is one layer's pixels within one frame, before compositing.
type cel struct {
	layerIndex int
	x, y       int
	opacity    uint8
	// img is nil for a linked cel, which borrows another frame's pixels.
	img *image.RGBA
	// linkedFrame is the frame a linked cel borrows from.
	linkedFrame int
	linked      bool
}

// Decode parses an Aseprite file and composites every frame.
func Decode(data []byte, opts Options) (*File, error) {
	if opts.MaxPixels <= 0 {
		opts.MaxPixels = DefaultMaxPixels
	}

	r := &reader{data: data}

	// --- header (128 bytes) ---

	if len(data) < headerSize {
		return nil, fmt.Errorf("%w: %d bytes is shorter than the 128-byte header",
			ErrNotAseprite, len(data))
	}
	r.skip(4) // file size, not trusted — the slice length is the truth
	magic, err := r.uint16()
	if err != nil {
		return nil, err
	}
	if magic != headerMagic {
		return nil, fmt.Errorf("%w: magic is 0x%04X, want 0x%04X", ErrNotAseprite, magic, headerMagic)
	}

	frameCount, _ := r.uint16()
	width, _ := r.uint16()
	height, _ := r.uint16()
	depth, _ := r.uint16()
	r.skip(4)                        // flags
	r.skip(2)                        // deprecated speed
	r.skip(8)                        // two reserved DWORDs
	transparentIndex, _ := r.uint8() // only meaningful for indexed colour
	r.skip(3)                        // ignored, and the palette-entry count that follows is unused:
	//           the palette chunk carries its own range.
	if err := r.err(); err != nil {
		return nil, err
	}
	// Skip to the end of the fixed-size header regardless of what was read.
	r.seek(headerSize)

	if width == 0 || height == 0 {
		return nil, fmt.Errorf("%w: canvas is %dx%d", ErrMalformed, width, height)
	}
	if px := int64(width) * int64(height); px > opts.MaxPixels {
		return nil, fmt.Errorf("%w: canvas %dx%d is %d pixels, over the limit",
			ErrUnsupportedFeature, width, height, px)
	}
	switch depth {
	case 32, 16, 8:
	default:
		return nil, fmt.Errorf("%w: colour depth %d bits", ErrUnsupportedFeature, depth)
	}

	f := &File{
		Width:      int(width),
		Height:     int(height),
		ColorDepth: int(depth),
	}

	// Palette state, shared across frames: an indexed file defines its palette once,
	// usually in frame 1.
	palette := make([]color.RGBA, 256)
	for i := range palette {
		// Default to opaque black rather than transparent, so a file with no palette
		// chunk still produces something visible rather than an empty image.
		palette[i] = color.RGBA{A: 0xff}
	}
	haveePalette := false

	// Per-frame cels, kept so linked cels can reach back into earlier frames.
	// capacityHint, because frameCount is also untrusted.
	frameCels := make([][]cel, 0, capacityHint(int(frameCount)))

	var (
		unsupportedBlend  = map[uint16]bool{}
		skippedTilemaps   int
		frameLimitReached bool
	)

	// --- frames ---

	for frameIndex := 0; frameIndex < int(frameCount); frameIndex++ {
		if opts.MaxFrames > 0 && frameIndex >= opts.MaxFrames {
			frameLimitReached = true
			break
		}
		if r.remaining() == 0 {
			// Fewer frames present than the header claimed. Tolerated when at least
			// one frame decoded: a truncated file is still worth previewing.
			if frameIndex == 0 {
				return nil, fmt.Errorf("%w: header claims %d frames but the file ends immediately",
					ErrMalformed, frameCount)
			}
			f.Notes = append(f.Notes,
				fmt.Sprintf("file ends after %d of %d declared frames", frameIndex, frameCount))
			break
		}

		frameStart := r.pos
		frameBytes, err := r.uint32()
		if err != nil {
			return nil, err
		}
		fMagic, err := r.uint16()
		if err != nil {
			return nil, err
		}
		if fMagic != frameMagic {
			return nil, fmt.Errorf("%w: frame %d magic is 0x%04X, want 0x%04X",
				ErrMalformed, frameIndex, fMagic, frameMagic)
		}

		oldChunkCount, _ := r.uint16()
		durationMS, _ := r.uint16()
		r.skip(2) // reserved
		newChunkCount, _ := r.uint32()
		if err := r.err(); err != nil {
			return nil, err
		}

		chunkCount := int(newChunkCount)
		if chunkCount == 0 {
			// The old 16-bit count is used when the 32-bit one is zero. 0xFFFF in the
			// old field means "look at the new field", which is already zero here.
			chunkCount = int(oldChunkCount)
		}

		// Frame end from the declared size, clamped to the real data.
		frameEnd := frameStart + int(frameBytes)
		if int(frameBytes) < frameHeaderSize || frameEnd > len(data) {
			frameEnd = len(data)
		}

		// The chunk count is a 32-bit field read from an untrusted file, and every
		// chunk needs at least its own 6-byte header — so a count larger than the
		// remaining bytes can hold is a lie. Clamping it here is load-bearing:
		// allocating from the raw value turned a crafted count of 4.29 billion into a
		// 240 GB allocation, which the fuzz sweep in the tests found.
		if maxPlausible := (frameEnd - r.pos) / 6; chunkCount > maxPlausible {
			chunkCount = maxPlausible
		}
		if chunkCount < 0 {
			chunkCount = 0
		}

		cels := make([]cel, 0, capacityHint(chunkCount))

		for c := 0; c < chunkCount; c++ {
			if r.pos >= frameEnd {
				break
			}
			chunkStart := r.pos
			chunkSize, err := r.uint32()
			if err != nil {
				return nil, err
			}
			chunkType, err := r.uint16()
			if err != nil {
				return nil, err
			}
			// A chunk must be at least its own 6-byte header, and must not claim to
			// extend past the frame.
			if int(chunkSize) < 6 {
				return nil, fmt.Errorf("%w: frame %d chunk %d claims %d bytes",
					ErrMalformed, frameIndex, c, chunkSize)
			}
			chunkEnd := chunkStart + int(chunkSize)
			if chunkEnd > frameEnd {
				chunkEnd = frameEnd
			}
			body := data[min(r.pos, chunkEnd):chunkEnd]

			switch chunkType {
			case chunkLayer:
				layer, err := parseLayer(body)
				if err != nil {
					return nil, err
				}
				if layer.Type == layerTypeTilemap {
					skippedTilemaps++
				}
				if layer.BlendMode != blendModeNormal {
					unsupportedBlend[layer.BlendMode] = true
				}
				f.Layers = append(f.Layers, layer)

			case chunkCel:
				parsed, err := parseCel(body, int(depth), int(width), int(height),
					opts.MaxPixels, palette, transparentIndex)
				switch {
				case errors.Is(err, ErrUnsupportedFeature):
					// A tilemap cel: skipped, counted, and reported.
					skippedTilemaps++
				case err != nil:
					return nil, err
				default:
					cels = append(cels, parsed)
				}

			case chunkPalette:
				if err := parsePalette(body, palette); err != nil {
					return nil, err
				}
				haveePalette = true

			case chunkOldPalette4, chunkOldPalette11:
				// Only used if the new-style chunk is absent; the new one is
				// authoritative when both appear.
				if !haveePalette {
					if err := parseOldPalette(body, palette); err != nil {
						return nil, err
					}
				}

			case chunkTags:
				tags, err := parseTags(body)
				if err != nil {
					return nil, err
				}
				f.Tags = append(f.Tags, tags...)

			case chunkColorProfile, chunkUserData, chunkSlice, chunkCelExtra:
				// Not needed for a composite.

			case chunkTileset:
				skippedTilemaps++
			}

			r.seek(chunkEnd)
		}

		frameCels = append(frameCels, cels)

		duration := time.Duration(durationMS) * time.Millisecond
		if duration <= 0 {
			// Aseprite's own default when a frame has no duration set.
			duration = 100 * time.Millisecond
		}

		img, err := composite(f, cels, frameCels, frameIndex)
		if err != nil {
			return nil, err
		}
		f.Frames = append(f.Frames, Frame{Image: img, Duration: duration})

		r.seek(frameEnd)
	}

	if len(f.Frames) == 0 {
		return nil, fmt.Errorf("%w: no frames could be decoded", ErrMalformed)
	}

	// Notes, so an approximation is never silent.
	for mode := range unsupportedBlend {
		f.Notes = append(f.Notes,
			fmt.Sprintf("layer blend mode %d is not implemented and was treated as Normal", mode))
	}
	if skippedTilemaps > 0 {
		f.Notes = append(f.Notes,
			fmt.Sprintf("%d tilemap layer(s) or cel(s) were skipped; tilemap support arrives with the .tmx work",
				skippedTilemaps))
	}
	if frameLimitReached {
		f.Notes = append(f.Notes, fmt.Sprintf("stopped after %d frames", opts.MaxFrames))
	}
	return f, nil
}

// composite flattens one frame's cels bottom-up.
//
// Only the Normal blend mode is implemented; §6 asks for "flattened composite per
// frame", and pixel-art layers are overwhelmingly Normal. Other modes are treated as
// Normal and recorded in Notes.
func composite(f *File, cels []cel, allFrames [][]cel, frameIndex int) (image.Image, error) {
	out := image.NewRGBA(image.Rect(0, 0, f.Width, f.Height))

	for _, c := range cels {
		if c.layerIndex < 0 || c.layerIndex >= len(f.Layers) {
			// A cel referring to a layer that was never declared.
			return nil, fmt.Errorf("%w: cel references layer %d of %d",
				ErrMalformed, c.layerIndex, len(f.Layers))
		}
		layer := f.Layers[c.layerIndex]
		if !layer.Visible || layer.Type == layerTypeGroup {
			continue
		}

		src := c.img
		if c.linked {
			// A linked cel borrows the pixels of the same layer in another frame.
			resolved, err := resolveLinked(c, allFrames, frameIndex)
			if err != nil {
				return nil, err
			}
			if resolved == nil {
				continue
			}
			src = resolved
		}
		if src == nil {
			continue
		}

		// Layer opacity and cel opacity multiply.
		alphaScale := float64(layer.Opacity) / 255 * float64(c.opacity) / 255
		blendNormal(out, src, c.x, c.y, alphaScale)
	}
	return out, nil
}

// resolveLinked finds the pixels a linked cel points at.
func resolveLinked(c cel, allFrames [][]cel, frameIndex int) (*image.RGBA, error) {
	if c.linkedFrame < 0 || c.linkedFrame >= len(allFrames) {
		// Forward references are illegal: a linked cel may only point at an earlier
		// frame, which by construction has already been parsed.
		return nil, fmt.Errorf("%w: frame %d links to frame %d, which is not available",
			ErrMalformed, frameIndex, c.linkedFrame)
	}
	for _, candidate := range allFrames[c.linkedFrame] {
		if candidate.layerIndex == c.layerIndex && candidate.img != nil {
			return candidate.img, nil
		}
	}
	// A link to a frame where that layer has no cel. Legal — the layer is simply
	// empty there — so this is not an error.
	return nil, nil
}

// blendNormal draws src over dst at (offsetX, offsetY) with an extra alpha scale.
//
// Written out rather than using image/draw because the alpha scale has to apply per
// pixel, and because src may hang off the edge of the canvas: Aseprite cels are
// allowed coordinates outside the frame.
func blendNormal(dst *image.RGBA, src *image.RGBA, offsetX, offsetY int, alphaScale float64) {
	if alphaScale <= 0 {
		return
	}
	sb := src.Bounds()

	for sy := sb.Min.Y; sy < sb.Max.Y; sy++ {
		dy := offsetY + (sy - sb.Min.Y)
		if dy < dst.Rect.Min.Y || dy >= dst.Rect.Max.Y {
			continue
		}
		for sx := sb.Min.X; sx < sb.Max.X; sx++ {
			dx := offsetX + (sx - sb.Min.X)
			if dx < dst.Rect.Min.X || dx >= dst.Rect.Max.X {
				continue
			}

			si := src.PixOffset(sx, sy)
			sa := float64(src.Pix[si+3]) * alphaScale
			if sa <= 0 {
				continue
			}

			di := dst.PixOffset(dx, dy)
			// Standard source-over, on non-premultiplied bytes scaled to 0..1.
			srcA := sa / 255
			dstA := float64(dst.Pix[di+3]) / 255
			outA := srcA + dstA*(1-srcA)
			if outA <= 0 {
				dst.Pix[di], dst.Pix[di+1], dst.Pix[di+2], dst.Pix[di+3] = 0, 0, 0, 0
				continue
			}
			for ch := 0; ch < 3; ch++ {
				s := float64(src.Pix[si+ch]) / 255
				d := float64(dst.Pix[di+ch]) / 255
				v := (s*srcA + d*dstA*(1-srcA)) / outA
				dst.Pix[di+ch] = uint8(clamp01(v)*255 + 0.5)
			}
			dst.Pix[di+3] = uint8(clamp01(outA)*255 + 0.5)
		}
	}
}

func clamp01(v float64) float64 {
	switch {
	case v < 0:
		return 0
	case v > 1:
		return 1
	default:
		return v
	}
}

// --- chunk parsers ----------------------------------------------------------

func parseLayer(body []byte) (Layer, error) {
	r := &reader{data: body}

	flags, err := r.uint16()
	if err != nil {
		return Layer{}, fmt.Errorf("%w: truncated layer chunk", ErrMalformed)
	}
	layerType, _ := r.uint16()
	childLevel, _ := r.uint16()
	r.skip(4) // default width and height, ignored
	blendMode, _ := r.uint16()
	opacity, _ := r.uint8()
	r.skip(3) // reserved
	name, err := r.string()
	if err != nil {
		return Layer{}, fmt.Errorf("%w: unreadable layer name", ErrMalformed)
	}

	return Layer{
		Name:       name,
		Visible:    flags&layerFlagVisible != 0,
		Opacity:    opacity,
		BlendMode:  blendMode,
		Type:       layerType,
		ChildLevel: childLevel,
	}, nil
}

func parseCel(body []byte, depth, canvasW, canvasH int, maxPixels int64,
	palette []color.RGBA, transparentIndex uint8) (cel, error) {

	r := &reader{data: body}

	layerIndex, err := r.uint16()
	if err != nil {
		return cel{}, fmt.Errorf("%w: truncated cel chunk", ErrMalformed)
	}
	x, _ := r.int16()
	y, _ := r.int16()
	opacity, _ := r.uint8()
	celType, _ := r.uint16()
	r.skip(2) // z-index (newer files), unused here
	r.skip(5) // reserved
	if err := r.err(); err != nil {
		return cel{}, fmt.Errorf("%w: truncated cel header", ErrMalformed)
	}

	c := cel{
		layerIndex: int(layerIndex),
		x:          int(x),
		y:          int(y),
		opacity:    opacity,
	}

	switch celType {
	case celTypeLinked:
		frame, err := r.uint16()
		if err != nil {
			return cel{}, fmt.Errorf("%w: truncated linked cel", ErrMalformed)
		}
		c.linked = true
		c.linkedFrame = int(frame)
		return c, nil

	case celTypeRaw, celTypeCompressedImage:
		w, err := r.uint16()
		if err != nil {
			return cel{}, fmt.Errorf("%w: truncated cel dimensions", ErrMalformed)
		}
		h, _ := r.uint16()
		if err := r.err(); err != nil {
			return cel{}, fmt.Errorf("%w: truncated cel dimensions", ErrMalformed)
		}
		if w == 0 || h == 0 {
			// An empty cel is legal and simply contributes nothing.
			return c, nil
		}
		// The cel may legitimately be larger than the canvas, but not absurdly so.
		if px := int64(w) * int64(h); px > maxPixels {
			return cel{}, fmt.Errorf("%w: cel is %dx%d, over the pixel limit",
				ErrUnsupportedFeature, w, h)
		}

		bytesPerPixel := depth / 8
		want := int(w) * int(h) * bytesPerPixel

		var pixels []byte
		if celType == celTypeRaw {
			pixels = r.rest()
			if len(pixels) < want {
				return cel{}, fmt.Errorf("%w: raw cel has %d bytes, needs %d",
					ErrMalformed, len(pixels), want)
			}
		} else {
			decompressed, err := inflate(r.rest(), want)
			if err != nil {
				return cel{}, err
			}
			pixels = decompressed
		}

		img, err := pixelsToImage(pixels, int(w), int(h), depth, palette, transparentIndex)
		if err != nil {
			return cel{}, err
		}
		c.img = img
		return c, nil

	case celTypeCompressedTilemap:
		return cel{}, fmt.Errorf("%w: tilemap cel", ErrUnsupportedFeature)
	}
	return cel{}, fmt.Errorf("%w: unknown cel type %d", ErrMalformed, celType)
}

// inflate decompresses a zlib stream, refusing to expand beyond what the cel's
// declared dimensions require.
//
// The bound is the defence: a crafted file could otherwise claim a 1x1 cel and ship a
// zlib bomb. io.LimitReader caps it at exactly the expected size plus a byte, so an
// over-long stream is detected rather than absorbed.
func inflate(compressed []byte, want int) ([]byte, error) {
	if want <= 0 {
		return nil, fmt.Errorf("%w: cel needs no pixels", ErrMalformed)
	}
	zr, err := zlib.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, fmt.Errorf("%w: cel is not a valid zlib stream: %v", ErrMalformed, err)
	}
	defer zr.Close()

	out := make([]byte, 0, want)
	buf := bytes.NewBuffer(out)
	if _, err := io.Copy(buf, io.LimitReader(zr, int64(want)+1)); err != nil {
		return nil, fmt.Errorf("%w: cel decompression failed: %v", ErrMalformed, err)
	}
	got := buf.Bytes()
	if len(got) < want {
		return nil, fmt.Errorf("%w: cel decompressed to %d bytes, needs %d",
			ErrMalformed, len(got), want)
	}
	return got[:want], nil
}

// pixelsToImage converts raw cel pixels into RGBA, per colour depth.
func pixelsToImage(pixels []byte, w, h, depth int, palette []color.RGBA,
	transparentIndex uint8) (*image.RGBA, error) {

	img := image.NewRGBA(image.Rect(0, 0, w, h))

	switch depth {
	case 32:
		// Straight (non-premultiplied) RGBA, which is what image.RGBA also holds.
		if len(pixels) < w*h*4 {
			return nil, fmt.Errorf("%w: RGBA cel is short", ErrMalformed)
		}
		copy(img.Pix, pixels[:w*h*4])

	case 16:
		// Grayscale plus alpha.
		if len(pixels) < w*h*2 {
			return nil, fmt.Errorf("%w: grayscale cel is short", ErrMalformed)
		}
		for i := 0; i < w*h; i++ {
			v, a := pixels[i*2], pixels[i*2+1]
			o := i * 4
			img.Pix[o], img.Pix[o+1], img.Pix[o+2], img.Pix[o+3] = v, v, v, a
		}

	case 8:
		// Indexed. The transparent index is fully transparent regardless of what the
		// palette says for it — that is the whole point of the field.
		if len(pixels) < w*h {
			return nil, fmt.Errorf("%w: indexed cel is short", ErrMalformed)
		}
		for i := 0; i < w*h; i++ {
			idx := pixels[i]
			o := i * 4
			if idx == transparentIndex {
				img.Pix[o], img.Pix[o+1], img.Pix[o+2], img.Pix[o+3] = 0, 0, 0, 0
				continue
			}
			c := palette[idx]
			img.Pix[o], img.Pix[o+1], img.Pix[o+2], img.Pix[o+3] = c.R, c.G, c.B, c.A
		}

	default:
		return nil, fmt.Errorf("%w: colour depth %d", ErrUnsupportedFeature, depth)
	}
	return img, nil
}

func parsePalette(body []byte, palette []color.RGBA) error {
	r := &reader{data: body}

	size, err := r.uint32()
	if err != nil {
		return fmt.Errorf("%w: truncated palette chunk", ErrMalformed)
	}
	_ = size
	first, _ := r.uint32()
	last, _ := r.uint32()
	r.skip(8) // reserved
	if err := r.err(); err != nil {
		return fmt.Errorf("%w: truncated palette header", ErrMalformed)
	}
	if last < first {
		return fmt.Errorf("%w: palette range %d..%d", ErrMalformed, first, last)
	}

	for i := first; i <= last; i++ {
		flags, err := r.uint16()
		if err != nil {
			return fmt.Errorf("%w: palette entry %d is truncated", ErrMalformed, i)
		}
		rr, _ := r.uint8()
		gg, _ := r.uint8()
		bb, _ := r.uint8()
		aa, err := r.uint8()
		if err != nil {
			return fmt.Errorf("%w: palette entry %d is truncated", ErrMalformed, i)
		}
		if flags&1 != 0 {
			// Entry carries a name, which is of no use here but must be skipped.
			if _, err := r.string(); err != nil {
				return fmt.Errorf("%w: palette entry %d name is truncated", ErrMalformed, i)
			}
		}
		if i < uint32(len(palette)) {
			palette[i] = color.RGBA{R: rr, G: gg, B: bb, A: aa}
		}
	}
	return nil
}

// parseOldPalette reads the pre-1.x palette chunks, which store 6-bit channels.
func parseOldPalette(body []byte, palette []color.RGBA) error {
	r := &reader{data: body}

	packets, err := r.uint16()
	if err != nil {
		return fmt.Errorf("%w: truncated old palette chunk", ErrMalformed)
	}

	index := 0
	for p := 0; p < int(packets); p++ {
		skip, err := r.uint8()
		if err != nil {
			return fmt.Errorf("%w: truncated old palette packet", ErrMalformed)
		}
		count, err := r.uint8()
		if err != nil {
			return fmt.Errorf("%w: truncated old palette packet", ErrMalformed)
		}
		index += int(skip)

		n := int(count)
		if n == 0 {
			n = 256 // 0 means 256 in this format
		}
		for i := 0; i < n; i++ {
			rr, _ := r.uint8()
			gg, _ := r.uint8()
			bb, err := r.uint8()
			if err != nil {
				return fmt.Errorf("%w: truncated old palette colour", ErrMalformed)
			}
			if index >= 0 && index < len(palette) {
				// Channels are 0..63 in this format, scaled to 0..255.
				palette[index] = color.RGBA{R: rr * 4, G: gg * 4, B: bb * 4, A: 0xff}
			}
			index++
		}
	}
	return nil
}

func parseTags(body []byte) ([]Tag, error) {
	r := &reader{data: body}

	count, err := r.uint16()
	if err != nil {
		return nil, fmt.Errorf("%w: truncated tags chunk", ErrMalformed)
	}
	r.skip(8) // reserved

	out := make([]Tag, 0, capacityHint(int(count)))
	for i := 0; i < int(count); i++ {
		from, err := r.uint16()
		if err != nil {
			return nil, fmt.Errorf("%w: tag %d is truncated", ErrMalformed, i)
		}
		to, _ := r.uint16()
		r.skip(1) // loop direction
		r.skip(2) // repeat count (newer files)
		r.skip(6) // reserved
		r.skip(3) // deprecated RGB colour
		r.skip(1) // extra byte
		name, err := r.string()
		if err != nil {
			return nil, fmt.Errorf("%w: tag %d name is truncated", ErrMalformed, i)
		}
		out = append(out, Tag{Name: name, From: int(from), To: int(to)})
	}
	return out, nil
}

// capacityHint bounds a slice pre-allocation taken from file data.
//
// Every count in an Aseprite file is attacker-controlled. Pre-sizing from one is a
// denial of service: append grows as needed, so the hint is only ever an
// optimisation, and capping it costs nothing.
func capacityHint(n int) int {
	const maxHint = 1024
	switch {
	case n < 0:
		return 0
	case n > maxHint:
		return maxHint
	default:
		return n
	}
}

// --- a bounds-checked little-endian reader ----------------------------------

// reader reads little-endian values, recording the first failure rather than
// panicking. Aseprite files come from the internet; every read has to be checked.
type reader struct {
	data    []byte
	pos     int
	failure error
}

func (r *reader) err() error { return r.failure }

func (r *reader) remaining() int {
	if r.pos >= len(r.data) {
		return 0
	}
	return len(r.data) - r.pos
}

func (r *reader) fail(need int) error {
	if r.failure == nil {
		r.failure = fmt.Errorf("%w: needed %d more bytes at offset %d of %d",
			ErrMalformed, need, r.pos, len(r.data))
	}
	return r.failure
}

func (r *reader) skip(n int) {
	r.pos += n
	if r.pos > len(r.data) {
		r.pos = len(r.data)
	}
}

func (r *reader) seek(pos int) {
	switch {
	case pos < 0:
		r.pos = 0
	case pos > len(r.data):
		r.pos = len(r.data)
	default:
		r.pos = pos
	}
}

func (r *reader) rest() []byte {
	if r.pos >= len(r.data) {
		return nil
	}
	out := r.data[r.pos:]
	r.pos = len(r.data)
	return out
}

func (r *reader) uint8() (uint8, error) {
	if r.remaining() < 1 {
		return 0, r.fail(1)
	}
	v := r.data[r.pos]
	r.pos++
	return v, nil
}

func (r *reader) uint16() (uint16, error) {
	if r.remaining() < 2 {
		return 0, r.fail(2)
	}
	v := binary.LittleEndian.Uint16(r.data[r.pos:])
	r.pos += 2
	return v, nil
}

func (r *reader) int16() (int16, error) {
	v, err := r.uint16()
	return int16(v), err
}

func (r *reader) uint32() (uint32, error) {
	if r.remaining() < 4 {
		return 0, r.fail(4)
	}
	v := binary.LittleEndian.Uint32(r.data[r.pos:])
	r.pos += 4
	return v, nil
}

// string reads Aseprite's length-prefixed UTF-8 string.
func (r *reader) string() (string, error) {
	n, err := r.uint16()
	if err != nil {
		return "", err
	}
	if r.remaining() < int(n) {
		return "", r.fail(int(n))
	}
	s := string(r.data[r.pos : r.pos+int(n)])
	r.pos += int(n)
	return s, nil
}
