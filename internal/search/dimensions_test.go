package search

import "testing"

// A bare "32x32" is a pixel size (M16).
//
// "Which of these are 32 by 32" is the most common question in a sprite library, and
// answering it used to require `width:32 height:32`. A bare NxM is not plausible as a
// filename search, so the shortcut costs nothing.
func TestBareDimensionsBecomeASizeFilter(t *testing.T) {
	cases := []struct {
		in   string
		w, h int
		ok   bool
	}{
		{"32x32", 32, 32, true},
		{"16X24", 16, 24, true},
		{"64×64", 64, 64, true}, // the multiplication sign, which is what our own UI prints
		{"dim:8x8", 8, 8, true},
		{"px:128x64", 128, 64, true},
		{"0x0", 0, 0, false},      // matches nothing; must not become a filter
		{"x32", 0, 0, false},      // not a size
		{"sword", 0, 0, false},    // an ordinary word
		{"32x32x32", 0, 0, false}, // not a size either
	}

	for _, tc := range cases {
		q, err := Parse(tc.in)
		if err != nil {
			t.Fatalf("Parse(%q): %v", tc.in, err)
		}
		var found *DimensionsTerm
		for _, g := range q.Groups {
			for _, term := range g.Terms {
				if d, isDim := term.(DimensionsTerm); isDim {
					copy := d
					found = &copy
				}
			}
		}
		if !tc.ok {
			if found != nil {
				t.Errorf("Parse(%q) produced a size filter %dx%d; it should not", tc.in, found.W, found.H)
			}
			continue
		}
		if found == nil {
			t.Errorf("Parse(%q) produced no size filter", tc.in)
			continue
		}
		if found.W != tc.w || found.H != tc.h {
			t.Errorf("Parse(%q) = %dx%d, want %dx%d", tc.in, found.W, found.H, tc.w, tc.h)
		}
	}
}

// The 3D and audio fields used to parse and then filter nothing, so `tris:<5000` returned the
// whole library — a wrong answer presented as a right one.
func TestModelAndAudioFieldsAreRealNow(t *testing.T) {
	for _, field := range []string{"tris", "verts", "materials", "duration"} {
		if _, isFuture := futureFields[field]; isFuture {
			t.Errorf("%s is still a no-op field", field)
		}
		if _, mapped := numericFields[field]; !mapped {
			t.Errorf("%s maps to no column", field)
		}
	}
}
