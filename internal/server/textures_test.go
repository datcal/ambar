package server

import (
	"io"
	"net/http"
	"net/url"
	"testing"
)

// TestTextureResolution walks the cases the real library produced: Synty source FBX files
// naming Photoshop working files, a typo, relative paths full of `..`, and an absolute
// Windows path — and the ones where a guess would be worse than nothing.
func TestTextureResolution(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)

	const trains = "3d/synty/SIMPLE - Trains/"
	const zombies = "3d/synty/POLYGON - Boss Zombies/"
	ts.seedLibrary(t, map[string]string{
		trains + "SIMPLE_Trains_ICON.png":                      "trains icon",
		trains + "Source/Fbx/SM_Env_Tree_04.fbx":               "fbx",
		trains + "Source/Textures/SimpleTrains_Texture_01.png": "trains 01",
		trains + "Source/Textures/SimpleTrains_Texture_02.png": "trains 02",
		trains + "Source/Textures/SimpleTrains_Texture_03.png": "trains 03",
		trains + "Source/Textures/Fence_Alpha.tga":             "fence tga",

		zombies + "POLYGON_BossZombies_ICON.png":                       "zombie icon",
		zombies + "Source/FBX/SM_Prop_Dumbbell_01.fbx":                 "fbx",
		zombies + "Source/FBX/Chr/SK_Chr_ZombieBoss_Brute_01.fbx":      "fbx",
		zombies + "Source/Textures/PolygonZombieBoss_Texture_01_A.png": "zombie 01 A",
		zombies + "Source/Textures/PolygonZombieBoss_Texture_01_B.png": "zombie 01 B",
		zombies + "Source/Textures/PolygonZombieBoss_Texture_04_A.png": "zombie 04 A",
		zombies + "Source/Textures/PolygonZombieBoss_Blood_Decal.tga":  "decal tga",

		"3d/kaykit/Assets/gltf/barrel.gltf":           "{}",
		"3d/kaykit/Assets/gltf/textures/wood.png":     "wood",
		"3d/kaykit/Assets/fbx/barrel.fbx":             "fbx",
		"3d/kaykit/Assets/Textures/hexagons_tile.png": "hexagons",
	})

	tree := ts.assetID(t, trains+"Source/Fbx/SM_Env_Tree_04.fbx")
	dumbbell := ts.assetID(t, zombies+"Source/FBX/SM_Prop_Dumbbell_01.fbx")
	brute := ts.assetID(t, zombies+"Source/FBX/Chr/SK_Chr_ZombieBoss_Brute_01.fbx")
	gltf := ts.assetID(t, "3d/kaykit/Assets/gltf/barrel.gltf")
	kayFBX := ts.assetID(t, "3d/kaykit/Assets/fbx/barrel.fbx")

	cases := []struct {
		name  string
		asset int64
		ref   string
		want  string // body served, or "" for 404
		how   string
	}{
		{"the user's tree: a renamed working file becomes the base atlas",
			tree, `..\Textures\SimpleTrainsTextures.psd`, "trains 01", "similar"},
		{"a .psd the .png was exported from",
			dumbbell, `..\_Working\Textures\PolygonZombieBoss_Texture_01_A.psd`, "zombie 01 A", "stem"},
		{"a typo'd name still finds its own atlas, not the first one",
			brute, `..\..\Textures\DPolygonZombieBoss_Texture_04_A.png`, "zombie 04 A", "similar"},
		{"a glTF's relative path, exactly",
			gltf, "textures/wood.png", "wood", "exact"},
		{"an artist's absolute Windows path, by name",
			kayFBX, `C:\Users\Kay Lousberg\Export Files\Textures\hexagons_tile.png`, "hexagons", "name"},

		{"a normal map is never guessed",
			dumbbell, `..\Textures\PolygonZombieBoss_Normal.psd`, "", ""},
		{"a decal only exists as .tga, which a browser cannot show",
			dumbbell, `..\..\PolygonZombies\_Working\Textures\Blood_Decal.tga`, "", ""},
		{"traversal finds nothing outside the pack",
			tree, `../../../../../../etc/passwd.png`, "", ""},
	}
	for _, c := range cases {
		resp := ts.get(t, itoa("/assets/%d/texture?ref=", c.asset)+url.QueryEscape(c.ref))
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		switch {
		case c.want == "" && resp.StatusCode != http.StatusNotFound:
			t.Errorf("%s: got %d %q, want 404", c.name, resp.StatusCode, body)
		case c.want != "" && resp.StatusCode != http.StatusOK:
			t.Errorf("%s: got %d, want the texture %q", c.name, resp.StatusCode, c.want)
		case c.want != "" && string(body) != c.want:
			t.Errorf("%s: served %q, want %q", c.name, body, c.want)
		case c.want != "" && resp.Header.Get("X-Ambar-Texture-Match") != c.how:
			t.Errorf("%s: matched by %q, want %q", c.name, resp.Header.Get("X-Ambar-Texture-Match"), c.how)
		}
	}
}

// TestTextureResolutionDoesNotPickTheIcon: a product with no textures of its own must not
// be dressed in its pack icon, whose name is the nearest thing to the pack's name.
func TestTextureResolutionDoesNotPickTheIcon(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	ts.seedLibrary(t, map[string]string{
		"3d/synty/SIMPLE - Trains/SIMPLE_Trains_ICON.png":  "icon",
		"3d/synty/SIMPLE - Trains/Source/Fbx/SM_Train.fbx": "fbx",
	})
	id := ts.assetID(t, "3d/synty/SIMPLE - Trains/Source/Fbx/SM_Train.fbx")
	resp := ts.get(t, itoa("/assets/%d/texture?ref=", id)+url.QueryEscape(`..\Textures\SimpleTrainsTextures.psd`))
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("got %d; the icon must not stand in for a texture", resp.StatusCode)
	}
}

// TestTextureResolutionLooksInsideTextureFolders: most Synty packs keep their atlases a
// folder down, in Textures/Alts, beside Textures/Normals and Textures/Misc.
func TestTextureResolutionLooksInsideTextureFolders(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	const casino = "3d/synty/POLYGON - Casino/"
	ts.seedLibrary(t, map[string]string{
		casino + "Source/FBX/Building/SM_Bld_Stairs_01.fbx":            "fbx",
		casino + "Source/Textures/Alts/PolygonCasino_Texture_01_A.png": "casino 01 A",
		casino + "Source/Textures/Alts/PolygonCasino_Texture_01_B.png": "casino 01 B",
		casino + "Source/Textures/Normals/PolygonCasino_Normal_01.png": "normal",
		casino + "Source/Textures/Misc/Coin_Gold.png":                  "coin",
		// Inside the model's own folder tree, two levels down: another model's, not a texture
		// folder, so never searched.
		casino + "Source/FBX/Building/Other/PolygonCasino_Texture_01.png": "wrong",
	})
	id := ts.assetID(t, casino+"Source/FBX/Building/SM_Bld_Stairs_01.fbx")
	resp := ts.get(t, itoa("/assets/%d/texture?ref=", id)+url.QueryEscape(
		`..\..\..\..\..\Dropbox\SyntyStudios\PolygonCasino\Working\Textures\PolygonCasino_Texture_01.psd`))
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "casino 01 A" {
		t.Errorf("got %d %q, want the Alts atlas 01_A", resp.StatusCode, body)
	}
}
