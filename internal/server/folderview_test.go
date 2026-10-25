package server

import (
	"html"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/datcal/ambar/internal/config"
	"github.com/datcal/ambar/internal/index"
)

// The folder view, end to end: "only the models in POLYGON - Spy Kit", reached by clicking,
// by pasting the path from a file manager, or by typing the pack's name into the search.

const spyKit = "3d/Vendor/POLYGON - Spy Kit"

func folderViewServer(t *testing.T) *testServer {
	t.Helper()
	ts := newTestServerWithConfig(t, func(c *config.Config) {
		c.LocalLibraryPath = "/mnt/game-assets"
	})
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	ts.seedLibrary(t, map[string]string{
		spyKit + "/cover.png":                       "cover",
		spyKit + "/Source/FBX/SM_Car.fbx":           "car fbx",
		spyKit + "/Source/Obj/SM_Car.obj":           "car obj",
		spyKit + "/Source/FBX/SM_Pen.fbx":           "pen fbx",
		spyKit + "/Source/Textures/Palette.png":     "palette",
		spyKit + "/Source/Audio/alarm.ogg":          "alarm",
		"3d/Vendor/POLYGON - Heist/FBX/SM_Safe.fbx": "safe",
		"2d/tiles/grass.png":                        "grass",
	})
	return ts
}

func location(t *testing.T, resp *http.Response) string {
	t.Helper()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}
	loc, err := url.QueryUnescape(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(loc, "+", " ")
}

