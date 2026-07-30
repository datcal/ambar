package palette

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"strings"
)

// Export formats (§8 "Copy and export"). Each is a real interchange format used in
// this workflow, not a demo: .gpl is what Aseprite imports, the PNG strip is what
// Lospec consumes, .gd and .tres paste straight into Godot.
const (
	FormatGPL      = "gpl"  // GIMP palette
	FormatTXT      = "txt"  // plain hex list, one per line
	FormatJSON     = "json" // swatches with counts and ratios retained
	FormatCSS      = "css"  // CSS custom properties
	FormatGDScript = "gd"   // GDScript const array of Color
	FormatTRES     = "tres" // Godot Gradient resource
	FormatPNG      = "png"  // 1px-per-swatch strip
)

// SupportedFormats lists every export format, for the UI and for validation.
var SupportedFormats = []string{
	FormatGPL, FormatPNG, FormatTXT, FormatJSON, FormatCSS, FormatGDScript, FormatTRES,
}

// ContentType is the MIME type an exported palette should be served with.
func ContentType(format string) string {
	switch format {
	case FormatPNG:
		return "image/png"
	case FormatJSON:
		return "application/json"
	default:
		// Everything else is text the browser should download, not render.
		return "text/plain; charset=utf-8"
	}
}

// Export renders a palette in the given format. name labels the palette inside
// formats that carry one (GIMP, GDScript). It returns ErrUnknownFormat for anything
// not in SupportedFormats.
func Export(p Palette, name, format string) ([]byte, error) {
	switch format {
	case FormatGPL:
		return exportGPL(p.Swatches, name), nil
	case FormatTXT:
		return exportTXT(p.Swatches), nil
	case FormatJSON:
		return exportJSON(p)
	case FormatCSS:
		return exportCSS(p.Swatches), nil
	case FormatGDScript:
		return exportGDScript(p.Swatches, name), nil
	case FormatTRES:
		return exportTRES(p.Swatches), nil
	case FormatPNG:
		return exportPNG(p.Swatches)
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownFormat, format)
	}
}

// ErrUnknownFormat is returned by Export for an unrecognised format.
var ErrUnknownFormat = fmt.Errorf("unknown palette export format")

func exportGPL(sw []Swatch, name string) []byte {
	var b strings.Builder
	b.WriteString("GIMP Palette\n")
	b.WriteString("Name: " + gplName(name) + "\n")
	b.WriteString("Columns: 0\n")
	b.WriteString("#\n")
	for _, s := range sw {
		// Three right-aligned 3-wide columns then the hex as the swatch name, which
		// is the format Aseprite and GIMP both parse.
		fmt.Fprintf(&b, "%3d %3d %3d\t%s\n", s.R, s.G, s.B, s.Hex)
	}
	return []byte(b.String())
}

// gplName keeps the palette name to one clean line; a filename with a newline must
// not break the header.
func gplName(name string) string {
	name = strings.ReplaceAll(name, "\n", " ")
	name = strings.ReplaceAll(name, "\r", " ")
	name = strings.TrimSpace(name)
	if name == "" {
		return "Ambar palette"
	}
	return name
}

func exportTXT(sw []Swatch) []byte {
	var b strings.Builder
	for _, s := range sw {
		b.WriteString(s.Hex)
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

func exportJSON(p Palette) ([]byte, error) {
	// A stable object shape: kind so a consumer knows whether the counts are exact,
	// and the swatches with counts and ratios retained (§8).
	out := struct {
		Kind   string   `json:"kind"`
		Colors []Swatch `json:"colors"`
	}{Kind: p.Kind, Colors: p.Swatches}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func exportCSS(sw []Swatch) []byte {
	var b strings.Builder
	b.WriteString(":root {\n")
	for i, s := range sw {
		fmt.Fprintf(&b, "  --color-%d: %s;\n", i+1, s.Hex)
	}
	b.WriteString("}\n")
	return []byte(b.String())
}

func exportGDScript(sw []Swatch, name string) []byte {
	var b strings.Builder
	b.WriteString("# " + gplName(name) + "\n")
	b.WriteString("const PALETTE: Array[Color] = [\n")
	for _, s := range sw {
		fmt.Fprintf(&b, "\t%s,\n", GDColor(s.R, s.G, s.B))
	}
	b.WriteString("]\n")
	return []byte(b.String())
}

// GDColor formats an 8-bit RGB triple as a GDScript Color literal with normalised
// floats, e.g. Color(0.545, 0.227, 0.227) (§8). Exported because the click-to-copy
// "Color(...)" format needs the identical rendering the server test pins down.
func GDColor(r, g, b int) string {
	return fmt.Sprintf("Color(%s, %s, %s)", norm(r), norm(g), norm(b))
}

// norm renders an 8-bit channel as a 0–1 float with three decimals, trailing zeros
// kept so 0.5 reads as 0.500 — consistent width, and it matches the spec example.
func norm(v int) string {
	return fmt.Sprintf("%.3f", float64(v)/255.0)
}

func exportTRES(sw []Swatch) []byte {
	var b strings.Builder
	b.WriteString(`[gd_resource type="Gradient" format=3]` + "\n\n")
	b.WriteString("[resource]\n")

	// Offsets spread evenly across 0..1; a single colour sits at 0.
	b.WriteString("offsets = PackedFloat32Array(")
	for i := range sw {
		if i > 0 {
			b.WriteString(", ")
		}
		var off float64
		if len(sw) > 1 {
			off = float64(i) / float64(len(sw)-1)
		}
		fmt.Fprintf(&b, "%g", off)
	}
	b.WriteString(")\n")

	b.WriteString("colors = PackedColorArray(")
	for i, s := range sw {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%g, %g, %g, 1", float64(s.R)/255.0, float64(s.G)/255.0, float64(s.B)/255.0)
	}
	b.WriteString(")\n")
	return []byte(b.String())
}

// exportPNG writes a 1px-tall strip, one pixel per swatch. This is the de facto
// pixel-art palette exchange format — what Aseprite and Lospec read (§8).
func exportPNG(sw []Swatch) ([]byte, error) {
	if len(sw) == 0 {
		// A 1x1 transparent pixel rather than a zero-dimension image, which some
		// decoders reject.
		img := image.NewNRGBA(image.Rect(0, 0, 1, 1))
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}
	img := image.NewNRGBA(image.Rect(0, 0, len(sw), 1))
	for i, s := range sw {
		img.Set(i, 0, color.NRGBA{R: uint8(s.R), G: uint8(s.G), B: uint8(s.B), A: 0xff})
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
