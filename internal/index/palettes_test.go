package index

import "testing"

// TestSpreadAcrossHuesBreaksTheShadowMonopoly is M17's colour-filter regression.
//
// Ranking the sidebar's colours by coverage put twelve near-blacks and dark browns at the
// top, because that is what pixel art is mostly made of: outlines and shadow. Measured on
// the real library, the leading swatch #0d0d13 appeared in 38% of all images — a filter
// chip that returns a third of the library is not a filter, and the row could not offer a
// green at all even though the library has 24 distinct green buckets.
func TestSpreadAcrossHuesBreaksTheShadowMonopoly(t *testing.T) {
	// Candidates as the query returns them: strongest first, and the strong end is all
	// shadow. The greens and blues are real but weak, exactly as they are in the library.
	candidates := []PackColour{
		{R: 0x0d, G: 0x0d, B: 0x13, Weight: 552, Assets: 2299},
		{R: 0x23, G: 0x1f, B: 0x23, Weight: 191, Assets: 1525},
		{R: 0x3b, G: 0x39, B: 0x3e, Weight: 143, Assets: 1184},
		{R: 0x53, G: 0x3c, B: 0x3c, Weight: 113, Assets: 1231},
		{R: 0x54, G: 0x2a, B: 0x23, Weight: 174, Assets: 1378},
		{R: 0x39, G: 0x23, B: 0x21, Weight: 130, Assets: 1223},
		{R: 0xeb, G: 0x96, B: 0x61, Weight: 163, Assets: 268},
		{R: 0x1f, G: 0x36, B: 0x27, Weight: 49, Assets: 406},
		{R: 0x27, G: 0x69, B: 0x42, Weight: 29, Assets: 252},
		{R: 0x3a, G: 0x58, B: 0x80, Weight: 20, Assets: 266},
		{R: 0xac, G: 0x1c, B: 0xe4, Weight: 4, Assets: 32},
	}

	got := spreadAcrossHues(candidates, 6)
	if len(got) != 6 {
		t.Fatalf("got %d colours, want 6", len(got))
	}

	// Breadth first: no family may take a second slot while another still has a first.
	seen := map[int]int{}
	for _, c := range got {
		seen[familyOf(c)]++
	}
	if seen[neutralFamily] > 1 {
		t.Errorf("the neutrals took %d of 6 slots; one per round is the whole point", seen[neutralFamily])
	}
	if len(seen) < 5 {
		t.Errorf("only %d hue families represented in 6 slots: %v", len(seen), seen)
	}

	// A green and a blue have to survive, because that is the complaint.
	var green, blue bool
	for _, c := range got {
		if c.G > c.R && c.G > c.B {
			green = true
		}
		if c.B > c.R && c.B > c.G {
			blue = true
		}
	}
	if !green || !blue {
		t.Errorf("green=%v blue=%v; the filter must be able to offer both", green, blue)
	}

	// And the strip is ordered as a spectrum, neutrals last, so it reads as a palette
	// rather than a ranking.
	last := -1
	for _, c := range got {
		f := familyOf(c)
		if f < last {
			t.Errorf("families out of order: %v after %v", f, last)
		}
		last = f
	}
	if familyOf(got[len(got)-1]) != neutralFamily {
		t.Error("the neutrals belong at the end of the strip")
	}
}

// TestSpreadAcrossHuesKeepsEverythingWhenItFits: with fewer candidates than slots the
// selection is a no-op apart from the ordering, so a small library still shows all of it.
func TestSpreadAcrossHuesKeepsEverythingWhenItFits(t *testing.T) {
	candidates := []PackColour{
		{R: 200, G: 30, B: 30, Weight: 3},
		{R: 30, G: 200, B: 30, Weight: 2},
		{R: 10, G: 10, B: 10, Weight: 1},
	}
	got := spreadAcrossHues(candidates, 40)
	if len(got) != 3 {
		t.Fatalf("got %d, want all 3", len(got))
	}
	if familyOf(got[2]) != neutralFamily {
		t.Error("still ordered: the near-black goes last")
	}
}

// TestHSL pins the conversion the families depend on.
func TestHSL(t *testing.T) {
	tests := []struct {
		name            string
		r, g, b         int
		wantH           float64
		wantSAbove      float64
		wantLBelowAbove [2]float64
	}{
		{"pure red", 255, 0, 0, 0, 0.9, [2]float64{0.4, 0.6}},
		{"pure green", 0, 255, 0, 1.0 / 3, 0.9, [2]float64{0.4, 0.6}},
		{"pure blue", 0, 0, 255, 2.0 / 3, 0.9, [2]float64{0.4, 0.6}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, s, l := hsl(tc.r, tc.g, tc.b)
			if diff := h - tc.wantH; diff > 0.01 || diff < -0.01 {
				t.Errorf("hue = %.3f, want %.3f", h, tc.wantH)
			}
			if s < tc.wantSAbove {
				t.Errorf("saturation = %.2f, want above %.2f", s, tc.wantSAbove)
			}
			if l < tc.wantLBelowAbove[0] || l > tc.wantLBelowAbove[1] {
				t.Errorf("lightness = %.2f, want between %v", l, tc.wantLBelowAbove)
			}
		})
	}

	// Grey has no hue, and must land in the neutrals rather than in "red".
	if _, s, _ := hsl(128, 128, 128); s != 0 {
		t.Errorf("grey saturation = %.2f, want 0", s)
	}
	if familyOf(PackColour{R: 128, G: 128, B: 128}) != neutralFamily {
		t.Error("mid grey is a neutral")
	}
	if familyOf(PackColour{R: 0x0d, G: 0x0d, B: 0x13}) != neutralFamily {
		t.Error("#0d0d13 is black, not a dark blue")
	}
}
