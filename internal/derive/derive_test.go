package derive

import (
	"bytes"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/image/webp"
)

// --- fixture builders -------------------------------------------------------

// pixelArtSprite builds the archetype: few flat colours, hard edges, no antialiasing.
func pixelArtSprite(w, h int) image.Image {
	palette := []color.RGBA{
		{R: 0x00, G: 0x00, B: 0x00, A: 0xff},
		{R: 0xff, G: 0xff, B: 0xff, A: 0xff},
		{R: 0xd9, G: 0x3f, B: 0x3f, A: 0xff},
		{R: 0x3f, G: 0x8f, B: 0xd9, A: 0xff},
		{R: 0x4f, G: 0xa8, B: 0x4f, A: 0xff},
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			// Blocks of 4x4, so runs are flat and transitions are abrupt.
			img.Set(x, y, palette[((x/4)+(y/4)*3)%len(palette)])
		}
	}
	return img
}

// photograph builds a smooth gradient with per-channel noise: many colours, soft
// transitions.
//
// Each channel varies independently. An earlier version derived green and blue from
// red, which made it a one-dimensional ramp of only ~200 distinct colours — nothing
// like a photograph, and it hid the fact that the colour-count cap was never exercised.
func photograph(w, h int) image.Image {
	rng := rand.New(rand.NewSource(1))
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			fx, fy := float64(x)/float64(w), float64(y)/float64(h)
			channel := func(base float64) uint8 {
				return uint8(math.Max(0, math.Min(255, base+rng.Float64()*20-10)))
			}
			img.Set(x, y, color.RGBA{
				R: channel(40 + fx*180),
				G: channel(30 + fy*190),
				B: channel(60 + (fx+fy)/2*150),
				A: 0xff,
			})
		}
	}
	return img
}

// antialiasedShape has few colours but soft edges — the case that separates a real
// edge test from a naive colour count.
func antialiasedShape(w, h int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	cx, cy, r := float64(w)/2, float64(h)/2, float64(min(w, h))/3

	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			d := math.Hypot(float64(x)-cx, float64(y)-cy)
			// A soft 3px falloff at the circle's edge.
			var coverage float64
			switch {
			case d < r-1.5:
				coverage = 1
			case d > r+1.5:
				coverage = 0
			default:
				coverage = (r + 1.5 - d) / 3
			}
			v := uint8(coverage * 255)
			img.Set(x, y, color.RGBA{R: v, G: v, B: v, A: 0xff})
		}
	}
	return img
}

func writePNG(t *testing.T, dir, name string, img image.Image) string {
	t.Helper()

	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// --- pixel-art detection ----------------------------------------------------

// TestPixelArtDetection is the test behind §6's "get this right". Misclassifying here
// means either mushed sprites or needlessly blocky photographs.
func TestPixelArtDetection(t *testing.T) {
	tests := []struct {
		name      string
		img       image.Image
		wantPixel bool
		why       string
	}{
		{
			name:      "small sprite",
			img:       pixelArtSprite(32, 32),
			wantPixel: true,
			why:       "few flat colours and hard edges",
		},
		{
			name:      "large pixel-art atlas",
			img:       pixelArtSprite(2048, 2048),
			wantPixel: true,
			why: "§6's literal 'either axis under 256' would reject this, but a 2048px " +
				"pixel-art tileset is common and must not be bilinear-downscaled",
		},
		{
			name:      "photograph",
			img:       photograph(800, 600),
			wantPixel: false,
			why:       "thousands of colours and smooth gradients",
		},
		{
			name:      "small photograph",
			img:       photograph(64, 64),
			wantPixel: false,
			why:       "small, but still a gradient — dimensions alone must not decide",
		},
		{
			name:      "antialiased vector shape",
			img:       antialiasedShape(256, 256),
			wantPixel: false,
			why:       "few colours but soft edges, which is what the edge test is for",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := Analyse(tc.img)
			if a.IsPixelArt != tc.wantPixel {
				t.Errorf("IsPixelArt = %v, want %v (%s)\n  colors=%d softTransitionRatio=%.3f size=%dx%d",
					a.IsPixelArt, tc.wantPixel, tc.why,
					a.ColorCount, a.SoftTransitionRatio, a.Width, a.Height)
			}
		})
	}
}

