package aseprite

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"image/color"
	"testing"
	"time"
)

// --- a builder for real files of the format ---------------------------------
//
// No Aseprite-authored file is available here, so the fixtures are constructed to the
// documented layout. That tests the decoder against the spec rather than against real
// output — an important limitation, and the reason the decoder degrades to
// ErrUnsupportedFeature rather than guessing, and why §6 keeps AMBAR_ASEPRITE_BIN as
// an escape hatch. Dropping one real .aseprite into testdata/fixtures/ is the single
// most valuable addition to this file.

type builder struct {
	width, height    int
	depth            int
	transparentIndex uint8
	frames           []frameBuilder
}

type frameBuilder struct {
	durationMS int
	chunks     []([]byte)
}

func newBuilder(w, h, depth int) *builder {
	return &builder{width: w, height: h, depth: depth}
}

func (b *builder) frame(durationMS int, chunks ...[]byte) *builder {
	b.frames = append(b.frames, frameBuilder{durationMS: durationMS, chunks: chunks})
	return b
}

// bytes assembles the whole file.
func (b *builder) bytes() []byte {
	var out bytes.Buffer

	// The 128-byte header.
	var header bytes.Buffer
	putU32(&header, 0) // file size, patched below
	putU16(&header, headerMagic)
	putU16(&header, uint16(len(b.frames)))
	putU16(&header, uint16(b.width))
	putU16(&header, uint16(b.height))
	putU16(&header, uint16(b.depth))
	putU32(&header, 0)                   // flags
	putU16(&header, 100)                 // deprecated speed
	putU32(&header, 0)                   // reserved
	putU32(&header, 0)                   // reserved
	header.WriteByte(b.transparentIndex) // transparent palette index
	header.Write([]byte{0, 0, 0})        // ignored
	putU16(&header, 0)                   // number of colours
	header.WriteByte(1)                  // pixel width
	header.WriteByte(1)                  // pixel height
	putU16(&header, 0)                   // grid x
	putU16(&header, 0)                   // grid y
	putU16(&header, 16)                  // grid width
	putU16(&header, 16)                  // grid height
	for header.Len() < headerSize {
		header.WriteByte(0)
	}
	out.Write(header.Bytes())

	for _, f := range b.frames {
		var frame bytes.Buffer
		putU32(&frame, 0) // frame size, patched below
		putU16(&frame, frameMagic)
		putU16(&frame, 0xFFFF) // old chunk count: defer to the 32-bit field
		putU16(&frame, uint16(f.durationMS))
		frame.Write([]byte{0, 0}) // reserved
		putU32(&frame, uint32(len(f.chunks)))
		for _, c := range f.chunks {
			frame.Write(c)
		}

		raw := frame.Bytes()
		binary.LittleEndian.PutUint32(raw[0:4], uint32(len(raw)))
		out.Write(raw)
	}

	result := out.Bytes()
	binary.LittleEndian.PutUint32(result[0:4], uint32(len(result)))
	return result
}

// chunk wraps a body in the 6-byte chunk header.
func chunk(chunkType uint16, body []byte) []byte {
	var out bytes.Buffer
	putU32(&out, uint32(len(body)+6))
	putU16(&out, chunkType)
	out.Write(body)
	return out.Bytes()
}

func layerChunk(name string, visible bool, opacity uint8, blendMode uint16, layerType uint16) []byte {
	var body bytes.Buffer
	var flags uint16
	if visible {
		flags |= layerFlagVisible
	}
	putU16(&body, flags)
	putU16(&body, layerType)
	putU16(&body, 0) // child level
	putU16(&body, 0) // default width
	putU16(&body, 0) // default height
	putU16(&body, blendMode)
	body.WriteByte(opacity)
	body.Write([]byte{0, 0, 0}) // reserved
	putString(&body, name)
	return chunk(chunkLayer, body.Bytes())
}

