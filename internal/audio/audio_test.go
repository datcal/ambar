package audio

import (
	"bytes"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// writeWAV writes a 16-bit PCM mono WAV of the given samples ([-1,1]).
func writeWAV(t *testing.T, path string, samples []float64, sampleRate int) {
	t.Helper()
	var body bytes.Buffer
	for _, s := range samples {
		binary.Write(&body, binary.LittleEndian, int16(s*32767))
	}
	data := body.Bytes()

	var buf bytes.Buffer
	buf.WriteString("RIFF")
	binary.Write(&buf, binary.LittleEndian, uint32(36+len(data)))
	buf.WriteString("WAVE")
	buf.WriteString("fmt ")
	binary.Write(&buf, binary.LittleEndian, uint32(16))
	binary.Write(&buf, binary.LittleEndian, uint16(1)) // PCM
	binary.Write(&buf, binary.LittleEndian, uint16(1)) // mono
	binary.Write(&buf, binary.LittleEndian, uint32(sampleRate))
	binary.Write(&buf, binary.LittleEndian, uint32(sampleRate*2)) // byte rate
	binary.Write(&buf, binary.LittleEndian, uint16(2))            // block align
	binary.Write(&buf, binary.LittleEndian, uint16(16))           // bits
	buf.WriteString("data")
	binary.Write(&buf, binary.LittleEndian, uint32(len(data)))
	buf.Write(data)

	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func sine(freq float64, seconds float64, amp float64, sampleRate int) []float64 {
	n := int(seconds * float64(sampleRate))
	out := make([]float64, n)
	for i := range out {
		out[i] = amp * math.Sin(2*math.Pi*freq*float64(i)/float64(sampleRate))
	}
	return out
}

func TestAnalyzeSineWAV(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tone.wav")
	// 440 Hz for exactly 1 second at 44100 Hz: an integer number of cycles, so it
	// begins and ends at zero — a clean loop.
	writeWAV(t, path, sine(440, 1.0, 0.5, 44100), 44100)

	res, err := Analyze(path, 0)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	i := res.Info
	if i.SampleRate != 44100 || i.Channels != 1 || i.BitDepth != 16 {
		t.Errorf("format wrong: %+v", i)
	}
	if i.DurationMS < 990 || i.DurationMS > 1010 {
		t.Errorf("duration = %d ms, want ~1000", i.DurationMS)
	}
	// Amplitude 0.5 → about -6 dBFS.
	if i.PeakDBFS < -7 || i.PeakDBFS > -5 {
		t.Errorf("peak dBFS = %.2f, want ~-6", i.PeakDBFS)
	}
	if !i.IsLoopable {
		t.Errorf("a full-cycle sine should be loopable")
	}
	if res.Peaks.Count != DefaultBuckets {
		t.Errorf("peaks count = %d, want %d", res.Peaks.Count, DefaultBuckets)
	}
	// The envelope should reach near the amplitude somewhere.
	var maxPeak float32
	for _, v := range res.Peaks.Max {
		if v > maxPeak {
			maxPeak = v
		}
	}
	if maxPeak < 0.4 {
		t.Errorf("peak envelope max = %.3f, want ~0.5", maxPeak)
	}
}

func TestAnalyzeSilenceNotLoopable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "silence.wav")
	writeWAV(t, path, make([]float64, 44100), 44100)

	res, err := Analyze(path, 0)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if res.Info.IsLoopable {
		t.Errorf("silence must not be loopable")
	}
	if res.Info.PeakDBFS != -120 {
		t.Errorf("silence peak = %.1f, want -120", res.Info.PeakDBFS)
	}
}

func TestAnalyzeShortIsOneShot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blip.wav")
	writeWAV(t, path, sine(440, 0.1, 0.5, 44100), 44100) // 100 ms
	res, _ := Analyze(path, 0)
	if res.Info.IsLoopable {
		t.Errorf("a 100 ms blip should be a one-shot, not loopable")
	}
}

func TestUnsupportedFormat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.aiff")
	os.WriteFile(path, []byte("nope"), 0o644)
	if _, err := Analyze(path, 0); err == nil {
		t.Error("expected ErrUnsupported")
	}
}

func TestPeaksBucketsBoundedForShortInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tiny.wav")
	writeWAV(t, path, sine(440, 0.001, 0.5, 44100), 44100) // ~44 samples
	res, err := Analyze(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Peaks.Count > 44 {
		t.Errorf("more buckets than samples: %d", res.Peaks.Count)
	}
}
