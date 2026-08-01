package search

import (
	"reflect"
	"testing"
	"time"
)

// unix is a helper for building expected date values.
func unix(t *testing.T, layout, s string) int64 {
	t.Helper()
	tm, err := time.ParseInLocation(layout, s, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	return tm.Unix()
}

func TestParseTerms(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		terms []Term // expected terms in a single group
	}{
		{"bare word lowercased", "Sword", []Term{WordTerm{base{false}, "sword"}}},
		{"negated word", "-sword", []Term{WordTerm{base{true}, "sword"}}},
		{"quoted phrase", `"laser turret"`, []Term{PhraseTerm{base{false}, "laser turret"}}},
		{"tag", "theme:sci-fi", []Term{TagTerm{base{false}, "theme:sci-fi"}}},
		{"hierarchy tag", "type:sfx:impact", []Term{TagTerm{base{false}, "type:sfx:impact"}}},
		{"negated tag", "-style:realistic", []Term{TagTerm{base{true}, "style:realistic"}}},
		{"kind via type", "type:model", []Term{KindTerm{base{false}, "model"}}},
		{"kind explicit", "kind:audio", []Term{KindTerm{base{false}, "audio"}}},
		{"has alpha", "has:alpha", []Term{HasTerm{base{false}, "alpha"}}},
		{"has animation", "has:animation", []Term{HasTerm{base{false}, "animation"}}},
		{"style pixel-art", "style:pixel-art", []Term{StyleTerm{base{false}, "pixel-art"}}},
		{"numeric lt", "width:<64", []Term{FieldTerm{base: base{false}, Field: "width", Op: "<", Num: 64}}},
		{"numeric gte", "height:>=128", []Term{FieldTerm{base: base{false}, Field: "height", Op: ">=", Num: 128}}},
		{"numeric eq default", "colors:16", []Term{FieldTerm{base: base{false}, Field: "colors", Op: "=", Num: 16}}},
		{"two terms implicit and", "cc0 sword", []Term{
			WordTerm{base{false}, "cc0"}, WordTerm{base{false}, "sword"},
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q, err := Parse(tc.in)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.in, err)
			}
			if len(q.Groups) != 1 {
				t.Fatalf("Parse(%q) groups = %d, want 1", tc.in, len(q.Groups))
			}
			if !reflect.DeepEqual(q.Groups[0].Terms, tc.terms) {
				t.Errorf("Parse(%q) terms =\n  %#v\nwant\n  %#v", tc.in, q.Groups[0].Terms, tc.terms)
			}
		})
	}
}

func TestParseDates(t *testing.T) {
	q, _ := Parse("added:>2026-01")
	want := FieldTerm{base: base{false}, Field: "added", Op: ">", IsDate: true, Date: unix(t, "2006-01", "2026-01")}
	if !reflect.DeepEqual(q.Groups[0].Terms[0], want) {
		t.Errorf("date term = %#v, want %#v", q.Groups[0].Terms[0], want)
	}
}

func TestParseOrGroups(t *testing.T) {
	q, _ := Parse("type:model OR type:audio")
	if len(q.Groups) != 2 {
		t.Fatalf("groups = %d, want 2", len(q.Groups))
	}
	if k, ok := q.Groups[0].Terms[0].(KindTerm); !ok || k.Kind != "model" {
		t.Errorf("group 0 = %#v", q.Groups[0].Terms[0])
	}
	if k, ok := q.Groups[1].Terms[0].(KindTerm); !ok || k.Kind != "audio" {
		t.Errorf("group 1 = %#v", q.Groups[1].Terms[0])
	}
}

func TestParsePipeIsOr(t *testing.T) {
	q, _ := Parse("a | b")
	if len(q.Groups) != 2 {
		t.Fatalf("groups = %d, want 2", len(q.Groups))
	}
}

func TestParseWarnings(t *testing.T) {
	tests := []struct {
		in      string
		wantLen int
	}{
		// color: and palette-near: used to warn and do nothing; they are real filters
		// now, so what warns is a malformed one.
		{"color:notacolour", 1},
		{"color:#8b3a3a~wide", 1},
		{"palette-near:notanid", 1},
		// tris: and duration: used to live here for the same reason color: did — they
		// parsed and filtered nothing. M16 connected them to the columns M5 and M6 added,
		// so a malformed one is what warns now.
		{"tris:manythousands", 1},
		{"duration:soon", 1},
		{"dim:32", 1},
		{"width:notanumber", 1},
		{"has:teleport", 1},
	}
	for _, tc := range tests {
		q, _ := Parse(tc.in)
		if len(q.Warnings) != tc.wantLen {
			t.Errorf("Parse(%q) warnings = %v, want %d", tc.in, q.Warnings, tc.wantLen)
		}
		if !q.Empty() {
			t.Errorf("Parse(%q) should be empty (all terms unactionable), got %#v", tc.in, q.Groups)
		}
	}
}

