package derive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/datcal/ambar/internal/blender"
	"github.com/datcal/ambar/internal/model"
)

// FileModelPreview is the normalised model the §8 three.js viewer loads (§6:
// "normalise everything to preview.glb").
const FileModelPreview = "preview.glb"

// ErrNeedsBlender means a model can only be handled once Blender is installed
// (§6). It is recorded as derive_state=needs_blender — not a failure, not
// retried — so the UI can say what is missing.
var ErrNeedsBlender = errors.New("this format needs Blender, which is not installed")

// blenderExts are the model formats §6 routes through Blender.
var blenderExts = map[string]bool{"fbx": true, "blend": true}

// modelExts are all the §5.1/§6 model formats the deriver recognises. glTF/GLB
// derive in pure Go; FBX/.blend wait for Blender; the rest need a converter that
// is not built yet and are reported unsupported with a reason.
var modelExts = map[string]bool{
	"glb": true, "gltf": true, "obj": true, "fbx": true, "blend": true,
	"dae": true, "stl": true, "ply": true, "3ds": true, "escn": true,
}

func isModelExt(ext string) bool { return modelExts[ext] }

// deriveModel is Generate's 3D branch: for glTF/GLB it extracts metadata and
// writes a normalised preview.glb; FBX/.blend defer to Blender; anything else has
// no pure-Go path yet.
func deriveModel(opts GenerateOptions) (*Result, error) {
	relDir, err := Dir(opts.SHA256)
	if err != nil {
		return nil, err
	}
	outDir := filepath.Join(opts.DataRoot, relDir)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, fmt.Errorf("create derivative directory: %w", err)
	}
	previewPath := filepath.Join(outDir, FileModelPreview)

	switch {
	case model.CanRead(opts.Ext):
		// glTF/GLB/OBJ: normalise and analyse in pure Go.
		info, err := model.Normalize(opts.AbsPath, previewPath)
		if errors.Is(err, model.ErrNoGeometry) {
			// Nothing to show, and nothing a retry would change.
			return nil, fmt.Errorf("%w: %v", ErrUnsupported, err)
		}
		if err != nil {
			return nil, err
		}
		return modelResult(info), nil

	case blenderExts[opts.Ext]:
		if opts.Ext == "fbx" && !binaryFBX(opts.AbsPath) {
			// Blender's importer refuses ASCII FBX outright, so no Blender can help.
			return nil, fmt.Errorf("%w: an ASCII FBX; Blender reads only binary FBX", ErrUnsupported)
		}
		// FBX/.blend: use Blender if one is configured, otherwise defer.
		bin, ok := blender.Locate(opts.BlenderBin, opts.DataRoot)
		if !ok {
			return nil, fmt.Errorf("%w (.%s)", ErrNeedsBlender, opts.Ext)
		}
		if err := blender.Convert(context.Background(), bin, opts.AbsPath, previewPath); err != nil {
			return nil, err // a Blender failure is a real failure, retried
		}
		// The metadata comes from the GLB Blender just produced.
		info, err := model.Analyze(previewPath)
		if err != nil {
			return nil, err
		}
		return modelResult(info), nil

	default:
		return nil, fmt.Errorf("%w: .%s → glb conversion is not implemented yet", ErrUnsupported, opts.Ext)
	}
}

// binaryFBX reports whether path starts as a binary FBX does. An unreadable file
// counts as binary, so Blender is the one to report what is wrong with it.
func binaryFBX(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return true
	}
	defer f.Close()
	head := make([]byte, len(fbxBinaryMagic))
	if _, err := io.ReadFull(f, head); err != nil {
		return true
	}
	return string(head) == fbxBinaryMagic
}

const fbxBinaryMagic = "Kaydara FBX Binary"

func modelResult(info model.Info) *Result {
	m := info
	return &Result{
		Model:          &m,
		AnimationNames: info.AnimationNames,
		Files:          []string{FileModelPreview},
	}
}
