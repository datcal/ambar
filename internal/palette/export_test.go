package palette

import (
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

// The four formats these tests used to cover — .txt, .json, .css and .png — were removed with
// their links in M16, so an old URL for one now takes the ErrUnknownFormat path below. That is
// the behaviour worth asserting; the exporters themselves are gone.
func TestExportUnknownFormat(t *testing.T) {
	if _, err := Export(samplePalette(), "", "xcf"); err == nil {
		t.Error("expected an error for an unknown format")
	}
}
