package derive

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"time"

	"github.com/HugoSmits86/nativewebp"
	"github.com/datcal/ambar/internal/blender"
	"github.com/datcal/ambar/internal/db"
	"github.com/datcal/ambar/internal/jobs"
	"github.com/datcal/ambar/internal/safepath"
	"github.com/datcal/ambar/internal/texture"
)

// Model thumbnails drawn by Blender.
//
// A model's derive produces geometry and never a picture; the picture came from a
// browser (M15), one tile at a time as someone scrolled past it. A Synty import put
// eighty thousand models in the library and a browser had drawn five thousand of them, so
// most of the grid, the Unreal plugin and the MCP endpoint showed an extension chip.
//
// A worker with Blender draws them instead. It already opens every FBX to convert it, and
// rendering what it opened is a fraction of a second more: Workbench, lit by a studio
// light and needing no GPU, framed as the browser frames it. The textures are found by the
// same search the browser uses (internal/texture), run on the worker against its own
// mount of the library. The server keeps every write: the worker sends the picture back,
// and Accept moves it into place exactly as a derive's files are.
//
// It is a job of its own, model.thumb, rather than part of a derive: derive_version says
// whether a model's analysis is current, and a picture drawn by a better renderer than
// the last one is not a reason to analyse eighty thousand models again.

// ThumbJobType is the queue type for drawing one model's thumbnail with Blender.
const ThumbJobType = "model.thumb"

// OpThumb marks a Task that draws a thumbnail rather than deriving.
const OpThumb = "thumb"

// FileThumbMeta records, beside a model's thumb.webp, how it was drawn.
const FileThumbMeta = "thumb.json"

// The model-thumbnail renderers, recorded in thumb.json. A thumbnail drawn by an older
// one is stale to a newer one, which replaces it.
const (
	// RendererBrowserV1 is the original browser renderer: textures fetched by the path the
	// model carries. A thumbnail without a thumb.json is one of these.
	RendererBrowserV1 = 1
	// RendererBrowser is the browser renderer today (modelthumb.js): textures resolved by
	// name through /assets/{id}/texture.
	RendererBrowser = 2
	// RendererBlender is a worker's Blender, lit by a studio light, with the product's
	// atlas for a mesh that names no texture.
	RendererBlender = 3
)

// ThumbMeta is FileThumbMeta's content.
type ThumbMeta struct {
	Renderer int `json:"renderer"`
	// Untextured is how many of the model's textures could not be found when it was
	// drawn.
	Untextured int   `json:"untextured"`
	DrawnAt    int64 `json:"drawn_at"`
	// Nothing is why a renderer found nothing to draw — an FBX that holds an animation and
	// no mesh. Recorded so the model is not handed out again to a renderer that would
	// find the same; there is no thumb.webp beside it.
	Nothing string `json:"nothing,omitempty"`
}

// ReadThumbMeta reads the record beside a thumbnail. A thumbnail without one predates the
// record, which is the same as RendererBrowserV1.
func ReadThumbMeta(outDir string) ThumbMeta {
	raw, err := os.ReadFile(filepath.Join(outDir, FileThumbMeta))
	if err != nil {
		return ThumbMeta{Renderer: RendererBrowserV1}
	}
	var m ThumbMeta
	if json.Unmarshal(raw, &m) != nil || m.Renderer < 1 {
		return ThumbMeta{Renderer: RendererBrowserV1}
	}
	return m
}

// ThumbStatus is what a model's thumbnail needs: nothing, a first render, or a redraw.
type ThumbStatus int

const (
	ThumbCurrent ThumbStatus = iota
	ThumbMissing
	ThumbStale
)

// ModelThumbStatus decides, from the derivative directory alone, what a renderer of
// version want would do for a model: draw it, draw it again, or leave it.
func ModelThumbStatus(outDir string, want int) ThumbStatus {
	meta := ReadThumbMeta(outDir)
	if _, err := os.Stat(filepath.Join(outDir, FileThumb)); err != nil {
		if meta.Nothing != "" && meta.Renderer >= want {
			return ThumbCurrent
		}
		return ThumbMissing
	}
	if meta.Renderer < want {
		return ThumbStale
	}
	return ThumbCurrent
}

// minVisibleRatio is how much of a thumbnail must actually be covered by the model.
// A 512x512 frame of a barrel fills a good part of it; 0.2% is far below anything a
// real render produces and far above the zero a failed one gives.
const minVisibleRatio = 0.002

