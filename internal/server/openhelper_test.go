package server

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

// TestLinuxHelperFindsASteamInstall runs the generated script, which is the only way to
// know whether it works — this is a shell program shipped as a Go string literal, and no
// amount of reading it caught the bug it is written against.
//
// The bug: "open in Aseprite" opened Aseprite with nothing in it. Aseprite takes files
// perfectly well ("aseprite [OPTIONS] [FILES]..."), but it was bought on Steam, so there
// is no `aseprite` on PATH and no Flatpak — and the script fell through to xdg-open,
// which honours Steam's own .desktop entry:
//
//	Exec=steam steam://rungameid/431730
//
// That carries no file argument. The application launched, empty, every time, which is
// indistinguishable from the ambar:// scheme not being registered at all.
//
// The fixture is a Steam library with a space in the install directory ("Godot Engine",
// which is what Steam really calls it), because the resolved command used to be expanded
// unquoted and would have split into two arguments.
func TestLinuxHelperFindsASteamInstall(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the linux helper is a POSIX shell script")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no POSIX shell available")
	}

	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	script := filepath.Join(t.TempDir(), "ambar-open.sh")
	if err := os.WriteFile(script, []byte(ts.body(t, ts.get(t, "/settings/open-helper?platform=linux"))), 0o700); err != nil {
		t.Fatal(err)
	}

	// A fake HOME holding a fake Steam library, reachable the way the real one is.
	home := t.TempDir()
	steam := filepath.Join(home, ".local", "share", "Steam")
	for dir, binary := range map[string]string{"Aseprite": "aseprite", "Godot Engine": "godot.x11.opt.tools.64"} {
		full := filepath.Join(steam, "steamapps", "common", dir)
		if err := os.MkdirAll(full, 0o755); err != nil {
			t.Fatal(err)
		}
		stub := "#!/bin/sh\nprintf 'args=%d first=[%s]\\n' \"$#\" \"$1\"\n"
		if err := os.WriteFile(filepath.Join(full, binary), []byte(stub), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	// The file being opened, with a space in its name for the same reason.
	asset := filepath.Join(t.TempDir(), "a sprite.aseprite")
	if err := os.WriteFile(asset, []byte("not really an aseprite"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := func(app string) string {
		return "ambar://open?app=" + app + "&path=" + strings.ReplaceAll(asset, " ", "%20")
	}

	// tolerateExit, because --check reports a non-zero status when the scheme is not
	// registered — which it is not, inside a temporary HOME, and that is the honest answer.
	run := func(t *testing.T, tolerateExit bool, args ...string) string {
		t.Helper()
		cmd := exec.Command(sh, append([]string{script}, args...)...)
		cmd.Env = append(os.Environ(), "HOME="+home)
		out, err := cmd.CombinedOutput()
		if err != nil && !tolerateExit {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		return string(out)
	}

	// It launches the Steam binary, and the stub reports exactly one argument — so
	// neither the space in "Godot Engine" nor the one in the filename split.
	for _, app := range []string{"aseprite", "godot"} {
		got := run(t, false, link(app))
		want := "args=1 first=[" + asset + "]"
		if strings.TrimSpace(got) != want {
			t.Errorf("%s: helper produced %q, want %q", app, strings.TrimSpace(got), want)
		}
	}

	// And --check names the command, because "the editor opened empty" needs a line
	// somebody can read rather than a guess about which of three failures it was.
	report := run(t, true, "--check")
	if !strings.Contains(report, filepath.Join(steam, "steamapps", "common", "Aseprite", "aseprite")) {
		t.Errorf("--check does not report where aseprite was found:\n%s", report)
	}
}
