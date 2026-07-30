package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/datcal/ambar/internal/config"
)

// TestOpenAppsFor: the launch links carry everything the helper needs — the app key
// and the path — because the helper has no credentials and no idea what Ambar is.
func TestOpenAppsFor(t *testing.T) {
	apps := openAppsFor("aseprite", "/mnt/assets/pack/hero.aseprite")
	if len(apps) < 2 {
		t.Fatalf("apps = %+v, want Aseprite plus the file manager", apps)
	}
	if apps[0].Label != "Aseprite" {
		t.Errorf("first app = %q, want Aseprite", apps[0].Label)
	}
	if !strings.HasPrefix(string(apps[0].URL), "ambar://open?app=aseprite&path=") {
		t.Errorf("url = %q", apps[0].URL)
	}
	// The path is escaped, so a space or a hash cannot break the link.
	spaced := openAppsFor("png", "/mnt/assets/my pack/hero #2.png")
	if url := string(spaced[0].URL); strings.Contains(url, " ") || strings.Contains(url, "#") {
		t.Errorf("the path is not escaped: %q", string(spaced[0].URL))
	}

	// An unknown extension still gets the file manager, which is useful for anything.
	other := openAppsFor("scml", "/mnt/assets/x.scml")
	if len(other) != 1 || other[0].Key != "reveal" {
		t.Errorf("unknown extension = %+v, want just reveal", other)
	}

	// No local path means no launch links: the helper takes the path from the URL, so a
	// link without one would do nothing at all.
	if apps := openAppsFor("aseprite", ""); apps != nil {
		t.Errorf("apps without a local path = %+v, want none", apps)
	}
}

// TestOpenHelperDownload: the helper is generated per platform and is meant to be read
// before it is trusted, so it arrives as plain text with its instructions in it.
func TestOpenHelperDownload(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)

	cases := map[string][]string{
		"linux":   {"x-scheme-handler/ambar", "--install", "aseprite"},
		"macos":   {"CFBundleURLSchemes", "open -a Aseprite"},
		"windows": {"HKCU:\\Software\\Classes\\ambar", "URL Protocol"},
	}
	for platform, wants := range cases {
		resp := ts.get(t, "/settings/open-helper?platform="+platform)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: status = %d", platform, resp.StatusCode)
			continue
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
			t.Errorf("%s: content-type = %q, want text/plain so it can be read", platform, ct)
		}
		body := ts.body(t, resp)
		for _, want := range wants {
			if !strings.Contains(body, want) {
				t.Errorf("%s helper is missing %q", platform, want)
			}
		}
		// It must refuse to pretend an smb:// URL is a path, because no application can
		// open one.
		if !strings.Contains(strings.ToLower(body), "smb://") {
			t.Errorf("%s helper does not handle the smb:// case", platform)
		}
	}

	if resp := ts.get(t, "/settings/open-helper?platform=amiga"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown platform: status = %d, want 400", resp.StatusCode)
	}
}

// TestAssetPageOffersLaunchButtons: with a local path configured, the asset page offers
// real launch buttons and still offers the path for the case where the helper is not
// installed.
func TestAssetPageOffersLaunchButtons(t *testing.T) {
	ts := newTestServerWithConfig(t, func(cfg *config.Config) {
		cfg.LocalLibraryPath = "/Volumes/game/assets"
	})
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	ts.seedLibrary(t, map[string]string{"pack/hero.aseprite": "art"})
	id := ts.assetID(t, "pack/hero.aseprite")

	body := ts.body(t, ts.get(t, itoa("/assets/%d", id)))
	for _, want := range []string{
		"ambar://open?app=aseprite",
		">Aseprite<",
		"/settings", // where the helper comes from
		"/Volumes/game/assets/pack/hero.aseprite", // the fallback that works today
		`data-role="copy-path"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the open-in panel is missing %q", want)
		}
	}
}
