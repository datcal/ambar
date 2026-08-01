package index

import "testing"

// TestAnimatedSeparatesRealAnimationFromAGuessedGrid is M17's regression, and the
// numbers are why it exists: on the real library 6,706 assets satisfied the old
// `FrameCount > 1` and exactly 795 anim.gif files had been written. The other ~5,900
// were still images whose pixels divide into a grid — §6's spritesheet *detection*,
// which guessed that a 48x40 tileset was an animation of 1,920 frames. Each of those
// tiles offered a hover animation whose URL 404s, and an <img> whose src 404s goes
// blank. "Bozuk görünüyor", correctly.
func TestAnimatedSeparatesRealAnimationFromAGuessedGrid(t *testing.T) {
	tests := []struct {
		name        string
		asset       Asset
		animated    bool
		wantPreview string
	}{
		{
			name:        "a gif with real frames",
			asset:       Asset{ID: 1, FrameCount: 12, FrameSource: ""},
			animated:    true,
			wantPreview: "/assets/1/anim.gif",
		},
		{
			name:     "a tileset the detector divided into a grid",
			asset:    Asset{ID: 2, FrameCount: 1920, FrameSource: "detected"},
			animated: false,
			// Nothing: §6 says a guess is never trusted silently, and animating one
			// in the grid is trusting it silently.
			wantPreview: "",
		},
		{
			name:        "a grid a human confirmed",
			asset:       Asset{ID: 3, FrameCount: 8, FrameSource: "manual"},
			animated:    false,
			wantPreview: "/assets/3/sheet.gif",
		},
		{
			name:        "a grid that came from a sidecar",
			asset:       Asset{ID: 4, FrameCount: 8, FrameSource: "sidecar"},
			animated:    false,
			wantPreview: "/assets/4/sheet.gif",
		},
		{
			name:        "a still image",
			asset:       Asset{ID: 5, FrameCount: 1},
			animated:    false,
			wantPreview: "",
		},
		{
			name:        "a single-frame grid is not worth animating",
			asset:       Asset{ID: 6, FrameCount: 1, FrameSource: "manual"},
			animated:    false,
			wantPreview: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.asset.Animated(); got != tc.animated {
				t.Errorf("Animated() = %v, want %v", got, tc.animated)
			}
			if got := tc.asset.AnimatedPreview(); got != tc.wantPreview {
				t.Errorf("AnimatedPreview() = %q, want %q", got, tc.wantPreview)
			}
		})
	}
}
