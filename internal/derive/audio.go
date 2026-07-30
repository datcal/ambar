package derive

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/datcal/ambar/internal/audio"
)

// FilePeaks is the waveform-peaks derivative within a content-hash directory (§3).
const FilePeaks = "peaks.json"

// audioExts are the §6 audio formats the deriver analyses. A file outside this
// set falls through to the image path, which reports it unsupported.
var audioExts = map[string]bool{
	"wav": true, "mp3": true, "ogg": true, "oga": true, "flac": true,
}

// isAudioExt reports whether Generate should take the audio path.
func isAudioExt(ext string) bool { return audioExts[ext] }

// deriveAudio is Generate's audio branch: analyse the sound, write peaks.json,
// and return the metadata for the §4 audio columns. It renders no image.
func deriveAudio(opts GenerateOptions) (*Result, error) {
	res, err := audio.Analyze(opts.AbsPath, audio.DefaultBuckets)
	if err != nil {
		return nil, err // wraps audio.ErrUnsupported for a codec we cannot read
	}

	relDir, err := Dir(opts.SHA256)
	if err != nil {
		return nil, err
	}
	outDir := filepath.Join(opts.DataRoot, relDir)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, fmt.Errorf("create derivative directory: %w", err)
	}

	data, err := json.Marshal(res.Peaks)
	if err != nil {
		return nil, fmt.Errorf("marshal peaks: %w", err)
	}
	if err := os.WriteFile(filepath.Join(outDir, FilePeaks), data, 0o644); err != nil {
		return nil, fmt.Errorf("write peaks: %w", err)
	}

	// A grid tile (M15). Audio used to show as a bare extension chip, which makes two
	// hundred .wav files indistinguishable; the waveform is the shape §8 already draws
	// on the detail page, rendered once here from the peaks this pass just computed —
	// so it costs no extra decode.
	files := []string{FilePeaks}
	if err := writeWaveformThumb(filepath.Join(outDir, FileThumb), res.Peaks); err != nil {
		// Not fatal: the sound is analysed and playable, and a missing tile is a
		// cosmetic loss. Recorded as a note so it is visible rather than silent.
		return &Result{
			Audio: &res.Info,
			Files: files,
			Notes: []string{"waveform tile could not be written: " + err.Error()},
		}, nil
	}
	files = append(files, FileThumb)

	info := res.Info
	return &Result{
		Audio: &info,
		Files: files,
	}, nil
}
