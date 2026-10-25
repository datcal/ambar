package derive

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// TestEveryGeneratedFileIsAccepted keeps the remote allow-list honest. A server takes
// only the derivative names it knows from a worker, so a new file Generate starts
// writing must be added there too — or every remote result carrying it is refused, which
// is exactly how thumb@2x.webp was found missing.
func TestEveryGeneratedFileIsAccepted(t *testing.T) {
	src := t.TempDir()

	still := image.NewRGBA(image.Rect(0, 0, 40, 24))
	for y := 0; y < 24; y++ {
		for x := 0; x < 40; x++ {
			still.Set(x, y, color.RGBA{R: uint8(x * 6), G: uint8(y * 10), B: 0x40, A: 0xff})
		}
	}
	pngPath := writePNG(t, src, "still.png", still)

	// Two frames, so the animated path writes anim.gif as well.
	pal := color.Palette{color.Black, color.White}
	frames := &gif.GIF{}
	for i := 0; i < 2; i++ {
		f := image.NewPaletted(image.Rect(0, 0, 16, 16), pal)
		f.SetColorIndex(i, i, 1)
		frames.Image = append(frames.Image, f)
		frames.Delay = append(frames.Delay, 10)
	}
	var gifBuf bytes.Buffer
	if err := gif.EncodeAll(&gifBuf, frames); err != nil {
		t.Fatal(err)
	}
	gifPath := filepath.Join(src, "anim.gif")
	if err := os.WriteFile(gifPath, gifBuf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	var pcm bytes.Buffer
	for i := 0; i < 4410; i++ {
		v := int16(8000 * math.Sin(2*math.Pi*440*float64(i)/44100))
		pcm.WriteByte(byte(v))
		pcm.WriteByte(byte(v >> 8))
	}
	var wav bytes.Buffer
	wav.WriteString("RIFF")
	writeU32(&wav, uint32(36+pcm.Len()))
	wav.WriteString("WAVEfmt ")
	writeU32(&wav, 16)
	writeU16(&wav, 1)
	writeU16(&wav, 1)
	writeU32(&wav, 44100)
	writeU32(&wav, 44100*2)
	writeU16(&wav, 2)
	writeU16(&wav, 16)
	wav.WriteString("data")
	writeU32(&wav, uint32(pcm.Len()))
	wav.Write(pcm.Bytes())
	wavPath := filepath.Join(src, "beep.wav")
	if err := os.WriteFile(wavPath, wav.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, in := range []struct{ path, ext string }{
		{pngPath, "png"}, {gifPath, "gif"}, {wavPath, "wav"},
	} {
		raw, err := os.ReadFile(in.path)
		if err != nil {
			t.Fatal(err)
		}
		sha := ContentHash(raw)
		dataRoot := t.TempDir()
		res, err := Generate(GenerateOptions{AbsPath: in.path, Ext: in.ext, SHA256: sha, DataRoot: dataRoot})
		if err != nil {
			t.Fatalf("%s: %v", in.ext, err)
		}
		if len(res.Files) == 0 {
			t.Fatalf("%s: Generate listed no files", in.ext)
		}
		relDir, _ := Dir(sha)
		for _, name := range res.Files {
			if err := checkDerivative(filepath.Join(dataRoot, relDir), name); err != nil {
				t.Errorf("%s: a worker's %s would be refused: %v", in.ext, name, err)
			}
		}
	}
}

// TestCheckDerivativeRefuses: a name not on the list, a file the result names but did
// not send, and content that is not what its name says.
func TestCheckDerivativeRefuses(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, content []byte) {
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var pngBuf bytes.Buffer
	writePNG(t, dir, "real.png", image.NewRGBA(image.Rect(0, 0, 2, 2)))
	raw, _ := os.ReadFile(filepath.Join(dir, "real.png"))
	pngBuf.Write(raw)

	write("evil.sh", []byte("#!/bin/sh"))
	write(FileThumb, pngBuf.Bytes()) // a PNG wearing a .webp name
	write(FilePeaks, []byte("{not json"))
	write(FileModelPreview, []byte("PK\x03\x04"))

	for _, name := range []string{"evil.sh", FileThumb, FilePeaks, FileModelPreview, FilePreview} {
		if err := checkDerivative(dir, name); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// TestOutcomeOf: the one mapping from Generate's errors to derive states, shared by the
// local and the remote path.
func TestOutcomeOf(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{nil, StateOK},
		{ErrNeedsBlender, StateNeedsBlender},
		{ErrUnsupported, StateUnsupported},
		{errors.New("corrupt"), StateFailed},
	}
	for _, c := range cases {
		var res *Result
		if c.err == nil {
			res = &Result{Notes: []string{"a", "b"}}
		}
		got := OutcomeOf(res, c.err)
		if got.State != c.want {
			t.Errorf("OutcomeOf(%v) = %s, want %s", c.err, got.State, c.want)
		}
	}
	if got := OutcomeOf(&Result{Notes: []string{"a", "b"}}, nil); got.Message != "a; b" || got.Result == nil {
		t.Errorf("a success keeps its notes and result: %+v", got)
	}
	if got := OutcomeOf(nil, nil); got.State != StateFailed {
		t.Errorf("no result and no error = %s, want failed", got.State)
	}
}
