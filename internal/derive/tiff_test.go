package derive

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"image/color"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/image/tiff"
)

// tiffSpec is one TIFF for encodeTIFF to write: 8-bit, one strip.
type tiffSpec struct {
	w, h        int
	photometric uint16
	samples     int
	// extra is the ExtraSamples value, or -1 to leave the tag out.
	extra       int
	compression uint16
	predictor   uint16
	pix         []byte
}

// encodeTIFF writes a little-endian baseline TIFF with the strip after the header and
// the IFD after the strip.
func encodeTIFF(t *testing.T, s tiffSpec) []byte {
	t.Helper()
	data := append([]byte(nil), s.pix...)
	if s.predictor == 2 {
		row := s.w * s.samples
		for y := 0; y < s.h; y++ {
			r := data[y*row : (y+1)*row]
			for i := len(r) - 1; i >= s.samples; i-- {
				r[i] -= r[i-s.samples]
			}
		}
	}
	if s.compression == 8 {
		var z bytes.Buffer
		zw := zlib.NewWriter(&z)
		zw.Write(data)
		zw.Close()
		data = z.Bytes()
	}

	le := binary.LittleEndian
	var out bytes.Buffer
	out.WriteString("II*\x00")
	ifd := 8 + len(data)
	ifd += ifd % 2
	binary.Write(&out, le, uint32(ifd))
	out.Write(data)
	if out.Len()%2 == 1 {
		out.WriteByte(0)
	}

	type entry struct {
		tag, typ uint16
		vals     []uint32
	}
	bits := make([]uint32, s.samples)
	for i := range bits {
		bits[i] = 8
	}
	entries := []entry{
		{256, 4, []uint32{uint32(s.w)}}, {257, 4, []uint32{uint32(s.h)}}, {258, 3, bits},
		{259, 3, []uint32{uint32(s.compression)}}, {262, 3, []uint32{uint32(s.photometric)}},
		{273, 4, []uint32{8}}, {277, 3, []uint32{uint32(s.samples)}}, {278, 4, []uint32{uint32(s.h)}},
		{279, 4, []uint32{uint32(len(data))}}, {284, 3, []uint32{1}},
	}
	if s.predictor != 0 {
		entries = append(entries, entry{317, 3, []uint32{uint32(s.predictor)}})
	}
	if s.extra >= 0 {
		entries = append(entries, entry{338, 3, []uint32{uint32(s.extra)}})
	}

	// Values too long for their entry go after the IFD.
	var spill bytes.Buffer
	spillAt := ifd + 2 + 12*len(entries) + 4
	binary.Write(&out, le, uint16(len(entries)))
	for _, e := range entries {
		var v bytes.Buffer
		for _, x := range e.vals {
			if e.typ == 3 {
				binary.Write(&v, le, uint16(x))
			} else {
				binary.Write(&v, le, x)
			}
		}
		binary.Write(&out, le, e.tag)
		binary.Write(&out, le, e.typ)
		binary.Write(&out, le, uint32(len(e.vals)))
		if v.Len() <= 4 {
			field := make([]byte, 4)
			copy(field, v.Bytes())
			out.Write(field)
		} else {
			binary.Write(&out, le, uint32(spillAt+spill.Len()))
			spill.Write(v.Bytes())
		}
	}
	binary.Write(&out, le, uint32(0))
	out.Write(spill.Bytes())
	return out.Bytes()
}

func writeTIFF(t *testing.T, s tiffSpec) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "map.tif")
	if err := os.WriteFile(path, encodeTIFF(t, s), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestTIFFWithAnUnspecifiedChannel is Synty's metallic map: RGB plus smoothness in a
// fourth channel marked unspecified, Deflate with the predictor. x/image/tiff refuses it;
// the preview is its colour, opaque, with the fourth channel left out.
func TestTIFFWithAnUnspecifiedChannel(t *testing.T) {
	pix := []byte{
		255, 0, 0, 10, 0, 255, 0, 200, 0, 0, 255, 0,
		10, 20, 30, 40, 200, 150, 100, 50, 255, 255, 255, 255,
	}
	path := writeTIFF(t, tiffSpec{w: 3, h: 2, photometric: 2, samples: 4, extra: 0,
		compression: 8, predictor: 2, pix: pix})

	f, _ := os.Open(path)
	if _, err := tiff.Decode(f); err == nil {
		t.Fatal("x/image/tiff read the file, so this test proves nothing")
	}
	f.Close()

	src, err := Decode(path, "tif", DecodeOptions{})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	img := src.First()
	for y := 0; y < 2; y++ {
		for x := 0; x < 3; x++ {
			p := pix[(y*3+x)*4:]
			want := color.NRGBA{p[0], p[1], p[2], 255}
			if got := color.NRGBAModel.Convert(img.At(x, y)); got != want {
				t.Errorf("(%d,%d) = %v, want %v", x, y, got, want)
			}
		}
	}
}

// Greyscale with alpha, which x/image/tiff does not read at all: the alpha is kept.
func TestTIFFGreyWithAlpha(t *testing.T) {
	path := writeTIFF(t, tiffSpec{w: 2, h: 1, photometric: 1, samples: 2, extra: 2,
		compression: 1, pix: []byte{100, 255, 30, 0}})
	src, err := Decode(path, "tif", DecodeOptions{})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	for x, want := range []color.NRGBA{{100, 100, 100, 255}, {30, 30, 30, 0}} {
		if got := color.NRGBAModel.Convert(src.First().At(x, 0)); got != want {
			t.Errorf("pixel %d = %v, want %v", x, got, want)
		}
	}
}

// A TIFF neither decoder reads is unsupported, not failed: here JPEG compression.
func TestTIFFNeitherReadsIsUnsupported(t *testing.T) {
	path := writeTIFF(t, tiffSpec{w: 1, h: 1, photometric: 1, samples: 2, extra: 0,
		compression: 7, pix: []byte{1, 2}})
	if _, err := Decode(path, "tif", DecodeOptions{}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("err = %v, want an ErrUnsupported wrap", err)
	}
}

// The pixel limit holds for the fallback too, whose header DecodeConfig cannot read.
func TestTIFFFallbackKeepsThePixelLimit(t *testing.T) {
	path := writeTIFF(t, tiffSpec{w: 4, h: 4, photometric: 1, samples: 2, extra: 0,
		compression: 1, pix: make([]byte, 32)})
	if _, err := Decode(path, "tif", DecodeOptions{MaxPixels: 8}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("err = %v, want the pixel limit as ErrUnsupported", err)
	}
}
