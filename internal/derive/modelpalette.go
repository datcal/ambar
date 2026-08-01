package derive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"os"
	"path/filepath"

	"github.com/datcal/ambar/internal/db"
	"github.com/datcal/ambar/internal/jobs"
	"github.com/datcal/ambar/internal/palette"

	// The same decoders the rest of derive registers. Our own thumbnails are lossless
	// WebP written by nativewebp, and x/image/webp reads VP8L, so this closes the loop:
	// what we wrote, we can read.
	_ "golang.org/x/image/webp"
)

// Giving 3D models a colour palette (M17).
//
// `color:` search worked on images and silently returned nothing for models. Not a bug in
// the search — a structural gap: swatches are extracted from a decoded image, and a model's
// derive produces a preview.glb and never an image, so there was nothing to extract from.
// Measured on the real library: 926 models, zero swatches.
//
// The browser-rendered thumbnail (M15) is the missing image. The upload handler extracts a
// palette from every new one, but that leaves every thumbnail already on disk — 235 of them
// here — which is what this job is for. It reads the stored thumb.webp rather than asking
// the browser to render again: the picture exists, and re-rendering hundreds of models to
// recompute something from a file we already have would be work for its own sake.
//
// What the palette describes is the *render*, lighting included. A white model under warm
// light reads slightly warm. That is a fair account of what the thing looks like, which is
// what a colour filter is for; reading base-colour factors out of the glTF instead would
// ignore every texture and be confidently wrong.

// PaletteJobType is the queue type for extracting one model's palette from its thumbnail.
const PaletteJobType = "asset.modelpalette"

// PalettePayload names the content to work on. Keyed by hash like every other derivative:
// two copies of the same model share the thumbnail and the palette.
type PalettePayload struct {
	SHA256 string `json:"sha256"`
}

// RegisterPalette wires the handler into the queue.
func (d *Deriver) RegisterPalette(q *jobs.Queue) {
	q.Register(PaletteJobType, d.HandlePalette)
}

// HandlePalette extracts and records one model palette.
func (d *Deriver) HandlePalette(ctx context.Context, raw []byte) error {
	var payload PalettePayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return fmt.Errorf("decode model palette payload: %w", err)
	}
	if payload.SHA256 == "" {
		return fmt.Errorf("model palette job has no content hash")
	}

	relDir, err := Dir(payload.SHA256)
	if err != nil {
		return err
	}
	thumbPath := filepath.Join(d.dataRoot, relDir, FileThumb)

	file, err := os.Open(thumbPath)
	if err != nil {
		// No thumbnail yet: nothing to read, and nothing wrong. The browser will render
		// one eventually and that path extracts the palette itself.
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("open model thumbnail: %w", err)
	}
	defer file.Close() //nolint:errcheck

	img, _, err := image.Decode(file)
	if err != nil {
		// A derivative we wrote that we cannot read is worth knowing about, but it is not
		// worth failing and retrying: the file will not fix itself.
		d.log.WarnContext(ctx, "could not decode a model thumbnail for its palette",
			"content", payload.SHA256[:8], "error", err)
		return nil
	}

	return RecordImagePalette(ctx, d.db, payload.SHA256, img)
}

// RecordImagePalette extracts a palette from a decoded image and records it against every
// asset with this content hash — the JSON column the detail page reads and the indexed
// swatch rows `color:` search reads.
//
// Exported because two paths produce a model's picture and both must record it the same
// way: this job, for thumbnails already on disk, and the upload handler, for each new
// render as it arrives.
func RecordImagePalette(ctx context.Context, database *db.DB, sha256hex string, img image.Image) error {
	pal := palette.Extract(img, palette.Options{})
	if len(pal.Swatches) == 0 {
		return nil
	}

	encoded, err := json.Marshal(pal.Swatches)
	if err != nil {
		return fmt.Errorf("marshal palette: %w", err)
	}
	if _, err := database.Writer.ExecContext(ctx, `
		UPDATE assets SET palette_json = ?, palette_kind = ?, updated_at = unixepoch()
		WHERE sha256 = ?`, string(encoded), string(pal.Kind), sha256hex); err != nil {
		return fmt.Errorf("record palette for content %s: %w", sha256hex[:8], err)
	}
	return recordSwatchesTo(ctx, database, sha256hex, pal.Swatches)
}

// EnqueueModelPalettes queues a palette job for every model that has no swatches.
//
// Called at startup and after a scan, beside EnqueueStale. Cheap to call repeatedly: a
// model that has swatches is excluded by the query, and one whose thumbnail does not exist
// yet has its job return without writing anything — so this converges rather than looping.
// Bounded by the number of models, which is under a thousand here and would be the same
// order on any library this application is for.
func EnqueueModelPalettes(ctx context.Context, database *db.DB, q *jobs.Queue) (int, error) {
	rows, err := database.Reader.QueryContext(ctx, `
		SELECT DISTINCT a.sha256
		FROM assets a
		WHERE a.kind = 'model'
		  AND a.missing_since IS NULL
		  AND NOT EXISTS (SELECT 1 FROM asset_swatches s WHERE s.asset_id = a.id)`)
	if err != nil {
		return 0, fmt.Errorf("find models without a palette: %w", err)
	}
	defer rows.Close()

	var hashes []string
	for rows.Next() {
		var sha string
		if err := rows.Scan(&sha); err != nil {
			return 0, err
		}
		hashes = append(hashes, sha)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	enqueued := 0
	for _, sha := range hashes {
		_, err := q.Enqueue(ctx, PaletteJobType, PalettePayload{SHA256: sha}, jobs.EnqueueOptions{
			// Below thumbnails and below an interactive scan: a colour filter that gains
			// models an hour late is fine, a grid that renders late is not.
			Priority:  0,
			DedupeKey: PaletteJobType + ":" + sha,
		})
		if err != nil {
			if errors.Is(err, jobs.ErrDuplicate) {
				continue
			}
			return enqueued, fmt.Errorf("enqueue model palette for %s: %w", sha[:8], err)
		}
		enqueued++
	}
	return enqueued, nil
}
