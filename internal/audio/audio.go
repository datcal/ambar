// Package audio decodes the §6 audio formats with pure-Go decoders (invariant 6)
// and produces what the library needs from a sound file without ever rendering an
// image: peaks for a client-side waveform (§8), the technical metadata (duration,
// sample rate, channels, bit depth, peak level), and a cheap loop-ability guess.
//
// Everything is mixed to mono for analysis — a waveform and a peak level do not
// need per-channel detail — while the original channel count is still reported.
package audio

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-audio/wav"
	"github.com/hajimehoshi/go-mp3"
	"github.com/jfreymuth/oggvorbis"
	"github.com/mewkiz/flac"
)

// ErrUnsupported means the extension has no pure-Go decoder here.
var ErrUnsupported = errors.New("no audio decoder for this format")

// DefaultBuckets is the §6 peak resolution: "roughly 2000 buckets".
const DefaultBuckets = 2000

// Info is the technical metadata extracted from a sound file (§4 audio columns).
type Info struct {
	DurationMS int
	SampleRate int
	Channels   int
	BitDepth   int // 0 when the format is float / has no integer depth (ogg)
	PeakDBFS   float64
	IsLoopable bool
}

// Peaks is the min/max envelope for a client-side waveform (§6, §8), one pair per
// bucket, normalised to [-1, 1].
type Peaks struct {
	Version int       `json:"version"`
	Count   int       `json:"count"`
	Min     []float32 `json:"min"`
	Max     []float32 `json:"max"`
}

// Result bundles what Analyze produced.
type Result struct {
	Info  Info
	Peaks Peaks
}

// mono is the decoded, channel-mixed signal plus its source parameters.
type mono struct {
	samples    []float32
	sampleRate int
	channels   int
	bitDepth   int
}

// Analyze decodes an audio file and computes its peaks and metadata. buckets is
// the peak resolution; pass 0 for the default.
func Analyze(path string, buckets int) (Result, error) {
	if buckets <= 0 {
		buckets = DefaultBuckets
	}
	m, err := decode(path)
	if err != nil {
		return Result{}, err
	}
	if m.sampleRate <= 0 {
		return Result{}, fmt.Errorf("audio: %s has no sample rate", filepath.Base(path))
	}

	info := Info{
		SampleRate: m.sampleRate,
		Channels:   m.channels,
		BitDepth:   m.bitDepth,
		DurationMS: int(int64(len(m.samples)) * 1000 / int64(m.sampleRate)),
		PeakDBFS:   peakDBFS(m.samples),
		IsLoopable: loopable(m.samples, m.sampleRate),
	}
	return Result{Info: info, Peaks: computePeaks(m.samples, buckets)}, nil
}

// decode dispatches on extension to a pure-Go decoder, returning a mono signal.
func decode(path string) (mono, error) {
	switch strings.ToLower(strings.TrimPrefix(filepath.Ext(path), ".")) {
	case "wav":
		return decodeWAV(path)
	case "mp3":
		return decodeMP3(path)
	case "ogg", "oga":
		return decodeOGG(path)
	case "flac":
		return decodeFLAC(path)
	default:
		return mono{}, fmt.Errorf("%w: %s", ErrUnsupported, filepath.Base(path))
	}
}

func decodeWAV(path string) (mono, error) {
	f, err := os.Open(path)
	if err != nil {
		return mono{}, err
	}
	defer f.Close()

	d := wav.NewDecoder(f)
	buf, err := d.FullPCMBuffer()
	if err != nil {
		return mono{}, fmt.Errorf("decode wav: %w", err)
	}
	ch := int(d.NumChans)
	if ch < 1 {
		ch = 1
	}
	bits := int(d.BitDepth)
	// Normalise to [-1,1]. 8-bit PCM WAV is unsigned with a 128 centre; the rest
	// are signed two's complement.
	scale := float64(int64(1) << (bits - 1))
	unsigned := bits == 8

	frames := len(buf.Data) / ch
	out := make([]float32, frames)
	for i := 0; i < frames; i++ {
		var sum float64
		for c := 0; c < ch; c++ {
			v := float64(buf.Data[i*ch+c])
			if unsigned {
				v -= 128
			}
			sum += v / scale
		}
		out[i] = float32(sum / float64(ch))
	}
	return mono{samples: out, sampleRate: int(d.SampleRate), channels: ch, bitDepth: bits}, nil
}

func decodeMP3(path string) (mono, error) {
	f, err := os.Open(path)
	if err != nil {
		return mono{}, err
	}
	defer f.Close()

	d, err := mp3.NewDecoder(f)
	if err != nil {
		return mono{}, fmt.Errorf("decode mp3: %w", err)
	}
	// go-mp3 yields 16-bit little-endian stereo frames.
	raw, err := io.ReadAll(d)
	if err != nil {
		return mono{}, fmt.Errorf("read mp3: %w", err)
	}
	const bytesPerFrame = 4 // 2 channels * int16
	frames := len(raw) / bytesPerFrame
	out := make([]float32, frames)
	for i := 0; i < frames; i++ {
		l := int16(binary.LittleEndian.Uint16(raw[i*4:]))
		r := int16(binary.LittleEndian.Uint16(raw[i*4+2:]))
		out[i] = float32((float64(l) + float64(r)) / 2 / 32768)
	}
	return mono{samples: out, sampleRate: d.SampleRate(), channels: 2, bitDepth: 16}, nil
}

