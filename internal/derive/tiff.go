package derive

import (
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"io"

	"golang.org/x/image/tiff"
	"golang.org/x/image/tiff/lzw"
)

// TIFFs that x/image/tiff refuses for their extra channels.
//
// Its decoder reads an RGB TIFF with a fourth channel only when the file marks that
// channel as alpha, and a greyscale TIFF with any extra channel not at all. Synty ships
// its metallic and specular maps with smoothness in a fourth channel marked
// "unspecified", which the TIFF spec allows, and every one was a failed preview that a
// retry failed again. Those are read here instead: strips of 8-bit samples, stored
// plain, LZW or Deflate, with or without the horizontal predictor — what image editors
// write. An extra channel is alpha when the file says so and is otherwise left out:
// smoothness drawn as transparency would misdraw the map.

// The baseline tags this reader looks at.
const (
	tiffImageWidth      = 256
	tiffImageLength     = 257
	tiffBitsPerSample   = 258
	tiffCompression     = 259
	tiffPhotometric     = 262
	tiffStripOffsets    = 273
	tiffSamplesPerPixel = 277
	tiffRowsPerStrip    = 278
	tiffStripByteCounts = 279
	tiffPlanarConfig    = 284
	tiffPredictor       = 317
	tiffTileWidth       = 322
	tiffExtraSamples    = 338
	tiffSampleFormat    = 339
)

// errNotExtraSamples means the file is not a TIFF this fallback reads, so x/image's own
// error stands.
var errNotExtraSamples = errors.New("not a TIFF with extra samples")

// decodeTIFFFallback is decodeStill's second try after image.Decode failed with first.
// A TIFF this reads is returned; a TIFF neither reads is unsupported when x/image said
// so, and failed — a broken file — when x/image said its format was wrong. Any other
// error is returned as it is.
func decodeTIFFFallback(r io.ReaderAt, first error, maxPixels int64) (image.Image, error) {
	var unsupported tiff.UnsupportedError
	var format tiff.FormatError
	isUnsupported := errors.As(first, &unsupported)
	if !isUnsupported && !errors.As(first, &format) {
		return nil, first
	}
	img, err := decodeTIFFExtraSamples(r, maxPixels)
	switch {
	case err == nil:
		return img, nil
	case !errors.Is(err, errNotExtraSamples):
		return nil, err
	case isUnsupported:
		return nil, fmt.Errorf("%w: %v", ErrUnsupported, first)
	}
	return nil, first
}

// decodeTIFFExtraSamples reads a TIFF that carries channels beyond grey or RGB.
func decodeTIFFExtraSamples(r io.ReaderAt, maxPixels int64) (image.Image, error) {
	var head [8]byte
	if _, err := r.ReadAt(head[:], 0); err != nil {
		return nil, errNotExtraSamples
	}
	var bo binary.ByteOrder
	switch string(head[:4]) {
	case "II*\x00":
		bo = binary.LittleEndian
	case "MM\x00*":
		bo = binary.BigEndian
	default:
		return nil, errNotExtraSamples // BigTIFF, or not a TIFF at all
	}
	tags, err := readTIFFTags(r, bo, int64(bo.Uint32(head[4:])))
	if err != nil {
		return nil, err
	}
	first := func(tag uint16, fallback uint32) uint32 {
		if v := tags[tag]; len(v) > 0 {
			return v[0]
		}
		return fallback
	}

	photometric := first(tiffPhotometric, ^uint32(0))
	var colour int // samples that make the colour: grey or RGB
	switch photometric {
	case 0, 1: // WhiteIsZero, BlackIsZero
		colour = 1
	case 2:
		colour = 3
	default:
		return nil, errNotExtraSamples
	}
	bits := tags[tiffBitsPerSample]
	samples := int(first(tiffSamplesPerPixel, 1))
	if samples <= colour || len(bits) != samples || len(tags[tiffTileWidth]) > 0 ||
		first(tiffPlanarConfig, 1) != 1 || first(tiffSampleFormat, 1) != 1 {
		return nil, errNotExtraSamples
	}
	for _, b := range bits {
		if b != 8 {
			return nil, errNotExtraSamples
		}
	}
	compression := first(tiffCompression, 1)
	switch compression {
	case 1, 5, 8, 32946: // none, LZW, Deflate, and Deflate's old number
	default:
		return nil, errNotExtraSamples
	}
	predictor := first(tiffPredictor, 1)
	if predictor != 1 && predictor != 2 {
		return nil, errNotExtraSamples
	}

	w, h := int(first(tiffImageWidth, 0)), int(first(tiffImageLength, 0))
	if w <= 0 || h <= 0 {
		return nil, tiff.FormatError("no image size")
	}
	if px := int64(w) * int64(h); px > maxPixels {
		return nil, fmt.Errorf("%w: %dx%d is %d pixels, over the %d limit (AMBAR_MAX_IMAGE_PIXELS)",
			ErrUnsupported, w, h, px, maxPixels)
	}
	rowsPerStrip := int(first(tiffRowsPerStrip, uint32(h)))
	if rowsPerStrip <= 0 || rowsPerStrip > h {
		rowsPerStrip = h
	}
	strips := (h + rowsPerStrip - 1) / rowsPerStrip
	offsets, counts := tags[tiffStripOffsets], tags[tiffStripByteCounts]
	if len(offsets) < strips || len(counts) < strips {
		return nil, tiff.FormatError("fewer strips than rows need")
	}

	// 1 is premultiplied alpha and 2 straight alpha; 0, unspecified, is not alpha.
	alpha := first(tiffExtraSamples, 0)
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	rowLen := w * samples
	buf := make([]byte, rowsPerStrip*rowLen)
	for s := 0; s < strips; s++ {
		rows := min(rowsPerStrip, h-s*rowsPerStrip)
		data := buf[:rows*rowLen]
		if err := readTIFFStrip(r, compression, int64(offsets[s]), int64(counts[s]), data); err != nil {
			return nil, err
		}
		for y := 0; y < rows; y++ {
			row := data[y*rowLen : (y+1)*rowLen]
			if predictor == 2 {
				for i := samples; i < len(row); i++ {
					row[i] += row[i-samples]
				}
			}
			out := img.Pix[(s*rowsPerStrip+y)*img.Stride:]
			for x := 0; x < w; x++ {
				px := row[x*samples:]
				var r, g, b uint8
				if colour == 3 {
					r, g, b = px[0], px[1], px[2]
				} else {
					v := px[0]
					if photometric == 0 {
						v = 255 - v
					}
					r, g, b = v, v, v
				}
				a := uint8(255)
				if alpha == 1 || alpha == 2 {
					a = px[colour]
				}
				if alpha == 1 && a > 0 && a < 255 {
					r, g, b = unpremultiply(r, a), unpremultiply(g, a), unpremultiply(b, a)
				}
				o := out[4*x : 4*x+4 : 4*x+4]
				o[0], o[1], o[2], o[3] = r, g, b, a
			}
		}
	}
	return img, nil
}