// celChunk writes a zlib-compressed image cel (type 2), the form Aseprite actually
// uses.
func celChunk(layerIndex, x, y int, opacity uint8, w, h int, pixels []byte) []byte {
	var body bytes.Buffer
	putU16(&body, uint16(layerIndex))
	putI16(&body, int16(x))
	putI16(&body, int16(y))
	body.WriteByte(opacity)
	putU16(&body, celTypeCompressedImage)
	putU16(&body, 0)            // z-index
	body.Write(make([]byte, 5)) // reserved
	putU16(&body, uint16(w))
	putU16(&body, uint16(h))

	var compressed bytes.Buffer
	zw := zlib.NewWriter(&compressed)
	zw.Write(pixels) //nolint:errcheck
	zw.Close()       //nolint:errcheck
	body.Write(compressed.Bytes())

	return chunk(chunkCel, body.Bytes())
}

// rawCelChunk writes an uncompressed cel (type 0).
func rawCelChunk(layerIndex, x, y int, opacity uint8, w, h int, pixels []byte) []byte {
	var body bytes.Buffer
	putU16(&body, uint16(layerIndex))
	putI16(&body, int16(x))
	putI16(&body, int16(y))
	body.WriteByte(opacity)
	putU16(&body, celTypeRaw)
	putU16(&body, 0)
	body.Write(make([]byte, 5))
	putU16(&body, uint16(w))
	putU16(&body, uint16(h))
	body.Write(pixels)
	return chunk(chunkCel, body.Bytes())
}

func linkedCelChunk(layerIndex int, opacity uint8, linkedFrame int) []byte {
	var body bytes.Buffer
	putU16(&body, uint16(layerIndex))
	putI16(&body, 0)
	putI16(&body, 0)
	body.WriteByte(opacity)
	putU16(&body, celTypeLinked)
	putU16(&body, 0)
	body.Write(make([]byte, 5))
	putU16(&body, uint16(linkedFrame))
	return chunk(chunkCel, body.Bytes())
}

func tilemapCelChunk(layerIndex int) []byte {
	var body bytes.Buffer
	putU16(&body, uint16(layerIndex))
	putI16(&body, 0)
	putI16(&body, 0)
	body.WriteByte(255)
	putU16(&body, celTypeCompressedTilemap)
	putU16(&body, 0)
	body.Write(make([]byte, 5))
	return chunk(chunkCel, body.Bytes())
}

func paletteChunk(entries map[int]color.RGBA) []byte {
	first, last := 256, 0
	for i := range entries {
		if i < first {
			first = i
		}
		if i > last {
			last = i
		}
	}

	var body bytes.Buffer
	putU32(&body, uint32(last-first+1)) // new size
	putU32(&body, uint32(first))
	putU32(&body, uint32(last))
	body.Write(make([]byte, 8)) // reserved
	for i := first; i <= last; i++ {
		c := entries[i]
		putU16(&body, 0) // flags: no name
		body.WriteByte(c.R)
		body.WriteByte(c.G)
		body.WriteByte(c.B)
		body.WriteByte(c.A)
	}
	return chunk(chunkPalette, body.Bytes())
}

func tagsChunk(tags ...Tag) []byte {
	var body bytes.Buffer
	putU16(&body, uint16(len(tags)))
	body.Write(make([]byte, 8)) // reserved
	for _, t := range tags {
		putU16(&body, uint16(t.From))
		putU16(&body, uint16(t.To))
		body.WriteByte(0)           // loop direction
		putU16(&body, 0)            // repeat
		body.Write(make([]byte, 6)) // reserved
		body.Write([]byte{0, 0, 0}) // deprecated colour
		body.WriteByte(0)           // extra
		putString(&body, t.Name)
	}
	return chunk(chunkTags, body.Bytes())
}

// rgbaPixels builds a solid block of straight RGBA.
func rgbaPixels(w, h int, c color.RGBA) []byte {
	out := make([]byte, w*h*4)
	for i := 0; i < w*h; i++ {
		out[i*4], out[i*4+1], out[i*4+2], out[i*4+3] = c.R, c.G, c.B, c.A
	}
	return out
}

func indexedPixels(w, h int, index uint8) []byte {
	out := make([]byte, w*h)
	for i := range out {
		out[i] = index
	}
	return out
}