// BlankImage reports whether an image has (almost) nothing visible in it: what a
// renderer that loaded a model and drew nothing hands back.
func BlankImage(img image.Image) bool {
	b := img.Bounds()
	if b.Empty() {
		return true
	}

	// Every 4th pixel in each direction: 1/16th of the work, and a model that covers
	// 0.2% of the frame still lands dozens of samples.
	const step = 4
	var sampled, visible int
	for y := b.Min.Y; y < b.Max.Y; y += step {
		for x := b.Min.X; x < b.Max.X; x += step {
			sampled++
			if _, _, _, a := img.At(x, y).RGBA(); a > 0x1000 {
				visible++
			}
		}
	}
	if sampled == 0 {
		return true
	}
	return float64(visible)/float64(sampled) < minVisibleRatio
}

// blenderThumbExts are the model formats Blender draws.
var blenderThumbExts = map[string]bool{"fbx": true, "obj": true, "gltf": true, "glb": true, "blend": true}

// ThumbPayload is the job payload: the asset whose content is drawn.
type ThumbPayload struct {
	AssetID int64 `json:"asset_id"`
}

// ThumbDedupeKey keeps one job per content hash and renderer in the queue.
func ThumbDedupeKey(sha256hex string) string {
	return fmt.Sprintf("%s:%s:%d", ThumbJobType, sha256hex, RendererBlender)
}

// thumbPriority keeps thumbnails behind derive work: a model's analysis, and the FBX
// conversion a thumbnail is no substitute for, come first.
const thumbPriority = -1

// RegisterThumb attaches the local handler. The server's own workers only take these jobs
// when it has Blender itself; otherwise they wait for a worker that has.
func (d *Deriver) RegisterThumb(q *jobs.Queue) {
	q.Register(ThumbJobType, d.HandleThumb)
}

// CanDrawThumbs reports whether this server has a Blender to draw thumbnails with.
func (d *Deriver) CanDrawThumbs() bool {
	_, ok := blender.Locate(d.blenderBin, d.dataRoot)
	return ok
}

// HandleThumb draws one model's thumbnail with this server's Blender.
func (d *Deriver) HandleThumb(ctx context.Context, raw []byte) error {
	var p ThumbPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return fmt.Errorf("unmarshal thumbnail payload: %w", err)
	}
	task, done, err := d.PrepareThumb(ctx, p.AssetID)
	if err != nil || done {
		return err
	}
	return d.FinishThumb(ctx, task, RenderThumb(ctx, task, d.libraryRoot, d.dataRoot, d.blenderBin))
}

// PrepareThumb loads what drawing an asset's thumbnail needs. done means there is nothing
// to draw: the asset is gone, missing, not a model Blender draws, not derived yet, or its
// thumbnail is already current.
func (d *Deriver) PrepareThumb(ctx context.Context, assetID int64) (task Task, done bool, err error) {
	asset, err := d.loadAsset(ctx, assetID)
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, true, nil
	}
	if err != nil {
		return Task{}, false, err
	}
	if asset.missing || !blenderThumbExts[asset.ext] || asset.deriveState != StateOK {
		return Task{}, true, nil
	}
	relDir, err := Dir(asset.sha256)
	if err != nil {
		return Task{}, true, nil
	}
	if ModelThumbStatus(filepath.Join(d.dataRoot, relDir), RendererBlender) == ThumbCurrent {
		return Task{}, true, nil
	}
	return Task{
		Op:            OpThumb,
		AssetID:       asset.id,
		SHA256:        asset.sha256,
		Ext:           asset.ext,
		Filename:      asset.filename,
		LibraryPath:   asset.libraryPath(),
		PackPath:      asset.packRelPath,
		Size:          asset.size,
		DeriveVersion: Version,
	}, false, nil
}

// libraryTextures is blender.Textures over a library on this machine.
type libraryTextures struct {
	root, model, pack string
}

func (t libraryTextures) Resolve(ref string) string {
	abs, _, ok := texture.Resolve(t.root, t.model, t.pack, ref)
	if !ok {
		return ""
	}
	return abs
}

func (t libraryTextures) Atlas() string {
	abs, _ := texture.Atlas(t.root, t.model, t.pack)
	return abs
}

