package texture

import (
	"os"
	"path/filepath"
	"testing"
)

func seed(t *testing.T, files ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, f := range files {
		abs := filepath.Join(root, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// TestAtlas: a model that names nothing findable — an OBJ with no materials — is drawn with
// the colour texture named like its product, whichever folder the model sits in.
func TestAtlas(t *testing.T) {
	const samurai = "3d/Synty/POLYGON - Samurai Pack/"
	root := seed(t,
		samurai+"Source/OBJ/SM_Bld_Tower_01.obj",
		samurai+"Source/Textures/Characters_Texture_01.png",
		samurai+"Source/Textures/PolygonSamurai_Emmision_Night.png",
		samurai+"Source/Textures/PolygonSamurai_Tex_01.png",
		samurai+"Source/Textures/PolygonSamurai_Tex_02.png",
		samurai+"Source/Textures/PolygonSamurai_Normal_01.png",
		"3d/Synty/POLYGON - Casino/Source/OBJ/SM_Prop_Chip_01.obj",
		"3d/Synty/POLYGON - Casino/Source/Textures/Felt_Green.png",
		"3d/Synty/SIMPLE - Trains/SIMPLE_Trains_ICON.png",
		"3d/Synty/SIMPLE - Trains/Source/Obj/SM_Train_01.obj",
	)

	got, ok := Atlas(root, samurai+"Source/OBJ/SM_Bld_Tower_01.obj", "3d/Synty")
	if want := filepath.Join(root, samurai+"Source/Textures/PolygonSamurai_Tex_01.png"); !ok || got != want {
		t.Errorf("Atlas = %q, %v; want the first colour atlas named for the product", got, ok)
	}

	// A product whose textures are named for something else has no atlas: a felt texture
	// across a whole casino chip is worse than its own colours.
	if got, ok := Atlas(root, "3d/Synty/POLYGON - Casino/Source/OBJ/SM_Prop_Chip_01.obj", "3d/Synty"); ok {
		t.Errorf("Atlas found %q for a product with no texture named for it", got)
	}
	// Nor is a pack icon ever one.
	if got, ok := Atlas(root, "3d/Synty/SIMPLE - Trains/Source/Obj/SM_Train_01.obj", "3d/Synty"); ok {
		t.Errorf("Atlas chose the pack icon %q", got)
	}
}

// TestResolveFallsBackToTheAtlas: a colour texture nothing else finds is the product's
// atlas, and a normal map never is.
func TestResolveFallsBackToTheAtlas(t *testing.T) {
	const casino = "3d/Synty/POLYGON - Casino/"
	root := seed(t,
		casino+"Source/FBX/SM_Bld_Roof_Cap_01.fbx",
		casino+"Source/Textures/PolygonCasino_Texture_01_A.png",
		casino+"Source/Textures/PolygonCasino_Texture_01_B.png",
	)
	model := casino + "Source/FBX/SM_Bld_Roof_Cap_01.fbx"

	got, how, ok := Resolve(root, model, "3d/Synty", `C:\Work\PolygonElven_Texture_01.png`)
	if want := filepath.Join(root, casino+"Source/Textures/PolygonCasino_Texture_01_A.png"); !ok || got != want || how != MatchAtlas {
		t.Errorf("Resolve = %q, %q, %v; want the Casino atlas", got, how, ok)
	}
	if got, _, ok := Resolve(root, model, "3d/Synty", "PolygonElven_Normal_01.png"); ok {
		t.Errorf("a normal map was answered with %q", got)
	}
}