func unpremultiply(c, a uint8) uint8 {
	return uint8(min(int(c)*255/int(a), 255))
}

// readTIFFTags reads the first IFD's values for the tags above, all of which are SHORT
// or LONG.
func readTIFFTags(r io.ReaderAt, bo binary.ByteOrder, offset int64) (map[uint16][]uint32, error) {
	var n [2]byte
	if _, err := r.ReadAt(n[:], offset); err != nil {
		return nil, tiff.FormatError("no IFD")
	}
	entries := make([]byte, 12*int(bo.Uint16(n[:])))
	if _, err := r.ReadAt(entries, offset+2); err != nil {
		return nil, tiff.FormatError("short IFD")
	}
	tags := map[uint16][]uint32{}
	for e := entries; len(e) >= 12; e = e[12:] {
		tag, typ, count := bo.Uint16(e), bo.Uint16(e[2:]), bo.Uint32(e[4:])
		switch tag {
		case tiffImageWidth, tiffImageLength, tiffBitsPerSample, tiffCompression, tiffPhotometric,
			tiffStripOffsets, tiffSamplesPerPixel, tiffRowsPerStrip, tiffStripByteCounts,
			tiffPlanarConfig, tiffPredictor, tiffTileWidth, tiffExtraSamples, tiffSampleFormat:
		default:
			continue
		}
		size := uint32(0)
		switch typ {
		case 3: // SHORT
			size = 2
		case 4: // LONG
			size = 4
		}
		// A strip per row of a 50-megapixel image is the most any of these can need.
		if size == 0 || count == 0 || count > 1<<24 {
			continue
		}
		raw := e[8:12]
		if count*size > 4 {
			raw = make([]byte, count*size)
			if _, err := r.ReadAt(raw, int64(bo.Uint32(e[8:]))); err != nil {
				return nil, tiff.FormatError("short tag value")
			}
		}
		vals := make([]uint32, count)
		for i := range vals {
			if size == 2 {
				vals[i] = uint32(bo.Uint16(raw[2*i:]))
			} else {
				vals[i] = bo.Uint32(raw[4*i:])
			}
		}
		tags[tag] = vals
	}
	return tags, nil
}

// readTIFFStrip fills dst with one strip, decompressed.
func readTIFFStrip(r io.ReaderAt, compression uint32, offset, n int64, dst []byte) error {
	var src io.Reader = io.NewSectionReader(r, offset, n)
	switch compression {
	case 5:
		lr := lzw.NewReader(src, lzw.MSB, 8)
		defer lr.Close()
		src = lr
	case 8, 32946:
		zr, err := zlib.NewReader(src)
		if err != nil {
			return fmt.Errorf("tiff: deflate strip at %d: %w", offset, err)
		}
		defer zr.Close()
		src = zr
	}
	if _, err := io.ReadFull(src, dst); err != nil {
		return fmt.Errorf("tiff: strip at %d: %w", offset, err)
	}
	return nil
}