func decodeOGG(path string) (mono, error) {
	f, err := os.Open(path)
	if err != nil {
		return mono{}, err
	}
	defer f.Close()

	data, format, err := oggvorbis.ReadAll(f)
	if err != nil {
		return mono{}, fmt.Errorf("decode ogg: %w", err)
	}
	ch := format.Channels
	if ch < 1 {
		ch = 1
	}
	frames := len(data) / ch
	out := make([]float32, frames)
	for i := 0; i < frames; i++ {
		var sum float32
		for c := 0; c < ch; c++ {
			sum += data[i*ch+c]
		}
		out[i] = sum / float32(ch)
	}
	return mono{samples: out, sampleRate: format.SampleRate, channels: ch, bitDepth: 0}, nil
}

func decodeFLAC(path string) (mono, error) {
	f, err := os.Open(path)
	if err != nil {
		return mono{}, err
	}
	defer f.Close()

	stream, err := flac.New(f)
	if err != nil {
		return mono{}, fmt.Errorf("decode flac: %w", err)
	}
	bits := int(stream.Info.BitsPerSample)
	scale := float64(int64(1) << (bits - 1))
	ch := int(stream.Info.NChannels)
	if ch < 1 {
		ch = 1
	}

	var out []float32
	if stream.Info.NSamples > 0 {
		out = make([]float32, 0, stream.Info.NSamples)
	}
	for {
		frame, err := stream.ParseNext()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return mono{}, fmt.Errorf("read flac frame: %w", err)
		}
		n := 0
		if len(frame.Subframes) > 0 {
			n = len(frame.Subframes[0].Samples)
		}
		for i := 0; i < n; i++ {
			var sum float64
			for c := 0; c < len(frame.Subframes); c++ {
				sum += float64(frame.Subframes[c].Samples[i]) / scale
			}
			out = append(out, float32(sum/float64(len(frame.Subframes))))
		}
	}
	return mono{samples: out, sampleRate: int(stream.Info.SampleRate), channels: ch, bitDepth: bits}, nil
}

// computePeaks reduces the signal to exactly `buckets` min/max pairs (fewer only
// when the signal is shorter than the bucket count). Bucket boundaries are spread
// evenly by integer arithmetic so the waveform width is stable.
func computePeaks(samples []float32, buckets int) Peaks {
	n := len(samples)
	if n == 0 {
		return Peaks{Version: 1, Count: 0, Min: []float32{}, Max: []float32{}}
	}
	if buckets > n {
		buckets = n
	}
	p := Peaks{Version: 1, Count: buckets, Min: make([]float32, buckets), Max: make([]float32, buckets)}
	for b := 0; b < buckets; b++ {
		start := b * n / buckets
		end := (b + 1) * n / buckets
		if end <= start {
			end = start + 1
		}
		lo, hi := samples[start], samples[start]
		for _, v := range samples[start:end] {
			if v < lo {
				lo = v
			}
			if v > hi {
				hi = v
			}
		}
		p.Min[b], p.Max[b] = lo, hi
	}
	return p
}

// peakDBFS is the loudest absolute sample in dBFS, floored at -120 for silence.
func peakDBFS(samples []float32) float64 {
	var peak float64
	for _, v := range samples {
		a := math.Abs(float64(v))
		if a > peak {
			peak = a
		}
	}
	if peak <= 0 {
		return -120
	}
	db := 20 * math.Log10(peak)
	if db < -120 {
		db = -120
	}
	return db
}

// loopable is §6's "probable loop" guess (advisory, approximate by design): a
// sound loops cleanly when it is still sounding at both ends — so it is not a
// one-shot that decays to silence — and the seam between its last and first
// sample is small enough not to click.
func loopable(samples []float32, sampleRate int) bool {
	n := len(samples)
	if n < sampleRate/4 { // shorter than 250 ms: a one-shot, not a loop
		return false
	}
	window := sampleRate / 100 // 10 ms
	if window < 1 {
		window = 1
	}
	// Sustained at both ends? A decayed one-shot has a near-silent tail and fails
	// here, which is what separates it from a genuine loop.
	if rms(samples[:window]) < 0.02 || rms(samples[n-window:]) < 0.02 {
		return false
	}
	// A small wrap-around discontinuity means no audible click at the loop point.
	seam := math.Abs(float64(samples[0]) - float64(samples[n-1]))
	return seam < 0.1
}

func rms(samples []float32) float64 {
	if len(samples) == 0 {
		return 0
	}
	var sumSq float64
	for _, v := range samples {
		sumSq += float64(v) * float64(v)
	}
	return math.Sqrt(sumSq / float64(len(samples)))
}