func TestFolderViewShowsOnlyTheModelsInAFolder(t *testing.T) {
	ts := folderViewServer(t)

	body := ts.body(t, ts.get(t, "/?dir="+url.QueryEscape(spyKit)+"&kind=model"))

	// The models, and nothing from the neighbouring pack.
	for _, want := range []string{"SM_Car.fbx", "SM_Pen.fbx"} {
		if !strings.Contains(body, want) {
			t.Errorf("%s is missing from the folder's models", want)
		}
	}
	for _, unwanted := range []string{"SM_Safe.fbx", "cover.png", "grass.png"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("%s should not be listed", unwanted)
		}
	}
	// The kind switch counts this folder, not the library: two models (the car's FBX and
	// OBJ are one), two images, one sound.
	for _, want := range []string{"Models <span class=\"n\">2</span>", "Images <span class=\"n\">2</span>", "Audio <span class=\"n\">1</span>"} {
		if !strings.Contains(body, want) {
			t.Errorf("kind switch is missing %q", want)
		}
	}
	// Where you are: the breadcrumb, the title, the chips in the search box, and the path
	// on this machine for the copy button.
	for _, want := range []string{
		`aria-label="Folder path"`, `aria-current="location">POLYGON - Spy Kit</span>`,
		`class="scopechip"`, `name="dir" value="` + spyKit + `"`,
		`data-copy-path="/mnt/game-assets/` + spyKit + `"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the folder header is missing %q", want)
		}
	}
	// The subfolder offered under the toolbar counts models too.
	if !strings.Contains(body, `title="`+spyKit+`/Source"`) {
		t.Error("the Source subfolder is not offered")
	}
}

func TestFolderViewWithoutSubfolders(t *testing.T) {
	ts := folderViewServer(t)

	// Only what sits in the pack folder itself: the cover.
	body := ts.body(t, ts.get(t, "/?dir="+url.QueryEscape(spyKit)+"&sub=0"))
	if !strings.Contains(body, "cover.png") || strings.Contains(body, "SM_Car.fbx") {
		t.Error("sub=0 should list the folder's own files only")
	}
	if !strings.Contains(body, `role="switch"`) || !strings.Contains(body, `aria-checked="false"`) {
		t.Error("the subfolder switch does not show that it is off")
	}

	// Asking for models there finds none — and says where they are instead.
	body = ts.body(t, ts.get(t, "/?dir="+url.QueryEscape(spyKit)+"&sub=0&kind=model"))
	if !strings.Contains(body, "Show the 2 in its subfolders") {
		t.Errorf("the empty folder does not point at its subfolders:\n%s", body)
	}
}

func TestFolderViewListMode(t *testing.T) {
	ts := folderViewServer(t)
	body := ts.body(t, ts.get(t, "/?dir="+url.QueryEscape(spyKit)+"&view=list"))
	if !strings.Contains(body, `class="thumbgrid listview"`) {
		t.Error("view=list does not render the list")
	}
	// The view survives a search from the box.
	if !strings.Contains(body, `name="view" value="list"`) {
		t.Error("the search form drops the list view")
	}
}

func TestGoResolvesWhatYouPaste(t *testing.T) {
	ts := folderViewServer(t)

	cases := map[string]string{
		// A path on the operator's machine, with and without a trailing slash.
		"/mnt/game-assets/" + spyKit + "/": "/?dir=" + spyKit,
		"/mnt/game-assets/" + spyKit:       "/?dir=" + spyKit,
		// The same path as Windows would write it, and as a file:// link.
		`\mnt\game-assets\3d\Vendor\POLYGON - Spy Kit`:              "/?dir=" + spyKit,
		"file:///mnt/game-assets/3d/Vendor/POLYGON%20-%20Spy%20Kit": "/?dir=" + spyKit,
		// Library-relative, and a file inside a folder: the folder opens.
		spyKit + "/Source/FBX":            "/?dir=" + spyKit + "/Source/FBX",
		spyKit + "/Source/FBX/SM_Car.fbx": "/?dir=" + spyKit + "/Source/FBX",
		// Just a name.
		"spy kit": "/?dir=" + spyKit,
	}
	for in, want := range cases {
		got := location(t, ts.get(t, "/go?path="+url.QueryEscape(in)))
		if got != want {
			t.Errorf("/go %q => %q, want %q", in, got, want)
		}
	}

	// The kind travels along.
	if got := location(t, ts.get(t, "/go?kind=model&path="+url.QueryEscape("spy"))); !strings.Contains(got, "kind=model") {
		t.Errorf("the kind was dropped: %s", got)
	}

	// A path that is not in the library lands on the nearest folder that is, and says so.
	got := location(t, ts.get(t, "/go?path="+url.QueryEscape(spyKit+"/Nope/Deeper")))
	if !strings.HasPrefix(got, "/?dir="+spyKit) || !strings.Contains(got, "nearest folder") {
		t.Errorf("an unknown path => %q", got)
	}
	got = location(t, ts.get(t, "/go?path=nothing-is-called-this"))
	if !strings.Contains(got, "No folder is called") {
		t.Errorf("an unknown name => %q", got)
	}
}

func TestFolderFind(t *testing.T) {
	ts := folderViewServer(t)
	body := ts.body(t, ts.get(t, "/folders/find?q=spy&kind=model"))
	if !strings.Contains(body, "POLYGON - Spy Kit") || !strings.Contains(body, "kind=model") {
		t.Errorf("the jump box did not find the pack, keeping the kind: %s", body)
	}
	// Folders inside the pack are not matches for "spy"; they are reached by opening it.
	if strings.Contains(body, ">FBX<") {
		t.Errorf("a folder inside the match was offered too: %s", body)
	}
	if body := ts.body(t, ts.get(t, "/folders/find?q=zzzz")); !strings.Contains(body, "No folder by that name") {
		t.Errorf("an unknown name: %s", body)
	}
}

func TestSearchSuggestMovesTheScope(t *testing.T) {
	ts := folderViewServer(t)

	// The links are HTML-escaped ("+" is &#43;) and use + for a space; read them as a
	// browser would.
	readable := func(s string) string { return strings.ReplaceAll(html.UnescapeString(s), "+", " ") }

	// "spy" offers the pack as a scope, first.
	body := ts.body(t, ts.get(t, "/api/v1/suggest?q=spy"))
	if !strings.Contains(readable(body), `data-href="/?dir=`+spyKit+`"`) {
		t.Errorf("no folder row for 'spy': %s", body)
	}

	// Inside it, "mod" offers Models, counted there, keeping the folder.
	body = ts.body(t, ts.get(t, "/api/v1/suggest?q=mod&dir="+url.QueryEscape(spyKit)))
	unescaped := readable(body)
	if !strings.Contains(unescaped, "Models") || !strings.Contains(unescaped, "2 in POLYGON - Spy Kit") {
		t.Errorf("no kind row for 'mod': %s", body)
	}
	if !strings.Contains(unescaped, "dir="+spyKit) || !strings.Contains(unescaped, "kind=model") {
		t.Errorf("the kind row does not keep the folder: %s", body)
	}

	// An empty box in a folder offers its kinds; with a kind chosen already, nothing.
	if body := ts.body(t, ts.get(t, "/api/v1/suggest?q=&dir="+url.QueryEscape(spyKit))); !strings.Contains(body, "Show only") {
		t.Errorf("an empty scoped box offered no kinds: %s", body)
	}
	if body := ts.body(t, ts.get(t, "/api/v1/suggest?q=&kind=model&dir="+url.QueryEscape(spyKit))); strings.Contains(body, "suggest-item") {
		t.Errorf("a box with a kind chip offered kinds again: %s", body)
	}
}

func TestAssetPeekSaysWhichFileEachEngineTakes(t *testing.T) {
	ts := folderViewServer(t)

	id := ts.assetID(t, spyKit+"/Source/FBX/SM_Car.fbx")
	body := ts.body(t, ts.get(t, "/assets/"+strconv.FormatInt(id, 10)+"/peek"))
	for _, want := range []string{
		"SM_Car.fbx", "Formats", "SM_Car.obj", // both variants, each downloadable
		"Use in a game", "engine-Godot", "engine-Unreal",
		"Window › Ambar Asset Library", // where the Unreal plugin lives
		`data-copy-path="/mnt/game-assets/` + spyKit + `/Source/FBX/SM_Car.fbx"`,
		"Show in folder",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the panel is missing %q", want)
		}
	}
	if strings.Contains(body, "<html") {
		t.Error("the panel is a fragment, not a page")
	}

	// Unreal cannot take an .ogg; the panel says so instead of offering it.
	ogg := ts.assetID(t, spyKit+"/Source/Audio/alarm.ogg")
	body = ts.body(t, ts.get(t, "/assets/"+strconv.FormatInt(ogg, 10)+"/peek"))
	if !strings.Contains(body, "Unreal cannot import .ogg") {
		t.Errorf("the panel offers an .ogg to Unreal:\n%s", body)
	}
}

func TestEngineChoices(t *testing.T) {
	fbx := index.Asset{ID: 1, Ext: "fbx", Filename: "a.fbx"}
	glb := index.Asset{ID: 2, Ext: "glb", Filename: "a.glb"}
	obj := index.Asset{ID: 3, Ext: "obj", Filename: "a.obj"}

	choices := engineChoices("model", []index.Asset{obj, fbx, glb})
	got := map[string]int64{}
	for _, c := range choices {
		if c.Asset != nil {
			got[c.Engine] = c.Asset.ID
		}
	}
	// Godot is built around glTF, Unreal around FBX.
	if got["Godot"] != 2 || got["Unreal"] != 1 {
		t.Errorf("choices = %v, want Godot the .glb and Unreal the .fbx", got)
	}

	if engineChoices("tilemap", []index.Asset{{Ext: "tmx"}}) != nil {
		t.Error("a Tiled map is not something either engine imports as such")
	}

	for path, want := range map[string]string{
		"res://assets/a.png": "Godot", "/Game/Ambar/model/Pack/a": "Unreal", "elsewhere": "",
	} {
		if got := engineOf(path); got != want {
			t.Errorf("engineOf(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestLibraryLink(t *testing.T) {
	current := url.Values{"dir": {"2d"}, "page": {"4"}, "msg": {"hi"}, "kind": {"image"}}
	got := libraryLink(current, "dir", "3d/Vendor/A Pack")
	// Paging and one-off messages never carry over; slashes stay readable.
	if got != "/?dir=3d/Vendor/A+Pack&kind=image" {
		t.Errorf("libraryLink = %q", got)
	}
	if got := libraryLink(url.Values{"dir": {"x"}}, "dir", ""); got != "/" {
		t.Errorf("removing the last parameter = %q, want /", got)
	}
}

// "or all N" inside a folder tags those N — not every asset in the library that matches
// the same search — and the folder's tag facets work at all (they used to fail on the
// folder filter, which nobody saw while the tree was hidden under the tags).
func TestBulkTagAllStaysInTheFolder(t *testing.T) {
	ts := folderViewServer(t)

	status, _ := ts.postForm(t, "/assets/tags/bulk", urlValues(
		"tag", "batch:spy", "scope", "all", "q", "", "kind", "model", "dir", spyKit))
	if status != http.StatusSeeOther && status != http.StatusOK {
		t.Fatalf("bulk status = %d", status)
	}
	tagged := func(path string) bool {
		var n int
		ts.db.Reader.QueryRow(`SELECT count(*) FROM asset_tags at JOIN tags t ON t.id = at.tag_id
			WHERE at.asset_id = ? AND t.namespace = 'batch'`, ts.assetID(t, path)).Scan(&n)
		return n == 1
	}
	if !tagged(spyKit + "/Source/FBX/SM_Pen.fbx") {
		t.Error("a model in the folder was not tagged")
	}
	if tagged("3d/Vendor/POLYGON - Heist/FBX/SM_Safe.fbx") {
		t.Error("a model outside the folder was tagged")
	}

	// The Tags section loads from /facets with the page's own filters.
	page := ts.body(t, ts.get(t, "/?dir="+url.QueryEscape(spyKit)+"&kind=model"))
	if !strings.Contains(page, `data-src="/facets?dir=`) {
		t.Errorf("the Tags fold does not point at the folder's facets")
	}
	body := ts.body(t, ts.get(t, "/facets?kind=model&dir="+url.QueryEscape(spyKit)))
	if !strings.Contains(body, "batch:spy") {
		t.Errorf("the folder's tag facets do not list the tag just applied: %s", body)
	}
	if strings.Contains(body, "<html") {
		t.Error("/facets is a fragment, not a page")
	}
}

// A dozen subfolders are shortcuts; a vendor folder's 180 packs are a wall above the grid.
func TestManySubfoldersFold(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	files := map[string]string{"few/a/x.png": "1", "few/b/y.png": "2"}
	for i := 0; i < 13; i++ {
		files["many/pack"+strconv.Itoa(i)+"/x.png"] = strconv.Itoa(100 + i)
	}
	ts.seedLibrary(t, files)

	if body := ts.body(t, ts.get(t, "/?dir=many")); !strings.Contains(body, "13 subfolders</summary>") {
		t.Error("thirteen subfolders are not folded away")
	}
	body := ts.body(t, ts.get(t, "/?dir=few"))
	if strings.Contains(body, "subfolders</summary>") || !strings.Contains(body, `title="few/a"`) {
		t.Error("two subfolders should be shown as they are")
	}
}
