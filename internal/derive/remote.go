package derive

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/datcal/ambar/internal/db"
	"github.com/datcal/ambar/internal/jobs"
	"github.com/datcal/ambar/internal/safepath"
)

// Deriving on another machine (internal/remote).
//
// Derive has always been two halves that only meet in Handle: Generate decodes a file
// and writes derivative files, touching no database, and Finish records what it found,
// touching no file but the row. A remote worker is those halves on two machines — the
// server prepares a Task, the worker runs Generate against its own mount of the library
// into a scratch directory, and sends the Outcome and the files back for Accept to
// check, move into place and Finish.
//
// Nothing here is a second implementation. RunTask is Generate; Accept ends in the same
// Finish a local worker calls; OutcomeOf is the one place an error from Generate becomes
// a derive state. The worker does not know SQL and the server does not decode.

// Task is one asset's derive, as handed to a remote worker.
type Task struct {
	AssetID  int64  `json:"asset_id"`
	SHA256   string `json:"sha256"`
	Ext      string `json:"ext"`
	Filename string `json:"filename"`
	// LibraryPath is slash-separated and relative to the library root, which the worker
	// resolves against its own mount of the same library.
	LibraryPath string `json:"library_path"`
	// Size is what the last scan recorded, so a worker reading a file that has changed
	// since does not record derivatives for bytes the row does not describe.
	Size int64 `json:"size"`
	// MaxPixels is the server's limit, so a worker cannot be more permissive than the
	// server would have been.
	MaxPixels int64 `json:"max_pixels"`
	// DeriveVersion is the server's Version. A worker built from different code would
	// record its derivatives as current when they are not; the claim refuses it.
	DeriveVersion int `json:"derive_version"`
}

// Outcome is what deriving one asset produced, in a form that crosses the network.
type Outcome struct {
	// State is StateOK, StateNeedsBlender, StateUnsupported or StateFailed.
	State   string  `json:"state"`
	Message string  `json:"message,omitempty"`
	Result  *Result `json:"result,omitempty"`
}

// OutcomeOf turns Generate's return values into an Outcome — the one place the error
// kinds become derive states, for local and remote work alike.
func OutcomeOf(result *Result, err error) Outcome {
	switch {
	case errors.Is(err, ErrNeedsBlender):
		return Outcome{State: StateNeedsBlender, Message: err.Error()}
	case errors.Is(err, ErrUnsupported):
		return Outcome{State: StateUnsupported, Message: err.Error()}
	case err != nil:
		return Outcome{State: StateFailed, Message: err.Error()}
	case result == nil:
		return Outcome{State: StateFailed, Message: "the decoder returned no result"}
	default:
		return Outcome{State: StateOK, Message: strings.Join(result.Notes, "; "), Result: result}
	}
}

// RunTask derives a task on a worker: the file comes from libraryRoot, the worker's own
// mount of the library, and the derivatives are written under dataRoot, a scratch
// directory the caller uploads from and then removes.
func RunTask(task Task, libraryRoot, dataRoot, blenderBin string) Outcome {
	absPath, err := safepath.ResolveExisting(libraryRoot, task.LibraryPath)
	if err != nil {
		return Outcome{State: StateFailed,
			Message: fmt.Sprintf("the worker could not open %s under its library: %v", task.LibraryPath, err)}
	}
	info, err := os.Stat(absPath)
	if err != nil {
		return Outcome{State: StateFailed, Message: fmt.Sprintf("the worker could not stat the file: %v", err)}
	}
	if info.Size() != task.Size {
		return Outcome{State: StateFailed, Message: fmt.Sprintf(
			"the file is %d bytes but the last scan recorded %d; run `ambar scan`", info.Size(), task.Size)}
	}

	maxPixels := task.MaxPixels
	if maxPixels <= 0 {
		maxPixels = DefaultMaxPixels
	}
	result, err := Generate(GenerateOptions{
		AbsPath:    absPath,
		Ext:        task.Ext,
		SHA256:     task.SHA256,
		DataRoot:   dataRoot,
		MaxPixels:  maxPixels,
		BlenderBin: blenderBin,
	})
	return OutcomeOf(result, err)
}

// derivativeLimits is every file name a derive can write, with the most a server will
// accept for it from a worker. An allow-list: the names become paths under the data root,
// and anything a worker sends that is not one of these is refused.
var derivativeLimits = map[string]int64{
	FileThumb:        32 << 20,
	FileThumb2x:      32 << 20,
	FileThumbAlpha:   32 << 20,
	FilePreview:      64 << 20,
	FileAnimation:    256 << 20,
	FileSheet:        256 << 20,
	FilePeaks:        16 << 20,
	FileModelPreview: 1 << 30,
}