// RenderThumb draws a task's model with Blender: the file and its textures come from
// libraryRoot, and thumb.webp, preview.webp and thumb.json are written to the content's
// directory under dataRoot — the data root itself on a server, a scratch directory on a
// worker.
func RenderThumb(ctx context.Context, task Task, libraryRoot, dataRoot, blenderBin string) Outcome {
	absPath, failed := openTaskFile(task, libraryRoot)
	if failed != nil {
		return *failed
	}
	bin, ok := blender.Locate(blenderBin, dataRoot)
	if !ok {
		return Outcome{State: StateFailed, Message: "no Blender to draw the thumbnail with"}
	}
	relDir, err := Dir(task.SHA256)
	if err != nil {
		return Outcome{State: StateFailed, Message: err.Error()}
	}
	outDir := filepath.Join(dataRoot, relDir)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return Outcome{State: StateFailed, Message: fmt.Sprintf("create derivative directory: %v", err)}
	}

	// Blender writes a PNG beside the derivatives: a sandboxed Blender can write there and
	// nowhere else. It is read back, checked and re-encoded, and never kept.
	drawn := filepath.Join(outDir, "blender-thumb.png")
	defer os.Remove(drawn) //nolint:errcheck
	info, err := blender.Thumbnail(ctx, bin, absPath, drawn, ThumbSize,
		libraryTextures{root: libraryRoot, model: task.LibraryPath, pack: task.PackPath})
	if errors.Is(err, blender.ErrNothingToDraw) {
		return Outcome{State: StateUnsupported, Message: err.Error()}
	}
	if err != nil {
		return Outcome{State: StateFailed, Message: err.Error()}
	}

	img, err := readPNG(drawn)
	if err != nil {
		return Outcome{State: StateFailed, Message: err.Error()}
	}
	if BlankImage(img) {
		return Outcome{State: StateUnsupported, Message: "nothing to draw: the model rendered empty"}
	}
	// The same image is the preview, as the browser's is, so opening the asset does not
	// draw it again. Encoded once: a lossless 512 px WebP is most of a worker's own CPU time.
	var encoded bytes.Buffer
	if err := nativewebp.Encode(&encoded, img, nil); err != nil {
		return Outcome{State: StateFailed, Message: fmt.Sprintf("encode the thumbnail: %v", err)}
	}
	for _, name := range []string{FileThumb, FilePreview} {
		if err := writeFileAtomic(filepath.Join(outDir, name), encoded.Bytes()); err != nil {
			return Outcome{State: StateFailed, Message: err.Error()}
		}
	}
	meta, _ := json.Marshal(ThumbMeta{Renderer: RendererBlender, Untextured: info.Untextured, DrawnAt: time.Now().Unix()})
	if err := writeFileAtomic(filepath.Join(outDir, FileThumbMeta), meta); err != nil {
		return Outcome{State: StateFailed, Message: err.Error()}
	}

	var notes []string
	if info.Untextured > 0 {
		notes = append(notes, fmt.Sprintf("%d of %d textures not found", info.Untextured, info.Textures))
	}
	if info.Atlas {
		notes = append(notes, "drawn with the product's atlas")
	}
	return Outcome{State: StateOK, Result: &Result{
		Files: []string{FileThumb, FilePreview, FileThumbMeta},
		Notes: notes,
	}}
}

func readPNG(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read Blender's render: %w", err)
	}
	defer f.Close() //nolint:errcheck
	img, err := png.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("decode Blender's render: %w", err)
	}
	return img, nil
}

// AcceptThumb checks a remote worker's thumbnail, moves it into place and records it.
func (d *Deriver) AcceptThumb(ctx context.Context, task Task, outcome Outcome, staging string) error {
	if outcome.State == StateOK {
		if outcome.Result == nil {
			return fmt.Errorf("a successful outcome for asset %d carried no result", task.AssetID)
		}
		if err := d.place(task, outcome.Result.Files, staging); err != nil {
			return err
		}
	}
	return d.FinishThumb(ctx, task, outcome)
}

// FinishThumb records a drawn thumbnail, wherever it was drawn. Unlike Finish it never
// touches derive_state: the model's analysis is what it was, with or without a picture.
func (d *Deriver) FinishThumb(ctx context.Context, task Task, outcome Outcome) error {
	relDir, err := Dir(task.SHA256)
	if err != nil {
		return err
	}
	outDir := filepath.Join(d.dataRoot, relDir)

	switch outcome.State {
	case StateOK:
		// A palette, from the picture: colour search reads it (M17).
		f, err := os.Open(filepath.Join(outDir, FileThumb))
		if err != nil {
			return fmt.Errorf("open the new thumbnail: %w", err)
		}
		img, _, err := image.Decode(f)
		f.Close() //nolint:errcheck
		if err != nil {
			return fmt.Errorf("decode the new thumbnail: %w", err)
		}
		if err := RecordImagePalette(ctx, d.db, task.SHA256, img); err != nil {
			d.log.WarnContext(ctx, "could not record a model palette", "asset_id", task.AssetID, "error", err)
		}
		return nil

	case StateUnsupported:
		// Nothing to draw. Remembered, so the model is not handed out again; a picture a
		// browser drew of it, if there is one, stays.
		meta, _ := json.Marshal(ThumbMeta{Renderer: RendererBlender, DrawnAt: time.Now().Unix(), Nothing: outcome.Message})
		if err := os.MkdirAll(outDir, 0o755); err != nil {
			return err
		}
		return writeFileAtomic(filepath.Join(outDir, FileThumbMeta), meta)

	default:
		// A failure is retried by the queue; the next connection of a worker with Blender
		// offers it again after that.
		return errors.New(outcome.Message)
	}
}