func grayPixels(w, h int, value, alpha uint8) []byte {
	out := make([]byte, w*h*2)
	for i := 0; i < w*h; i++ {
		out[i*2], out[i*2+1] = value, alpha
	}
	return out
}

func putU16(b *bytes.Buffer, v uint16) {
	var tmp [2]byte
	binary.LittleEndian.PutUint16(tmp[:], v)
	b.Write(tmp[:])
}

func putI16(b *bytes.Buffer, v int16) { putU16(b, uint16(v)) }

func putU32(b *bytes.Buffer, v uint32) {
	var tmp [4]byte
	binary.LittleEndian.PutUint32(tmp[:], v)
	b.Write(tmp[:])
}

func putString(b *bytes.Buffer, s string) {
	putU16(b, uint16(len(s)))
	b.WriteString(s)
}

// --- tests ------------------------------------------------------------------

func TestDecodeRGBASingleFrame(t *testing.T) {
	red := color.RGBA{R: 0xff, G: 0x00, B: 0x00, A: 0xff}

	data := newBuilder(4, 4, 32).
		frame(100,
			layerChunk("Layer 1", true, 255, blendModeNormal, layerTypeNormal),
			celChunk(0, 0, 0, 255, 4, 4, rgbaPixels(4, 4, red)),
		).bytes()

	f, err := Decode(data, Options{})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if f.Width != 4 || f.Height != 4 {
		t.Errorf("size = %dx%d, want 4x4", f.Width, f.Height)
	}
	if f.ColorDepth != 32 {
		t.Errorf("depth = %d, want 32", f.ColorDepth)
	}
	if len(f.Frames) != 1 {
		t.Fatalf("%d frames, want 1", len(f.Frames))
	}
	if got := f.Frames[0].Duration; got != 100*time.Millisecond {
		t.Errorf("duration = %s, want 100ms", got)
	}
	if len(f.Layers) != 1 || f.Layers[0].Name != "Layer 1" {
		t.Errorf("layers = %+v", f.Layers)
	}

	assertPixel(t, f, 0, 0, 0, red)
	assertPixel(t, f, 0, 3, 3, red)
}

func TestDecodeIndexed(t *testing.T) {
	// Indexed is what pixel artists mostly work in, so this is the important path.
	green := color.RGBA{R: 0x00, G: 0xcc, B: 0x44, A: 0xff}

	b := newBuilder(2, 2, 8)
	b.transparentIndex = 0
	data := b.frame(100,
		layerChunk("Layer 1", true, 255, blendModeNormal, layerTypeNormal),
		paletteChunk(map[int]color.RGBA{0: {}, 1: green}),
		celChunk(0, 0, 0, 255, 2, 2, indexedPixels(2, 2, 1)),
	).bytes()

	f, err := Decode(data, Options{})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	assertPixel(t, f, 0, 0, 0, green)
	assertPixel(t, f, 0, 1, 1, green)
}

// TestIndexedTransparentIndexIsTransparent: the transparent index must win over
// whatever the palette says for it — that is the entire purpose of the field.
func TestIndexedTransparentIndexIsTransparent(t *testing.T) {
	opaqueBlack := color.RGBA{A: 0xff}

	b := newBuilder(2, 1, 8)
	b.transparentIndex = 0
	data := b.frame(100,
		layerChunk("L", true, 255, blendModeNormal, layerTypeNormal),
		// Index 0 is deliberately given an opaque colour in the palette.
		paletteChunk(map[int]color.RGBA{0: opaqueBlack, 1: {R: 0xff, A: 0xff}}),
		celChunk(0, 0, 0, 255, 2, 1, []byte{0, 1}),
	).bytes()

	f, err := Decode(data, Options{})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, _, _, a := f.Frames[0].Image.At(0, 0).RGBA(); a != 0 {
		t.Errorf("the transparent index rendered with alpha %d, want 0", a)
	}
	if _, _, _, a := f.Frames[0].Image.At(1, 0).RGBA(); a == 0 {
		t.Error("a normal index rendered transparent")
	}
}

