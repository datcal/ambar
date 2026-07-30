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

	info := res.Info
	return &Result{
		Audio: &info,
		Files: []string{FilePeaks},
	}, nil
}