func TestAnalyseAlpha(t *testing.T) {
	opaque := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for i := range opaque.Pix {
		opaque.Pix[i] = 0xff
	}
	if a := Analyse(opaque); a.HasAlpha || a.HasSemitransparent {
		t.Errorf("a fully opaque image reported alpha: %+v", a)
	}

	// Fully transparent pixels: alpha yes, semi-transparent no.
	hardAlpha := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			if x < 4 {
				hardAlpha.Set(x, y, color.RGBA{R: 0xff, A: 0xff})
			} else {
				hardAlpha.Set(x, y, color.RGBA{})
			}
		}
	}
	a := Analyse(hardAlpha)
	if !a.HasAlpha {
		t.Error("transparent pixels were not detected")
	}
	if a.HasSemitransparent {
		t.Error("fully transparent pixels were counted as semi-transparent")
	}

	// Partial alpha, which §8 says is usually an authoring mistake in pixel art.
	soft := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for i := 0; i < 64; i++ {
		soft.Set(i%8, i/8, color.RGBA{R: 0xff, A: 0x80})
	}
	if a := Analyse(soft); !a.HasSemitransparent {
		t.Error("semi-transparent pixels were not detected")
	}
}

// TestAnalyseExcludesTransparentFromTheColourCount is §8's rule: otherwise "the
// palette is dominated by transparent black".
func TestAnalyseExcludesTransparentFromTheColourCount(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 16, 16))
	// One visible colour, the rest fully transparent.
	for y := 0; y < 16; y++ {
		for x := 0; x < 16; x++ {
			if x == 0 {
				img.Set(x, y, color.RGBA{R: 0xff, A: 0xff})
			}
		}
	}
	a := Analyse(img)
	if a.ColorCount != 1 {
		t.Errorf("ColorCount = %d, want 1 — transparent pixels must not count", a.ColorCount)
	}
}

func TestAnalyseColorCountIsCapped(t *testing.T) {
	a := Analyse(photograph(600, 600))
	if a.ColorCount > ColorCountCap {
		t.Errorf("ColorCount = %d, over the cap of %d", a.ColorCount, ColorCountCap)
	}
	if a.ColorCountExact {
		t.Error("a photograph reported an exact colour count; the cap was not hit")
	}

	exact := Analyse(pixelArtSprite(32, 32))
	if !exact.ColorCountExact {
		t.Error("a 5-colour sprite did not report an exact count")
	}
	if exact.ColorCount != 5 {
		t.Errorf("ColorCount = %d, want 5", exact.ColorCount)
	}
}

func TestAnalyseEmptyImage(t *testing.T) {
	// Must not divide by zero or panic.
	a := Analyse(image.NewRGBA(image.Rect(0, 0, 0, 0)))
	if a.Width != 0 || a.Height != 0 {
		t.Errorf("analysis of an empty image = %+v", a)
	}
}

// --- resizing ---------------------------------------------------------------

// TestPixelArtResizePreservesThePalette is the concrete form of §6's mush complaint.
// Nearest-neighbour output may only contain colours from the original; any smoothing
// filter invents intermediate ones.
func TestPixelArtResizePreservesThePalette(t *testing.T) {
	src := pixelArtSprite(256, 256)

	original := map[color.RGBA]bool{}
	b := src.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			original[toRGBA(src.At(x, y))] = true
		}
	}

	scaled := Fit(src, 64, true)
	sb := scaled.Bounds()
	if sb.Dx() != 64 || sb.Dy() != 64 {
		t.Fatalf("scaled to %dx%d, want 64x64", sb.Dx(), sb.Dy())
	}

	for y := sb.Min.Y; y < sb.Max.Y; y++ {
		for x := sb.Min.X; x < sb.Max.X; x++ {
			got := toRGBA(scaled.At(x, y))
			if !original[got] {
				t.Fatalf("nearest-neighbour downscale invented the colour %+v at (%d,%d); "+
					"this is the mush failure §6 warns about", got, x, y)
			}
		}
	}

	// And the contrast: a smoothing filter *does* invent colours, which is why the
	// branch exists at all.
	smoothed := Fit(src, 64, false)
	invented := 0
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			if !original[toRGBA(smoothed.At(x, y))] {
				invented++
			}
		}
	}
	if invented == 0 {
		t.Error("the smoothing path invented no new colours, so the two paths are not distinct")
	}
}