func TestDecodeGrayscale(t *testing.T) {
	data := newBuilder(2, 2, 16).
		frame(100,
			layerChunk("L", true, 255, blendModeNormal, layerTypeNormal),
			celChunk(0, 0, 0, 255, 2, 2, grayPixels(2, 2, 0x80, 0xff)),
		).bytes()

	f, err := Decode(data, Options{})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	r, g, b, a := f.Frames[0].Image.At(0, 0).RGBA()
	if r != g || g != b {
		t.Errorf("grayscale produced unequal channels: %d %d %d", r>>8, g>>8, b>>8)
	}
	if a == 0 {
		t.Error("grayscale pixel is transparent")
	}
}

func TestDecodeRawCel(t *testing.T) {
	blue := color.RGBA{B: 0xff, A: 0xff}
	data := newBuilder(2, 2, 32).
		frame(100,
			layerChunk("L", true, 255, blendModeNormal, layerTypeNormal),
			rawCelChunk(0, 0, 0, 255, 2, 2, rgbaPixels(2, 2, blue)),
		).bytes()

	f, err := Decode(data, Options{})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	assertPixel(t, f, 0, 0, 0, blue)
}

// TestMultipleFramesAndDurations is what makes an animated preview possible without an
// exported PNG sequence (§6).
func TestMultipleFramesAndDurations(t *testing.T) {
	red := color.RGBA{R: 0xff, A: 0xff}
	blue := color.RGBA{B: 0xff, A: 0xff}

	data := newBuilder(2, 2, 32).
		frame(50,
			layerChunk("L", true, 255, blendModeNormal, layerTypeNormal),
			celChunk(0, 0, 0, 255, 2, 2, rgbaPixels(2, 2, red)),
		).
		frame(150,
			celChunk(0, 0, 0, 255, 2, 2, rgbaPixels(2, 2, blue)),
		).bytes()

	f, err := Decode(data, Options{})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(f.Frames) != 2 {
		t.Fatalf("%d frames, want 2", len(f.Frames))
	}
	assertPixel(t, f, 0, 0, 0, red)
	assertPixel(t, f, 1, 0, 0, blue)

	if f.Frames[0].Duration != 50*time.Millisecond {
		t.Errorf("frame 0 duration = %s", f.Frames[0].Duration)
	}
	if f.Frames[1].Duration != 150*time.Millisecond {
		t.Errorf("frame 1 duration = %s", f.Frames[1].Duration)
	}
}

// TestLinkedCel: frame 2 reuses frame 1's pixels, which is how Aseprite stores a held
// frame. Getting this wrong produces an empty frame in the middle of an animation.
func TestLinkedCel(t *testing.T) {
	red := color.RGBA{R: 0xff, A: 0xff}

	data := newBuilder(2, 2, 32).
		frame(100,
			layerChunk("L", true, 255, blendModeNormal, layerTypeNormal),
			celChunk(0, 0, 0, 255, 2, 2, rgbaPixels(2, 2, red)),
		).
		frame(100,
			linkedCelChunk(0, 255, 0),
		).bytes()

	f, err := Decode(data, Options{})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(f.Frames) != 2 {
		t.Fatalf("%d frames, want 2", len(f.Frames))
	}
	assertPixel(t, f, 1, 0, 0, red)
}

func TestLinkedCelToMissingFrameIsAnError(t *testing.T) {
	data := newBuilder(2, 2, 32).
		frame(100,
			layerChunk("L", true, 255, blendModeNormal, layerTypeNormal),
			// Links forward to frame 5, which does not exist.
			linkedCelChunk(0, 255, 5),
		).bytes()

	if _, err := Decode(data, Options{}); !errors.Is(err, ErrMalformed) {
		t.Errorf("err = %v, want ErrMalformed", err)
	}
}

func TestHiddenLayerIsNotComposited(t *testing.T) {
	red := color.RGBA{R: 0xff, A: 0xff}

	data := newBuilder(2, 2, 32).
		frame(100,
			layerChunk("hidden", false, 255, blendModeNormal, layerTypeNormal),
			celChunk(0, 0, 0, 255, 2, 2, rgbaPixels(2, 2, red)),
		).bytes()

	f, err := Decode(data, Options{})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, _, _, a := f.Frames[0].Image.At(0, 0).RGBA(); a != 0 {
		t.Errorf("a hidden layer was composited (alpha %d)", a)
	}
	// The layer is still reported, because the UI lists layers.
	if len(f.Layers) != 1 || f.Layers[0].Visible {
		t.Errorf("layers = %+v", f.Layers)
	}
}