// DerivativeLimit reports whether name is a derivative file a worker may upload, and the
// largest it may be.
func DerivativeLimit(name string) (int64, bool) {
	limit, ok := derivativeLimits[name]
	return limit, ok
}

// Accept checks a remote worker's derivatives, moves them into place and records the
// outcome. staging holds the uploaded files under their own names and must be on the
// same filesystem as the data root, so each move is a rename; whatever is left in it
// afterwards is the caller's to remove.
//
// A file is only trusted as far as it can be checked cheaply: its name is on the
// allow-list, its size within the limit, and its content is what the name says — an
// image decodes, a GLB starts with its magic, peaks are JSON. The worker holds a write
// token, so this is not a defence against a hostile one; it is what keeps a confused one
// from putting something in front of a browser that is not what the row says it is.
func (d *Deriver) Accept(ctx context.Context, task Task, outcome Outcome, staging string) error {
	if outcome.State != StateOK {
		return d.Finish(ctx, task, outcome)
	}
	if outcome.Result == nil {
		return fmt.Errorf("a successful outcome for asset %d carried no result", task.AssetID)
	}

	relDir, err := Dir(task.SHA256)
	if err != nil {
		return err
	}
	for _, name := range outcome.Result.Files {
		if err := checkDerivative(staging, name); err != nil {
			return fmt.Errorf("asset %d: %w", task.AssetID, err)
		}
	}

	outDir := filepath.Join(d.dataRoot, relDir)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("create derivative directory: %w", err)
	}
	for _, name := range outcome.Result.Files {
		if err := os.Rename(filepath.Join(staging, name), filepath.Join(outDir, name)); err != nil {
			return fmt.Errorf("move %s into place: %w", name, err)
		}
	}
	return d.Finish(ctx, task, outcome)
}

// checkDerivative validates one uploaded file.
func checkDerivative(staging, name string) error {
	limit, ok := DerivativeLimit(name)
	if !ok {
		return fmt.Errorf("%q is not a derivative file this server writes", name)
	}
	path := filepath.Join(staging, name)
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("the result lists %s but it was not uploaded", name)
	}
	if info.Size() > limit {
		return fmt.Errorf("%s is %d bytes, over the %d limit", name, info.Size(), limit)
	}

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck

	switch name {
	case FileModelPreview:
		magic := make([]byte, 4)
		if _, err := io.ReadFull(f, magic); err != nil || !bytes.Equal(magic, []byte("glTF")) {
			return fmt.Errorf("%s is not a GLB file", name)
		}
	case FilePeaks:
		raw, err := io.ReadAll(f)
		if err != nil || !json.Valid(raw) {
			return fmt.Errorf("%s is not valid JSON", name)
		}
	default:
		// The image derivatives. DecodeConfig reads only the header, which is enough to
		// say the file is the format its name claims.
		_, format, err := image.DecodeConfig(f)
		if err != nil {
			return fmt.Errorf("%s does not decode as an image: %v", name, err)
		}
		if want := strings.TrimPrefix(filepath.Ext(name), "."); format != want {
			return fmt.Errorf("%s decodes as %s, not %s", name, format, want)
		}
	}
	return nil
}

// EnqueueNeedsBlender queues a derive for every present asset waiting on Blender.
//
// Called when a worker that has Blender connects. These rows are at the current derive
// version, so EnqueueStale rightly passes them over — but they are only waiting because
// this server has no Blender, and the worker does.
func EnqueueNeedsBlender(ctx context.Context, database *db.DB, q *jobs.Queue) (int, error) {
	rows, err := database.Reader.QueryContext(ctx, `
		SELECT sha256, min(id) FROM assets
		WHERE missing_since IS NULL AND derive_state = ?
		GROUP BY sha256
		ORDER BY min(id)`, StateNeedsBlender)
	if err != nil {
		return 0, fmt.Errorf("find assets waiting on Blender: %w", err)
	}
	type pending struct {
		sha256 string
		id     int64
	}
	var todo []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.sha256, &p.id); err != nil {
			rows.Close() //nolint:errcheck
			return 0, err
		}
		todo = append(todo, p)
	}
	rows.Close() //nolint:errcheck
	if err := rows.Err(); err != nil {
		return 0, err
	}

	enqueued := 0
	for _, p := range todo {
		_, err := q.Enqueue(ctx, JobType, Payload{AssetID: p.id}, jobs.EnqueueOptions{
			Priority:  0,
			DedupeKey: DedupeKey(p.sha256),
		})
		switch {
		case errors.Is(err, jobs.ErrDuplicate):
		case err != nil:
			return enqueued, err
		default:
			enqueued++
		}
	}
	return enqueued, nil
}