func TestFitPreservesAspectRatio(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 800, 400))
	scaled := Fit(src, 100, false)
	b := scaled.Bounds()
	if b.Dx() != 100 || b.Dy() != 50 {
		t.Errorf("scaled to %dx%d, want 100x50", b.Dx(), b.Dy())
	}
}

// TestFitDoesNotUpscale: blowing a 32x32 sprite up to 512x512 server-side would ship
// 250x the bytes for the same information, and §8's viewer scales it client-side.
func TestFitDoesNotUpscale(t *testing.T) {
	src := pixelArtSprite(32, 32)
	scaled := Fit(src, 512, true)
	if b := scaled.Bounds(); b.Dx() != 32 || b.Dy() != 32 {
		t.Errorf("a small image was resized to %dx%d, want 32x32 untouched", b.Dx(), b.Dy())
	}
}

func TestFitHandlesExtremeAspectRatios(t *testing.T) {
	// A 2000x1 strip must not round to a zero-height image.
	scaled := Fit(image.NewRGBA(image.Rect(0, 0, 2000, 1)), 100, false)
	if b := scaled.Bounds(); b.Dy() < 1 || b.Dx() < 1 {
		t.Errorf("scaled to %dx%d; neither axis may be zero", b.Dx(), b.Dy())
	}
}

// TestCompositeOverRemovesTransparency is §6's dark-UI requirement.
func TestCompositeOverRemovesTransparency(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 4, 4)) // fully transparent
	out := CompositeOver(src, MidGrey)

	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			c := toRGBA(out.At(x, y))
			if c.A != 0xff {
				t.Fatalf("pixel (%d,%d) has alpha %d, want opaque", x, y, c.A)
			}
			if c.R != MidGrey.R || c.G != MidGrey.G || c.B != MidGrey.B {
				t.Fatalf("pixel (%d,%d) = %+v, want mid-grey", x, y, c)
			}
		}
	}
}

// --- perceptual hash --------------------------------------------------------

func TestPerceptualHash(t *testing.T) {
	src := photograph(200, 200)

	h1 := PerceptualHash(src)
	if len(h1) != 16 {
		t.Fatalf("hash is %q, want 16 hex characters", h1)
	}
	// Deterministic.
	if h2 := PerceptualHash(src); h1 != h2 {
		t.Errorf("hash is not deterministic: %q then %q", h1, h2)
	}

	// The property M13 depends on: the same image at a different resolution hashes
	// close. §5 calls this "the same sprite pack downloaded twice at different
	// resolutions is the common case".
	half := Fit(src, 100, false)
	d, err := HammingDistance(h1, PerceptualHash(half))
	if err != nil {
		t.Fatal(err)
	}
	if d > 12 {
		t.Errorf("the same image at half size differs by %d bits, want a small distance", d)
	}

	// And an unrelated image hashes far away.
	far, err := HammingDistance(h1, PerceptualHash(pixelArtSprite(200, 200)))
	if err != nil {
		t.Fatal(err)
	}
	if far <= d {
		t.Errorf("an unrelated image is %d bits away but a rescale is %d; the hash is not discriminating",
			far, d)
	}
}

func TestHammingDistanceRejectsBadInput(t *testing.T) {
	for _, tc := range [][2]string{
		{"", ""},
		{"abc", "abc"},
		{"zzzzzzzzzzzzzzzz", "0000000000000000"},
		{"00000000000000000", "0000000000000000"},
	} {
		if _, err := HammingDistance(tc[0], tc[1]); err == nil {
			t.Errorf("HammingDistance(%q, %q) succeeded", tc[0], tc[1])
		}
	}
}

// --- the derivative directory layout ----------------------------------------

func TestDirLayoutMatchesTheSpec(t *testing.T) {
	// §3: derivatives/<sha[0:2]>/<sha>/
	hash := "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	dir, err := Dir(hash)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join("derivatives", "ab", hash)
	if dir != want {
		t.Errorf("Dir = %q, want %q", dir, want)
	}
}