func TestLayerOpacityIsApplied(t *testing.T) {
	white := color.RGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff}

	data := newBuilder(1, 1, 32).
		frame(100,
			layerChunk("half", true, 128, blendModeNormal, layerTypeNormal),
			celChunk(0, 0, 0, 255, 1, 1, rgbaPixels(1, 1, white)),
		).bytes()

	f, err := Decode(data, Options{})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	_, _, _, a := f.Frames[0].Image.At(0, 0).RGBA()
	alpha8 := a >> 8
	if alpha8 < 120 || alpha8 > 136 {
		t.Errorf("alpha = %d, want roughly 128 from 50%% layer opacity", alpha8)
	}
}

func TestCelOpacityIsApplied(t *testing.T) {
	white := color.RGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff}

	data := newBuilder(1, 1, 32).
		frame(100,
			layerChunk("L", true, 255, blendModeNormal, layerTypeNormal),
			celChunk(0, 0, 0, 64, 1, 1, rgbaPixels(1, 1, white)),
		).bytes()

	f, err := Decode(data, Options{})
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, a := f.Frames[0].Image.At(0, 0).RGBA()
	if alpha8 := a >> 8; alpha8 < 56 || alpha8 > 72 {
		t.Errorf("alpha = %d, want roughly 64 from cel opacity", alpha8)
	}
}

// TestTagsBecomeAnimationNames is §6's "Frame tags map directly onto animation_names".
func TestTagsBecomeAnimationNames(t *testing.T) {
	data := newBuilder(2, 2, 32).
		frame(100,
			layerChunk("L", true, 255, blendModeNormal, layerTypeNormal),
			celChunk(0, 0, 0, 255, 2, 2, rgbaPixels(2, 2, color.RGBA{A: 0xff})),
			tagsChunk(
				Tag{Name: "idle", From: 0, To: 3},
				Tag{Name: "walk", From: 4, To: 11},
				Tag{Name: "attack", From: 12, To: 15},
			),
		).bytes()

	f, err := Decode(data, Options{})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	names := f.TagNames()
	if len(names) != 3 || names[0] != "idle" || names[1] != "walk" || names[2] != "attack" {
		t.Errorf("TagNames() = %v, want [idle walk attack]", names)
	}
	if f.Tags[1].From != 4 || f.Tags[1].To != 11 {
		t.Errorf("tag range = %d..%d, want 4..11", f.Tags[1].From, f.Tags[1].To)
	}
}

// TestCelOffsetAndClipping: Aseprite cels carry their own position and may extend past
// the canvas, so compositing has to clip rather than index out of bounds.
func TestCelOffsetAndClipping(t *testing.T) {
	red := color.RGBA{R: 0xff, A: 0xff}

	data := newBuilder(4, 4, 32).
		frame(100,
			layerChunk("L", true, 255, blendModeNormal, layerTypeNormal),
			// A 2x2 cel placed so half of it hangs off the right/bottom edge.
			celChunk(0, 3, 3, 255, 2, 2, rgbaPixels(2, 2, red)),
		).bytes()

	f, err := Decode(data, Options{})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	assertPixel(t, f, 0, 3, 3, red)
	// And nothing was written where the cel does not reach.
	if _, _, _, a := f.Frames[0].Image.At(0, 0).RGBA(); a != 0 {
		t.Error("a pixel outside the cel was painted")
	}
}

func TestNegativeCelOffsetIsClipped(t *testing.T) {
	red := color.RGBA{R: 0xff, A: 0xff}

	data := newBuilder(4, 4, 32).
		frame(100,
			layerChunk("L", true, 255, blendModeNormal, layerTypeNormal),
			celChunk(0, -1, -1, 255, 2, 2, rgbaPixels(2, 2, red)),
		).bytes()

	f, err := Decode(data, Options{})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Only the bottom-right quarter of the cel lands on the canvas.
	assertPixel(t, f, 0, 0, 0, red)
	if _, _, _, a := f.Frames[0].Image.At(1, 1).RGBA(); a != 0 {
		t.Error("the cel painted further than it should")
	}
}