// EnqueueModelThumbs queues a thumbnail for every model whose picture is missing or was
// drawn by an older renderer than Blender. Called when a worker with Blender connects, and
// at startup on a server with Blender of its own.
//
// It converges: a model drawn by Blender, or one Blender found nothing in, is current and
// left alone. Only models whose derive is done are queued; one still waiting for its
// derive is queued when that finishes (EnqueueThumb).
func EnqueueModelThumbs(ctx context.Context, database *db.DB, q *jobs.Queue, dataRoot string) (int, error) {
	rows, err := database.Reader.QueryContext(ctx, `
		SELECT sha256, min(id) FROM assets
		WHERE kind = 'model' AND missing_since IS NULL AND derive_state = ?
		  AND ext IN ('fbx', 'obj', 'gltf', 'glb', 'blend')
		GROUP BY sha256
		ORDER BY min(id)`, StateOK)
	if err != nil {
		return 0, fmt.Errorf("find models to draw: %w", err)
	}
	type model struct {
		sha256 string
		id     int64
	}
	var todo []model
	for rows.Next() {
		var m model
		if err := rows.Scan(&m.sha256, &m.id); err != nil {
			rows.Close() //nolint:errcheck
			return 0, err
		}
		todo = append(todo, m)
	}
	rows.Close() //nolint:errcheck // done reading; free the read connection before the stats
	if err := rows.Err(); err != nil {
		return 0, err
	}

	enqueued := 0
	for _, m := range todo {
		relDir, err := Dir(m.sha256)
		if err != nil {
			continue
		}
		if ModelThumbStatus(filepath.Join(dataRoot, relDir), RendererBlender) == ThumbCurrent {
			continue
		}
		ok, err := EnqueueThumb(ctx, q, m.id, m.sha256)
		if err != nil {
			return enqueued, err
		}
		if ok {
			enqueued++
		}
	}
	return enqueued, nil
}

// QueueThumbs is EnqueueModelThumbs on the queue the deriver is registered with.
func (d *Deriver) QueueThumbs(ctx context.Context) (int, error) {
	if d.queue == nil {
		return 0, errors.New("the deriver is not registered with a queue")
	}
	return EnqueueModelThumbs(ctx, d.db, d.queue, d.dataRoot)
}

// EnqueueThumb queues one model's thumbnail, reporting false when it is already queued.
func EnqueueThumb(ctx context.Context, q *jobs.Queue, assetID int64, sha256hex string) (bool, error) {
	_, err := q.Enqueue(ctx, ThumbJobType, ThumbPayload{AssetID: assetID}, jobs.EnqueueOptions{
		Priority:  thumbPriority,
		DedupeKey: ThumbDedupeKey(sha256hex),
	})
	switch {
	case errors.Is(err, jobs.ErrDuplicate):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("enqueue a model thumbnail: %w", err)
	}
	return true, nil
}

// DrawsThumb reports whether a model of this extension gets a thumbnail from Blender.
func DrawsThumb(ext string) bool { return blenderThumbExts[ext] }

// openTaskFile resolves a task's file under libraryRoot and checks it is the size the
// last scan recorded, or says why not as a failed outcome.
func openTaskFile(task Task, libraryRoot string) (string, *Outcome) {
	absPath, err := safepath.ResolveExisting(libraryRoot, task.LibraryPath)
	if err != nil {
		return "", &Outcome{State: StateFailed,
			Message: fmt.Sprintf("the worker could not open %s under its library: %v", task.LibraryPath, err)}
	}
	info, err := os.Stat(absPath)
	if err != nil {
		return "", &Outcome{State: StateFailed, Message: fmt.Sprintf("the worker could not stat the file: %v", err)}
	}
	if info.Size() != task.Size {
		return "", &Outcome{State: StateFailed, Message: fmt.Sprintf(
			"the file is %d bytes but the last scan recorded %d; run `ambar scan`", info.Size(), task.Size)}
	}
	return absPath, nil
}
