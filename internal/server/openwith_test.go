package server

import (
	"strings"
	"testing"

	"github.com/datcal/ambar/internal/config"
)

// TestLocalPathFor covers the M14 mapping from a library-relative path to the path
// the operator's own machine understands. The template's shape decides the
// separator, because asking the browser what OS it runs on is both unreliable and
// unnecessary.
func TestLocalPathFor(t *testing.T) {
	tests := []struct {
		name     string
		template string
		rel      string
		wantPath string
		wantURL  string
		windows  bool
	}{
		{
			name:     "smb url",
			template: "smb://nas.local/game/assets",
			rel:      "2d/pack/hero sprite.png",
			wantPath: "smb://nas.local/game/assets/2d/pack/hero sprite.png",
			wantURL:  "smb://nas.local/game/assets/2d/pack/hero%20sprite.png",
		},
		{
			name:     "unc share",
			template: `\\nas\game\assets`,
			rel:      "2d/pack/hero.png",
			wantPath: `\\nas\game\assets\2d\pack\hero.png`,
			windows:  true,
		},
		{
			name:     "windows drive",
			template: `Z:\game\assets`,
			rel:      "2d/hero.png",
			wantPath: `Z:\game\assets\2d\hero.png`,
			windows:  true,
		},
		{
			name:     "unix mount point",
			template: "/Volumes/game/assets",
			rel:      "2d/hero.png",
			wantPath: "/Volumes/game/assets/2d/hero.png",
			wantURL:  "file:///Volumes/game/assets/2d/hero.png",
		},
		{
			name:     "trailing separator is not doubled",
			template: "smb://nas/assets/",
			rel:      "hero.png",
			wantPath: "smb://nas/assets/hero.png",
			wantURL:  "smb://nas/assets/hero.png",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := localPathFor(tc.template, tc.rel)
			if !ok {
				t.Fatal("no path produced")
			}
			if got.Path != tc.wantPath {
				t.Errorf("path = %q, want %q", got.Path, tc.wantPath)
			}
			if tc.wantURL != "" && got.URL != tc.wantURL {
				t.Errorf("url = %q, want %q", got.URL, tc.wantURL)
			}
			if got.Windows != tc.windows {
				t.Errorf("windows = %v, want %v", got.Windows, tc.windows)
			}
		})
	}

	// Unconfigured means the feature is absent, not a broken path.
	if _, ok := localPathFor("", "2d/hero.png"); ok {
		t.Error("an unset template must produce nothing")
	}
	if _, ok := localPathFor("smb://nas/assets", ""); ok {
		t.Error("an empty relative path must produce nothing")
	}
}

// TestOpenWithPanel is the page half: with the mapping configured, the asset page
// offers the path and says plainly that it cannot launch the application.
func TestOpenWithPanel(t *testing.T) {
	ts := newTestServerWithConfig(t, func(cfg *config.Config) {
		cfg.LocalLibraryPath = "smb://nas.local/game/assets"
	})
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	ts.seedLibrary(t, map[string]string{"pack/hero.aseprite": "art"})
	id := ts.assetID(t, "pack/hero.aseprite")

	body := ts.body(t, ts.get(t, itoa("/assets/%d", id)))
	for _, want := range []string{
		"Open in",
		"smb://nas.local/game/assets/pack/hero.aseprite",
		"Aseprite", // suggested for the extension
		// M16: "Ambar cannot launch it for you" is a tooltip on the copy button now, not
		// a standing paragraph. The panel is a control, not documentation.
		"cannot open the file for you",
		`data-role="copy-path"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the open-with panel is missing %q", want)
		}
	}

	// Without the mapping, the panel explains how to enable it rather than showing a
	// path that would not work.
	plain := newTestServer(t)
	plain.createUser(t, testUsername, testPassword)
	plain.login(t, testUsername, testPassword)
	plain.seedLibrary(t, map[string]string{"pack/hero.aseprite": "art"})
	otherID := plain.assetID(t, "pack/hero.aseprite")
	body = plain.body(t, plain.get(t, itoa("/assets/%d", otherID)))
	if strings.Contains(body, "smb://") {
		t.Error("no local path may be shown when the mapping is unset")
	}
	// M16: how to configure the mapping is setup documentation, so the panel links to
	// /settings instead of reciting an environment variable.
	if !strings.Contains(body, `href="/settings#open-in"`) {
		t.Error("the panel should link to where the mapping is configured")
	}
}