// TestNonNormalBlendModeIsNoted: an approximation must never be silent.
func TestNonNormalBlendModeIsNoted(t *testing.T) {
	data := newBuilder(2, 2, 32).
		frame(100,
			layerChunk("multiply", true, 255, 3 /* some non-normal mode */, layerTypeNormal),
			celChunk(0, 0, 0, 255, 2, 2, rgbaPixels(2, 2, color.RGBA{R: 0xff, A: 0xff})),
		).bytes()

	f, err := Decode(data, Options{})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(f.Notes) == 0 {
		t.Fatal("an unimplemented blend mode produced no note")
	}
	found := false
	for _, n := range f.Notes {
		if bytes.Contains([]byte(n), []byte("blend mode")) {
			found = true
		}
	}
	if !found {
		t.Errorf("notes = %v, want one mentioning the blend mode", f.Notes)
	}
}

// TestTilemapCelIsSkippedAndNoted: tilemap support belongs with §5.1's .tmx work, and
// until then the file still has to produce a usable composite of its other layers.
func TestTilemapCelIsSkippedAndNoted(t *testing.T) {
	red := color.RGBA{R: 0xff, A: 0xff}

	data := newBuilder(2, 2, 32).
		frame(100,
			layerChunk("tiles", true, 255, blendModeNormal, layerTypeTilemap),
			layerChunk("art", true, 255, blendModeNormal, layerTypeNormal),
			tilemapCelChunk(0),
			celChunk(1, 0, 0, 255, 2, 2, rgbaPixels(2, 2, red)),
		).bytes()

	f, err := Decode(data, Options{})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	// The normal layer still rendered.
	assertPixel(t, f, 0, 0, 0, red)
	if len(f.Notes) == 0 {
		t.Error("the skipped tilemap was not reported")
	}
}

func TestOldPaletteChunk(t *testing.T) {
	// Pre-1.x files store 6-bit channels in chunk 0x0011.
	var body bytes.Buffer
	putU16(&body, 1)             // one packet
	body.WriteByte(0)            // skip 0 entries
	body.WriteByte(2)            // two colours
	body.Write([]byte{63, 0, 0}) // index 0: full red at 6-bit
	body.Write([]byte{0, 63, 0}) // index 1: full green

	b := newBuilder(2, 1, 8)
	b.transparentIndex = 255 // so index 0 is not treated as transparent
	data := b.frame(100,
		layerChunk("L", true, 255, blendModeNormal, layerTypeNormal),
		chunk(chunkOldPalette11, body.Bytes()),
		celChunk(0, 0, 0, 255, 2, 1, []byte{0, 1}),
	).bytes()

	f, err := Decode(data, Options{})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	r, _, _, _ := f.Frames[0].Image.At(0, 0).RGBA()
	if r>>8 < 240 {
		t.Errorf("6-bit red scaled to %d, want ~252", r>>8)
	}
	_, g, _, _ := f.Frames[0].Image.At(1, 0).RGBA()
	if g>>8 < 240 {
		t.Errorf("6-bit green scaled to %d, want ~252", g>>8)
	}
}

func TestMaxFrames(t *testing.T) {
	b := newBuilder(1, 1, 32)
	b.frame(100,
		layerChunk("L", true, 255, blendModeNormal, layerTypeNormal),
		celChunk(0, 0, 0, 255, 1, 1, rgbaPixels(1, 1, color.RGBA{A: 0xff})),
	)
	for i := 0; i < 9; i++ {
		b.frame(100, celChunk(0, 0, 0, 255, 1, 1, rgbaPixels(1, 1, color.RGBA{A: 0xff})))
	}

	f, err := Decode(b.bytes(), Options{MaxFrames: 3})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(f.Frames) != 3 {
		t.Errorf("%d frames, want 3", len(f.Frames))
	}
	if len(f.Notes) == 0 {
		t.Error("hitting the frame limit was not reported")
	}
}