func TestParseQuotedNeverAlias(t *testing.T) {
	// A quoted OR/color is a phrase, not an operator or a filter.
	q, _ := Parse(`"OR" "color:red"`)
	if len(q.Groups) != 1 || len(q.Groups[0].Terms) != 2 {
		t.Fatalf("quoted operators leaked: %#v", q.Groups)
	}
	if p, ok := q.Groups[0].Terms[0].(PhraseTerm); !ok || p.Phrase != "OR" {
		t.Errorf("term 0 = %#v", q.Groups[0].Terms[0])
	}
}

func TestParseEmpty(t *testing.T) {
	for _, in := range []string{"", "   ", `""`} {
		q, _ := Parse(in)
		if !q.Empty() {
			t.Errorf("Parse(%q) not empty: %#v", in, q.Groups)
		}
	}
}

// TestParseColourTerms covers §7's two colour filters, including the tolerance
// suffix and the forms a person actually types.
func TestParseColourTerms(t *testing.T) {
	tests := []struct {
		in            string
		wantR         int
		wantG         int
		wantB         int
		wantTolerance int
	}{
		{"color:#8b3a3a", 0x8b, 0x3a, 0x3a, DefaultColorTolerance},
		{"color:8b3a3a", 0x8b, 0x3a, 0x3a, DefaultColorTolerance},
		{"colour:#8b3a3a", 0x8b, 0x3a, 0x3a, DefaultColorTolerance},
		{"color:#f0a", 0xff, 0x00, 0xaa, DefaultColorTolerance},
		{"color:#8b3a3a~0", 0x8b, 0x3a, 0x3a, 0},
		{"color:#8b3a3a~40", 0x8b, 0x3a, 0x3a, 40},
		// A tolerance past the end of the range matches everything either way.
		{"color:#8b3a3a~9000", 0x8b, 0x3a, 0x3a, 255},
	}
	for _, tc := range tests {
		q, err := Parse(tc.in)
		if err != nil {
			t.Fatalf("Parse(%q): %v", tc.in, err)
		}
		if len(q.Groups) != 1 || len(q.Groups[0].Terms) != 1 {
			t.Fatalf("Parse(%q) = %#v, want one term (warnings %v)", tc.in, q.Groups, q.Warnings)
		}
		got, ok := q.Groups[0].Terms[0].(ColorTerm)
		if !ok {
			t.Fatalf("Parse(%q) term = %#v, want a ColorTerm", tc.in, q.Groups[0].Terms[0])
		}
		if got.R != tc.wantR || got.G != tc.wantG || got.B != tc.wantB || got.Tolerance != tc.wantTolerance {
			t.Errorf("Parse(%q) = %d,%d,%d ~%d; want %d,%d,%d ~%d", tc.in,
				got.R, got.G, got.B, got.Tolerance, tc.wantR, tc.wantG, tc.wantB, tc.wantTolerance)
		}
	}

	// Negation applies to the whole filter.
	q, _ := Parse("-color:#8b3a3a")
	if term, ok := q.Groups[0].Terms[0].(ColorTerm); !ok || !term.negated() {
		t.Errorf("-color: did not parse as a negated ColorTerm: %#v", q.Groups[0].Terms[0])
	}

	// palette-near takes an asset id.
	q, _ = Parse("palette-near:42~30")
	near, ok := q.Groups[0].Terms[0].(PaletteNearTerm)
	if !ok {
		t.Fatalf("palette-near term = %#v", q.Groups[0].Terms[0])
	}
	if near.AssetID != 42 || near.Tolerance != 30 {
		t.Errorf("palette-near:42~30 = id %d ~%d", near.AssetID, near.Tolerance)
	}
	if q, _ := Parse("palette-near:0"); len(q.Warnings) != 1 {
		t.Errorf("palette-near:0 should warn, got %v", q.Warnings)
	}
}
