package palette

import (
	"bytes"
	"encoding/json"
	"image/png"
	"strings"
	"testing"
)

func samplePalette() Palette {
	return Palette{
		Kind: KindExact,
		Swatches: []Swatch{
			{Hex: "#8b3a3a", R: 0x8b, G: 0x3a, B: 0x3a, Count: 10, Ratio: 0.5},
			{Hex: "#000000", R: 0, G: 0, B: 0, Count: 6, Ratio: 0.3},
			{Hex: "#ffffff", R: 255, G: 255, B: 255, Count: 4, Ratio: 0.2},
		},
		Visible: 20,
	}
}

func TestGDColorMatchesSpecExample(t *testing.T) {
	// The spec pins this exact rendering: Color(0.545, 0.227, 0.227) for #8b3a3a.
	if got := GDColor(0x8b, 0x3a, 0x3a); got != "Color(0.545, 0.227, 0.227)" {
		t.Errorf("GDColor = %q, want Color(0.545, 0.227, 0.227)", got)
	}
}

func TestExportGPL(t *testing.T) {
	out, err := Export(samplePalette(), "My Pack", FormatGPL)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.HasPrefix(s, "GIMP Palette\n") {
		t.Errorf("missing GIMP Palette header:\n%s", s)
	}
	if !strings.Contains(s, "Name: My Pack\n") {
		t.Errorf("missing name line:\n%s", s)
	}
	if !strings.Contains(s, "139  58  58\t#8b3a3a") {
		t.Errorf("missing red swatch line:\n%s", s)
	}
}

func TestExportTXT(t *testing.T) {
	out, err := Export(samplePalette(), "", FormatTXT)
	if err != nil {
		t.Fatal(err)
	}
	want := "#8b3a3a\n#000000\n#ffffff\n"
	if string(out) != want {
		t.Errorf("TXT = %q, want %q", out, want)
	}
}

func TestExportJSONRoundTrips(t *testing.T) {
	out, err := Export(samplePalette(), "", FormatJSON)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Kind   string   `json:"kind"`
		Colors []Swatch `json:"colors"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Kind != KindExact || len(got.Colors) != 3 {
		t.Fatalf("got kind=%q colors=%d", got.Kind, len(got.Colors))
	}
	if got.Colors[0].Count != 10 || got.Colors[0].Ratio != 0.5 {
		t.Errorf("counts/ratios not retained: %+v", got.Colors[0])
	}
}

func TestExportCSS(t *testing.T) {
	out, err := Export(samplePalette(), "", FormatCSS)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, "--color-1: #8b3a3a;") || !strings.Contains(s, ":root {") {
		t.Errorf("unexpected CSS:\n%s", s)
	}
}

func TestExportGDScript(t *testing.T) {
	out, err := Export(samplePalette(), "Pack", FormatGDScript)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, "const PALETTE: Array[Color] = [") {
		t.Errorf("missing const declaration:\n%s", s)
	}
	if !strings.Contains(s, "Color(0.545, 0.227, 0.227),") {
		t.Errorf("missing normalised Color literal:\n%s", s)
	}
}

func TestExportTRES(t *testing.T) {
	out, err := Export(samplePalette(), "", FormatTRES)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, `[gd_resource type="Gradient" format=3]`) {
		t.Errorf("missing gd_resource header:\n%s", s)
	}
	if !strings.Contains(s, "offsets = PackedFloat32Array(0, 0.5, 1)") {
		t.Errorf("unexpected offsets:\n%s", s)
	}
	if !strings.Contains(s, "colors = PackedColorArray(") {
		t.Errorf("missing colors array:\n%s", s)
	}
}

func TestExportPNGStrip(t *testing.T) {
	out, err := Export(samplePalette(), "", FormatPNG)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("decode PNG: %v", err)
	}
	b := img.Bounds()
	if b.Dx() != 3 || b.Dy() != 1 {
		t.Fatalf("strip is %dx%d, want 3x1", b.Dx(), b.Dy())
	}
	r, g, bl, _ := img.At(0, 0).RGBA()
	if r>>8 != 0x8b || g>>8 != 0x3a || bl>>8 != 0x3a {
		t.Errorf("first pixel = %d,%d,%d, want 139,58,58", r>>8, g>>8, bl>>8)
	}
}

func TestExportUnknownFormat(t *testing.T) {
	if _, err := Export(samplePalette(), "", "xcf"); err == nil {
		t.Error("expected an error for an unknown format")
	}
}