// TestBrokenFiles is §16's "deliberately broken ones". None may panic, all must return
// a classified error, and none may allocate wildly.
func TestBrokenFiles(t *testing.T) {
	valid := newBuilder(4, 4, 32).
		frame(100,
			layerChunk("L", true, 255, blendModeNormal, layerTypeNormal),
			celChunk(0, 0, 0, 255, 4, 4, rgbaPixels(4, 4, color.RGBA{A: 0xff})),
		).bytes()

	tests := []struct {
		name string
		data []byte
		want error
	}{
		{"empty", nil, ErrNotAseprite},
		{"too short for a header", make([]byte, 10), ErrNotAseprite},
		{"header-length zeroes", make([]byte, headerSize), ErrNotAseprite},
		{"wrong magic", func() []byte {
			d := append([]byte(nil), valid...)
			binary.LittleEndian.PutUint16(d[4:6], 0x1234)
			return d
		}(), ErrNotAseprite},
		{"zero width", func() []byte {
			d := append([]byte(nil), valid...)
			binary.LittleEndian.PutUint16(d[8:10], 0)
			return d
		}(), ErrMalformed},
		{"absurd canvas", func() []byte {
			d := append([]byte(nil), valid...)
			binary.LittleEndian.PutUint16(d[8:10], 65535)
			binary.LittleEndian.PutUint16(d[10:12], 65535)
			return d
		}(), ErrUnsupportedFeature},
		{"unknown colour depth", func() []byte {
			d := append([]byte(nil), valid...)
			binary.LittleEndian.PutUint16(d[12:14], 24)
			return d
		}(), ErrUnsupportedFeature},
		{"truncated after the header", valid[:headerSize+4], ErrMalformed},
		{"truncated mid-frame", valid[:len(valid)-10], nil},
		{"bad frame magic", func() []byte {
			d := append([]byte(nil), valid...)
			binary.LittleEndian.PutUint16(d[headerSize+4:headerSize+6], 0xDEAD)
			return d
		}(), ErrMalformed},
		{"header claims more frames than exist", func() []byte {
			d := append([]byte(nil), valid...)
			binary.LittleEndian.PutUint16(d[6:8], 50)
			return d
		}(), nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// The contract: never a panic.
			f, err := Decode(tc.data, Options{})

			if tc.want == nil {
				// Tolerated: a partial decode is better than nothing, but it must
				// either succeed with frames or fail cleanly.
				if err == nil && (f == nil || len(f.Frames) == 0) {
					t.Error("decode succeeded with no frames")
				}
				return
			}
			if err == nil {
				t.Fatalf("decode succeeded, want %v", tc.want)
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestCorruptZlibCel is the case where the structure is right but the payload is not.
func TestCorruptZlibCel(t *testing.T) {
	var body bytes.Buffer
	putU16(&body, 0) // layer index
	putI16(&body, 0)
	putI16(&body, 0)
	body.WriteByte(255)
	putU16(&body, celTypeCompressedImage)
	putU16(&body, 0)
	body.Write(make([]byte, 5))
	putU16(&body, 4) // width
	putU16(&body, 4) // height
	body.Write([]byte("this is definitely not a zlib stream"))

	data := newBuilder(4, 4, 32).
		frame(100,
			layerChunk("L", true, 255, blendModeNormal, layerTypeNormal),
			chunk(chunkCel, body.Bytes()),
		).bytes()

	_, err := Decode(data, Options{})
	if !errors.Is(err, ErrMalformed) {
		t.Errorf("err = %v, want ErrMalformed", err)
	}
}

// TestZlibCelShorterThanDeclared: a cel claiming 4x4 but decompressing to less must be
// rejected rather than read past the buffer.
func TestZlibCelShorterThanDeclared(t *testing.T) {
	var compressed bytes.Buffer
	zw := zlib.NewWriter(&compressed)
	zw.Write([]byte{1, 2, 3}) //nolint:errcheck // far fewer than 4*4*4 bytes
	zw.Close()                //nolint:errcheck

	var body bytes.Buffer
	putU16(&body, 0)
	putI16(&body, 0)
	putI16(&body, 0)
	body.WriteByte(255)
	putU16(&body, celTypeCompressedImage)
	putU16(&body, 0)
	body.Write(make([]byte, 5))
	putU16(&body, 4)
	putU16(&body, 4)
	body.Write(compressed.Bytes())

	data := newBuilder(4, 4, 32).
		frame(100,
			layerChunk("L", true, 255, blendModeNormal, layerTypeNormal),
			chunk(chunkCel, body.Bytes()),
		).bytes()

	if _, err := Decode(data, Options{}); !errors.Is(err, ErrMalformed) {
		t.Errorf("err = %v, want ErrMalformed", err)
	}
}

// TestCelReferencingAMissingLayer must not index out of bounds.
func TestCelReferencingAMissingLayer(t *testing.T) {
	data := newBuilder(2, 2, 32).
		frame(100,
			// No layer chunk at all, but a cel claiming layer 7.
			celChunk(7, 0, 0, 255, 2, 2, rgbaPixels(2, 2, color.RGBA{A: 0xff})),
		).bytes()

	if _, err := Decode(data, Options{}); !errors.Is(err, ErrMalformed) {
		t.Errorf("err = %v, want ErrMalformed", err)
	}
}

// TestEmptyCelIsHarmless: a 0x0 cel is legal and simply contributes nothing.
func TestEmptyCelIsHarmless(t *testing.T) {
	data := newBuilder(2, 2, 32).
		frame(100,
			layerChunk("L", true, 255, blendModeNormal, layerTypeNormal),
			celChunk(0, 0, 0, 255, 0, 0, nil),
		).bytes()

	f, err := Decode(data, Options{})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(f.Frames) != 1 {
		t.Errorf("%d frames, want 1", len(f.Frames))
	}
}

// TestFuzzyGarbageDoesNotPanic sweeps mutations of a valid file. The parser reads
// untrusted input, so the only acceptable failure mode is an error.
func TestFuzzyGarbageDoesNotPanic(t *testing.T) {
	valid := newBuilder(4, 4, 32).
		frame(100,
			layerChunk("L", true, 255, blendModeNormal, layerTypeNormal),
			celChunk(0, 0, 0, 255, 4, 4, rgbaPixels(4, 4, color.RGBA{A: 0xff})),
			tagsChunk(Tag{Name: "idle", From: 0, To: 0}),
		).bytes()

	// Truncate at every length.
	for cut := 0; cut < len(valid); cut++ {
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					t.Fatalf("panic on a %d-byte truncation: %v", cut, rec)
				}
			}()
			Decode(valid[:cut], Options{}) //nolint:errcheck
		}()
	}

	// Flip one byte at a time.
	for i := 0; i < len(valid); i++ {
		mutated := append([]byte(nil), valid...)
		mutated[i] ^= 0xff
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					t.Fatalf("panic on a byte flip at offset %d: %v", i, rec)
				}
			}()
			Decode(mutated, Options{}) //nolint:errcheck
		}()
	}
}

// assertPixel checks one pixel of one frame, allowing a small rounding tolerance from
// the alpha compositing arithmetic.
func assertPixel(t *testing.T, f *File, frame, x, y int, want color.RGBA) {
	t.Helper()

	if frame >= len(f.Frames) {
		t.Fatalf("frame %d does not exist", frame)
	}
	r, g, b, a := f.Frames[frame].Image.At(x, y).RGBA()
	got := color.RGBA{R: uint8(r >> 8), G: uint8(g >> 8), B: uint8(b >> 8), A: uint8(a >> 8)}

	const tolerance = 2
	near := func(x, y uint8) bool {
		if x > y {
			return x-y <= tolerance
		}
		return y-x <= tolerance
	}
	if !near(got.R, want.R) || !near(got.G, want.G) || !near(got.B, want.B) || !near(got.A, want.A) {
		t.Errorf("frame %d pixel (%d,%d) = %+v, want %+v", frame, x, y, got, want)
	}
}