// TestDirRejectsNonHashes matters because this value becomes a filesystem path.
func TestDirRejectsNonHashes(t *testing.T) {
	for _, bad := range []string{
		"", "short", "../../etc/passwd",
		"zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz",
		"abcdef0123456789abcdef0123456789abcdef0123456789abcdef012345678", // 63 chars
		"/abcdef0123456789abcdef0123456789abcdef0123456789abcdef012345678",
	} {
		if _, err := Dir(bad); err == nil {
			t.Errorf("Dir(%q) was accepted", bad)
		}
	}
}

// --- end-to-end generation --------------------------------------------------

func TestGenerateWritesTheThumbnailSet(t *testing.T) {
	libRoot := t.TempDir()
	dataRoot := t.TempDir()

	path := writePNG(t, libRoot, "sprite.png", pixelArtSprite(64, 64))
	hash := ContentHash(mustRead(t, path))

	result, err := Generate(GenerateOptions{
		AbsPath: path, Ext: "png", SHA256: hash, DataRoot: dataRoot,
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	if !result.Analysis.IsPixelArt {
		t.Error("the sprite was not detected as pixel art")
	}
	if result.PHash == "" {
		t.Error("no perceptual hash was computed")
	}

	relDir, err := Dir(hash)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{FileThumb, FileThumb2x, FileThumbAlpha, FilePreview} {
		full := filepath.Join(dataRoot, relDir, name)
		info, err := os.Stat(full)
		if err != nil {
			t.Errorf("%s was not written: %v", name, err)
			continue
		}
		if info.Size() == 0 {
			t.Errorf("%s is empty", name)
		}
		// It has to be decodable WebP, not just bytes on disk.
		f, err := os.Open(full)
		if err != nil {
			t.Fatal(err)
		}
		img, err := webp.Decode(f)
		f.Close()
		if err != nil {
			t.Errorf("%s is not valid WebP: %v", name, err)
			continue
		}
		if img.Bounds().Empty() {
			t.Errorf("%s decoded to an empty image", name)
		}
	}

	// No animation for a still image.
	if _, err := os.Stat(filepath.Join(dataRoot, relDir, FileAnimation)); err == nil {
		t.Error("an animated preview was written for a still image")
	}
}

// TestGenerateCompositesTheThumbnailButNotTheAlphaVersion covers §6 wanting both.
func TestGenerateCompositesTheThumbnailButNotTheAlphaVersion(t *testing.T) {
	libRoot := t.TempDir()
	dataRoot := t.TempDir()

	// Half transparent, half red.
	img := image.NewRGBA(image.Rect(0, 0, 32, 32))
	for y := 0; y < 32; y++ {
		for x := 16; x < 32; x++ {
			img.Set(x, y, color.RGBA{R: 0xff, A: 0xff})
		}
	}
	path := writePNG(t, libRoot, "half.png", img)
	hash := ContentHash(mustRead(t, path))

	if _, err := Generate(GenerateOptions{
		AbsPath: path, Ext: "png", SHA256: hash, DataRoot: dataRoot,
	}); err != nil {
		t.Fatal(err)
	}

	relDir, _ := Dir(hash)
	opaque := decodeWebP(t, filepath.Join(dataRoot, relDir, FileThumb))
	transparent := decodeWebP(t, filepath.Join(dataRoot, relDir, FileThumbAlpha))

	if c := toRGBA(opaque.At(2, 2)); c.A != 0xff {
		t.Errorf("thumb.webp has alpha %d at a transparent source pixel, want opaque mid-grey", c.A)
	}
	if c := toRGBA(transparent.At(2, 2)); c.A != 0 {
		t.Errorf("thumb-alpha.webp has alpha %d, want the transparency preserved", c.A)
	}
}

func TestGenerateAnimatedGIF(t *testing.T) {
	libRoot := t.TempDir()
	dataRoot := t.TempDir()

	// A three-frame GIF.
	anim := &gif.GIF{LoopCount: 0}
	for i := 0; i < 3; i++ {
		frame := image.NewPaletted(image.Rect(0, 0, 16, 16), color.Palette{
			color.RGBA{A: 0xff}, color.RGBA{R: 0xff, A: 0xff}, color.RGBA{B: 0xff, A: 0xff},
		})
		for p := range frame.Pix {
			frame.Pix[p] = uint8(i % 3)
		}
		anim.Image = append(anim.Image, frame)
		anim.Delay = append(anim.Delay, 5) // 50ms
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, anim); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(libRoot, "spin.gif")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	hash := ContentHash(buf.Bytes())

	result, err := Generate(GenerateOptions{
		AbsPath: path, Ext: "gif", SHA256: hash, DataRoot: dataRoot,
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	if result.FrameCount != 3 {
		t.Errorf("FrameCount = %d, want 3", result.FrameCount)
	}
	if result.FPS < 15 || result.FPS > 25 {
		t.Errorf("FPS = %.1f, want about 20 from 50ms frames", result.FPS)
	}

	relDir, _ := Dir(hash)
	animPath := filepath.Join(dataRoot, relDir, FileAnimation)
	f, err := os.Open(animPath)
	if err != nil {
		t.Fatalf("no animated preview: %v", err)
	}
	defer f.Close()
	decoded, err := gif.DecodeAll(f)
	if err != nil {
		t.Fatalf("the animated preview is not valid GIF: %v", err)
	}
	if len(decoded.Image) != 3 {
		t.Errorf("the preview has %d frames, want 3", len(decoded.Image))
	}
}

// TestGenerateUnsupportedFormats: §6 wants .xcf handled "gracefully", and these must
// be distinguishable from failures so the queue does not retry them.
func TestGenerateUnsupportedFormats(t *testing.T) {
	libRoot := t.TempDir()
	dataRoot := t.TempDir()

	// .wav/.mp3/.ogg/.flac (M5) and .glb/.gltf (M6) now derive; a *malformed* one
	// is a failure, tested separately. These remain formats with no decoder at all.
	// (.hdr/.exr are HDRI, still unsupported until their own path lands.)
	for _, ext := range []string{"xcf", "tga", "hdr", "exr", "zzz"} {
		path := filepath.Join(libRoot, "file."+ext)
		if err := os.WriteFile(path, []byte("some bytes"), 0o644); err != nil {
			t.Fatal(err)
		}
		hash := ContentHash([]byte("some bytes"))

		_, err := Generate(GenerateOptions{
			AbsPath: path, Ext: ext, SHA256: hash, DataRoot: dataRoot,
		})
		if err == nil {
			t.Errorf(".%s was accepted", ext)
			continue
		}
		if !isUnsupported(err) {
			t.Errorf(".%s returned %v, want an ErrUnsupported wrap so the queue does not retry it",
				ext, err)
		}
	}
}

// TestGenerateBrokenImages is §16's "deliberately broken ones".
func TestGenerateBrokenImages(t *testing.T) {
	libRoot := t.TempDir()
	dataRoot := t.TempDir()

	valid := func() []byte {
		var buf bytes.Buffer
		if err := png.Encode(&buf, pixelArtSprite(32, 32)); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}()

	broken := map[string][]byte{
		"truncated.png": valid[:len(valid)/2],
		"empty.png":     {},
		"garbage.png":   []byte("definitely not a png"),
		"header.png":    []byte("\x89PNG\r\n\x1a\n"),
	}
	for name, content := range broken {
		path := filepath.Join(libRoot, name)
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}
		// Never a panic, always an error.
		if _, err := Generate(GenerateOptions{
			AbsPath: path, Ext: "png", SHA256: ContentHash(content), DataRoot: dataRoot,
		}); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// TestMaxPixelsGuard is the image equivalent of §5's zip-bomb caps: a huge header must
// be refused before anything is allocated for it.
func TestMaxPixelsGuard(t *testing.T) {
	libRoot := t.TempDir()
	dataRoot := t.TempDir()

	// A PNG header claiming 20000x20000 (400 megapixels) with no real pixel data.
	// Only the header is read, so the file stays tiny.
	var buf bytes.Buffer
	buf.Write([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'})
	writeChunk(&buf, "IHDR", func() []byte {
		b := make([]byte, 13)
		putBE32(b[0:4], 20000)
		putBE32(b[4:8], 20000)
		b[8] = 8 // bit depth
		b[9] = 6 // truecolour with alpha
		return b
	}())
	path := filepath.Join(libRoot, "huge.png")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Generate(GenerateOptions{
		AbsPath: path, Ext: "png", SHA256: ContentHash(buf.Bytes()),
		DataRoot: dataRoot, MaxPixels: DefaultMaxPixels,
	})
	if err == nil {
		t.Fatal("a 400-megapixel image was accepted")
	}
	if !isUnsupported(err) {
		t.Errorf("err = %v, want an ErrUnsupported wrap", err)
	}
	// The message should name the knob, since that is what the operator would change.
	if !bytes.Contains([]byte(err.Error()), []byte("AMBAR_MAX_IMAGE_PIXELS")) {
		t.Errorf("err = %v, want it to name AMBAR_MAX_IMAGE_PIXELS", err)
	}
}

// --- helpers ----------------------------------------------------------------

func toRGBA(c color.Color) color.RGBA {
	r, g, b, a := c.RGBA()
	return color.RGBA{R: uint8(r >> 8), G: uint8(g >> 8), B: uint8(b >> 8), A: uint8(a >> 8)}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func decodeWebP(t *testing.T, path string) image.Image {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, err := webp.Decode(f)
	if err != nil {
		t.Fatalf("decode %s: %v", filepath.Base(path), err)
	}
	return img
}

func isUnsupported(err error) bool {
	for err != nil {
		if err == ErrUnsupported {
			return true
		}
		unwrapped, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapped.Unwrap()
	}
	return false
}

// writeChunk writes a PNG chunk with its CRC.
func writeChunk(buf *bytes.Buffer, kind string, data []byte) {
	length := make([]byte, 4)
	putBE32(length, uint32(len(data)))
	buf.Write(length)
	buf.WriteString(kind)
	buf.Write(data)

	crc := crc32Of(append([]byte(kind), data...))
	sum := make([]byte, 4)
	putBE32(sum, crc)
	buf.Write(sum)
}

func putBE32(b []byte, v uint32) {
	b[0], b[1], b[2], b[3] = byte(v>>24), byte(v>>16), byte(v>>8), byte(v)
}

func crc32Of(data []byte) uint32 {
	var table [256]uint32
	for i := range table {
		c := uint32(i)
		for range 8 {
			if c&1 != 0 {
				c = 0xEDB88320 ^ (c >> 1)
			} else {
				c >>= 1
			}
		}
		table[i] = c
	}
	crc := uint32(0xFFFFFFFF)
	for _, b := range data {
		crc = table[(crc^uint32(b))&0xFF] ^ (crc >> 8)
	}
	return crc ^ 0xFFFFFFFF
}

// TestTransitionRatiosAreWellSeparated documents the measurements the thresholds are
// set from, and fails if the gap closes.
//
// A threshold picked to make today's tests pass is a threshold nobody can safely
// change. This records the actual separation, so a future tweak to the metric shows up
// as a narrowing margin rather than as a mysterious misclassification.
func TestTransitionRatiosAreWellSeparated(t *testing.T) {
	cases := []struct {
		name      string
		img       image.Image
		wantBelow bool // below the threshold means "sharp", i.e. pixel art
	}{
		{"flat-palette sprite", pixelArtSprite(128, 128), true},
		{"large pixel-art atlas", pixelArtSprite(1024, 1024), true},
		{"antialiased vector shape", antialiasedShape(256, 256), false},
		{"photograph", photograph(400, 400), false},
	}

	const margin = 0.10

	for _, tc := range cases {
		a := Analyse(tc.img)
		t.Logf("%-26s colors=%-5d transitions=%-7d softRatio=%.3f  pixelArt=%v",
			tc.name, a.ColorCount, a.TransitionSamples, a.SoftTransitionRatio, a.IsPixelArt)

		if tc.wantBelow {
			if a.SoftTransitionRatio > pixelArtMaxSoftRatio-margin {
				t.Errorf("%s scores %.3f, uncomfortably close to the %.2f threshold",
					tc.name, a.SoftTransitionRatio, pixelArtMaxSoftRatio)
			}
		} else if a.SoftTransitionRatio < pixelArtMaxSoftRatio+margin &&
			a.ColorCount <= pixelArtMaxColors {
			// Only a problem when colour count would not have caught it anyway.
			t.Errorf("%s scores %.3f, uncomfortably close to the %.2f threshold",
				tc.name, a.SoftTransitionRatio, pixelArtMaxSoftRatio)
		}
	}
}

// TestGenerateAudioWAV proves the M5 audio path: a real WAV yields metadata and
// a peaks.json rather than an image.
func TestGenerateAudioWAV(t *testing.T) {
	libRoot := t.TempDir()
	dataRoot := t.TempDir()

	// A minimal 16-bit mono WAV: 4410 samples (0.1 s) of a soft tone.
	var pcm bytes.Buffer
	for i := 0; i < 4410; i++ {
		v := int16(8000 * math.Sin(2*3.141592653589793*440*float64(i)/44100))
		pcm.WriteByte(byte(v))
		pcm.WriteByte(byte(v >> 8))
	}
	body := pcm.Bytes()
	var w bytes.Buffer
	w.WriteString("RIFF")
	writeU32(&w, uint32(36+len(body)))
	w.WriteString("WAVEfmt ")
	writeU32(&w, 16)
	writeU16(&w, 1)
	writeU16(&w, 1)
	writeU32(&w, 44100)
	writeU32(&w, 44100*2)
	writeU16(&w, 2)
	writeU16(&w, 16)
	w.WriteString("data")
	writeU32(&w, uint32(len(body)))
	w.Write(body)

	path := filepath.Join(libRoot, "beep.wav")
	if err := os.WriteFile(path, w.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	hash := ContentHash(w.Bytes())

	res, err := Generate(GenerateOptions{AbsPath: path, Ext: "wav", SHA256: hash, DataRoot: dataRoot})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if res.Audio == nil {
		t.Fatal("no audio metadata produced")
	}
	if res.Audio.SampleRate != 44100 || res.Audio.Channels != 1 || res.Audio.BitDepth != 16 {
		t.Errorf("audio metadata wrong: %+v", res.Audio)
	}
	rel, _ := Dir(hash)
	if _, err := os.Stat(filepath.Join(dataRoot, rel, FilePeaks)); err != nil {
		t.Errorf("peaks.json not written: %v", err)
	}
}

func writeU32(w *bytes.Buffer, v uint32) {
	w.WriteByte(byte(v))
	w.WriteByte(byte(v >> 8))
	w.WriteByte(byte(v >> 16))
	w.WriteByte(byte(v >> 24))
}
func writeU16(w *bytes.Buffer, v uint16) {
	w.WriteByte(byte(v))
	w.WriteByte(byte(v >> 8))
}

func TestGenerateFBXNeedsBlender(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mesh.fbx")
	os.WriteFile(path, []byte("Kaydara FBX Binary"), 0o644)
	_, err := Generate(GenerateOptions{
		AbsPath: path, Ext: "fbx", SHA256: ContentHash([]byte("Kaydara FBX Binary")), DataRoot: dir,
	})
	if !isNeedsBlender(err) {
		t.Errorf(".fbx returned %v, want ErrNeedsBlender", err)
	}
}

func isNeedsBlender(err error) bool {
	for err != nil {
		if err == ErrNeedsBlender {
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func TestGenerateSpritesheet(t *testing.T) {
	libRoot := t.TempDir()
	dataRoot := t.TempDir()

	// A 4x3 grid of 20px cells with transparent seam lines — a clean sheet.
	cols, rows, cell := 4, 3, 20
	w, h := cols*cell, rows*cell
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if (x%cell == 0 && x > 0) || (y%cell == 0 && y > 0) {
				img.Set(x, y, color.RGBA{})
			} else {
				img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 0x80, A: 0xff})
			}
		}
	}
	path := writePNG(t, libRoot, "hero_sheet.png", img)
	data, _ := os.ReadFile(path)
	hash := ContentHash(data)

	res, err := Generate(GenerateOptions{AbsPath: path, Ext: "png", SHA256: hash, DataRoot: dataRoot})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if res.Sheet == nil {
		t.Fatal("no spritesheet detected on a clean sheet")
	}
	if res.Sheet.Cols != cols || res.Sheet.Rows != rows {
		t.Errorf("grid = %dx%d, want %dx%d", res.Sheet.Cols, res.Sheet.Rows, cols, rows)
	}
	if !res.Sheet.Confident {
		t.Errorf("clean sheet not confident")
	}
	// A confident sheet gets an animated preview.
	rel, _ := Dir(hash)
	if _, err := os.Stat(filepath.Join(dataRoot, rel, FileSheet)); err != nil {
		t.Errorf("sheet.gif not written: %v", err)
	}
}
