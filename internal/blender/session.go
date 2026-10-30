package blender

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// A Blender that stays open.
//
// Starting Blender is most of what a conversion costs: measured on the NAS's N100, 2.9 s
// a file, nearly all of it Blender coming up, the FBX import and GLB export a fraction.
// So Convert keeps Blender open and hands it one file after another on stdin; it
// answers each on a line of its own among its usual output. Each conversion still
// starts from an empty factory scene, so one file cannot leak into the next.
//
// A Blender left idle for IdleTimeout exits, so a finished backlog gives its memory
// back, and one is replaced after MaxUses files, so whatever it accumulates stays
// bounded. One that crashes or overruns ConvertTimeout is killed, and the next file
// starts a fresh one. Callers converting at once each get their own.

// ConvertTimeout bounds one conversion so a pathological file cannot hang a worker
// forever. Blender is killed when it runs out.
const ConvertTimeout = 3 * time.Minute

var (
	// IdleTimeout is how long an open Blender waits for the next file before exiting.
	IdleTimeout = time.Minute
	// MaxUses is how many files one Blender converts before it is replaced.
	MaxUses = 500
	// startTimeout bounds Blender coming up.
	startTimeout = 2 * time.Minute
)

const (
	readyMark  = "@@ambar-ready@@"
	resultMark = "@@ambar-result@@ "
)

// sessionScript runs inside Blender: it reads one JSON request a line on stdin and
// answers every line with resultMark and {"ok", "error", …}. The answer starts on a new
// line, as Blender's own output does not always end with one.
//
//   - convert {"src", "dest"}: import the source (FBX or .blend) into an empty scene and
//     export a single GLB. A request without "op" is a convert.
//   - open {"src"}: import a model into an empty scene and leave it there, answering how
//     many meshes it has, "refs", the image paths it names that are not where it says, and
//     "bare", how many of its meshes have UVs and no material at all — an OBJ without its
//     .mtl.
//   - thumb {"dest", "size", "textures", "atlas"}: draw the scene the last open left as a
//     square PNG, with each ref in "textures" pointed at the file found for it and the bare
//     meshes textured with "atlas". A ref with no file is taken out of its materials, which
//     then show their own colour rather than Blender's missing-texture magenta;
//     "untextured" counts them.
//
// A scene with nothing to draw — an FBX holding only an animation — answers "nothing"
// with the reason, which is an outcome rather than an error.
const sessionScript = `
import bpy, sys, os, json, math, traceback
from mathutils import Vector

class NothingToDraw(Exception):
    pass

def load(src):
    ext = os.path.splitext(src)[1].lower()
    bpy.ops.wm.read_factory_settings(use_empty=True)
    if ext == ".blend":
        bpy.ops.wm.open_mainfile(filepath=src)
    elif ext == ".fbx":
        bpy.ops.import_scene.fbx(filepath=src)
    elif ext == ".obj":
        bpy.ops.wm.obj_import(filepath=src)
    elif ext in (".gltf", ".glb"):
        bpy.ops.import_scene.gltf(filepath=src)
    else:
        raise ValueError("not a model Blender reads here: " + ext)

def convert(req):
    load(req["src"])
    bpy.ops.export_scene.gltf(filepath=req["dest"], export_format='GLB')
    return {}

def meshes():
    return [o for o in bpy.context.scene.objects if o.type == 'MESH' and o.visible_get()]

def missing_images():
    out = []
    for im in bpy.data.images:
        if im.source != 'FILE' or im.packed_file is not None:
            continue
        if not os.path.isfile(bpy.path.abspath(im.filepath)):
            out.append(im)
    return out

def bare(objects):
    return [o for o in objects if o.data.uv_layers and not any(slot.material for slot in o.material_slots)]

def open_model(req):
    load(req["src"])
    drawn = meshes()
    return {"meshes": len(drawn), "bare": len(bare(drawn)),
            "refs": sorted({im.filepath for im in missing_images()})}

def dress(objects, path):
    mat = bpy.data.materials.new("ambar-atlas")
    mat.use_nodes = True
    nodes = mat.node_tree.nodes
    image = nodes.new('ShaderNodeTexImage')
    image.image = bpy.data.images.load(path, check_existing=True)
    mat.node_tree.links.new(image.outputs['Color'], nodes['Principled BSDF'].inputs['Base Color'])
    nodes.active = image
    for o in objects:
        o.data.materials.clear()
        o.data.materials.append(mat)

def colour_node(mat):
    # The image texture feeding the shader's base colour, which is the one Workbench
    # should show: it draws the active image node, and an importer can leave a normal
    # map active.
    tree = mat.node_tree
    for node in tree.nodes:
        if node.type == 'BSDF_PRINCIPLED':
            for link in node.inputs['Base Color'].links:
                if link.from_node.type == 'TEX_IMAGE':
                    return link.from_node
    for node in tree.nodes:
        if node.type == 'TEX_IMAGE' and node.image is not None:
            return node
    return None

def thumb(req):
    scene = bpy.context.scene
    textures = req.get("textures") or {}
    lost = set()
    for im in missing_images():
        found = textures.get(im.filepath)
        if found:
            im.filepath = found
            im.reload()
        else:
            lost.add(im.name)
    for mat in bpy.data.materials:
        if not mat.use_nodes or mat.node_tree is None:
            continue
        nodes = mat.node_tree.nodes
        for node in list(nodes):
            if node.type == 'TEX_IMAGE' and node.image is not None and node.image.name in lost:
                nodes.remove(node)
        node = colour_node(mat)
        if node is not None:
            nodes.active = node

    drawn = meshes()
    if not drawn:
        raise NothingToDraw("no geometry to draw (an animation or a rig on its own?)")
    if req.get("atlas"):
        dress(bare(drawn), req["atlas"])
    corners = [o.matrix_world @ Vector(c) for o in drawn for c in o.bound_box]
    lo = Vector((min(c.x for c in corners), min(c.y for c in corners), min(c.z for c in corners)))
    hi = Vector((max(c.x for c in corners), max(c.y for c in corners), max(c.z for c in corners)))
    centre = (lo + hi) / 2
    radius = (hi - lo).length / 2 or 1.0

    # Framed as the browser renderer frames it (modelthumb.js): the bounding sphere fills a
    # 35-degree view from the front right, looking slightly down. Its (0.6, 0.5, 0.75) is
    # three.js's Y-up; Blender is Z-up with the front towards -Y.
    cam = bpy.data.objects.new("ambar-camera", bpy.data.cameras.new("ambar-camera"))
    scene.collection.objects.link(cam)
    scene.camera = cam
    cam.data.sensor_fit = 'VERTICAL'
    cam.data.angle = math.radians(35)
    distance = radius / math.sin(cam.data.angle / 2)
    cam.location = centre + Vector((0.6, -0.75, 0.5)) * distance
    cam.rotation_euler = (centre - cam.location).to_track_quat('-Z', 'Y').to_euler()
    cam.data.clip_start = max(distance / 100, 0.001)
    cam.data.clip_end = distance * 10

    # Workbench: fast, needs no lights, and draws without a GPU too. Studio light "paint"
    # with the Standard view and a little exposure keeps a texture's colours close to what
    # they are; Blender's default view (AgX) and studio light darken them.
    scene.render.engine = 'BLENDER_WORKBENCH'
    shading = scene.display.shading
    shading.light = 'STUDIO'
    try:
        shading.studio_light = 'paint.sl'
    except TypeError:
        pass
    shading.color_type = 'TEXTURE'
    shading.show_cavity = True
    shading.cavity_type = 'WORLD'
    scene.display.render_aa = '8'
    scene.view_settings.view_transform = 'Standard'
    scene.view_settings.look = 'None'
    scene.view_settings.exposure = 0.8
    scene.render.film_transparent = True
    size = int(req.get("size") or 512)
    scene.render.resolution_x = size
    scene.render.resolution_y = size
    scene.render.resolution_percentage = 100
    scene.render.image_settings.file_format = 'PNG'
    scene.render.image_settings.color_mode = 'RGBA'
    scene.render.image_settings.color_depth = '8'
    scene.render.filepath = req["dest"]
    bpy.ops.render.render(write_still=True)
    return {"untextured": len(lost)}

ops = {"convert": convert, "open": open_model, "thumb": thumb}

print("\n` + readyMark + `", flush=True)
while True:
    line = sys.stdin.readline()
    if not line:
        break
    try:
        req = json.loads(line)
        out = ops[req.get("op") or "convert"](req)
        out["ok"] = True
    except NothingToDraw as e:
        out = {"ok": False, "nothing": str(e)}
    except Exception as e:
        out = {"ok": False, "error": "".join(traceback.format_exception_only(type(e), e)).strip()}
    print("\n` + resultMark + `" + json.dumps(out), flush=True)
`

// Convert imports src (FBX/.blend) through Blender and writes a normalised GLB to
// dest. It fails if Blender reports an error, exits, or produces no output.
func Convert(ctx context.Context, bin, src, dest string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	s, err := acquire(ctx, bin, filepath.Dir(dest))
	if err != nil {
		return err
	}
	_, err = s.call(ctx, request{Op: "convert", Src: src, Dest: dest}, filepath.Base(src))
	pool.put(s)
	if err != nil {
		return err
	}
	if info, statErr := os.Stat(dest); statErr != nil || info.Size() == 0 {
		return fmt.Errorf("blender produced no output for %s\n%s", filepath.Base(src), s.output())
	}
	return nil
}

// ErrNothingToDraw means a model imported but has no geometry to draw: an FBX holding an
// animation or a rig and nothing else. Not a failure, and not one a retry would change.
var ErrNothingToDraw = errors.New("nothing to draw")

// ThumbInfo is what drawing a model found.
type ThumbInfo struct {
	// Textures is how many images the model named that were not where it said they were;
	// Untextured is how many of those nothing was found for, drawn without.
	Textures   int
	Untextured int
	// Atlas is whether meshes with no material were drawn with the product's atlas.
	Atlas bool
}

// Textures finds, for Thumbnail, the files a model's textures meant.
type Textures interface {
	// Resolve answers a texture the model names but is not where it says with the file it
	// meant, or "" when there is none.
	Resolve(ref string) string
	// Atlas is the texture for meshes that have UVs and no material at all, or "".
	Atlas() string
}

// Thumbnail draws src — FBX, OBJ, glTF/GLB or .blend — as a size×size PNG with a
// transparent background at dest, its textures found through find.
func Thumbnail(ctx context.Context, bin, src, dest string, size int, find Textures) (ThumbInfo, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	s, err := acquire(ctx, bin, filepath.Dir(dest))
	if err != nil {
		return ThumbInfo{}, err
	}
	defer pool.put(s)

	name := filepath.Base(src)
	opened, err := s.call(ctx, request{Op: "open", Src: src}, name)
	if err != nil {
		return ThumbInfo{}, err
	}
	req := request{Op: "thumb", Dest: dest, Size: size, Textures: map[string]string{}}
	for _, ref := range opened.Refs {
		if found := find.Resolve(ref); found != "" {
			req.Textures[ref] = found
		}
	}
	if opened.Bare > 0 {
		req.Atlas = find.Atlas()
	}
	drawn, err := s.call(ctx, req, name)
	if err != nil {
		return ThumbInfo{}, err
	}
	if info, statErr := os.Stat(dest); statErr != nil || info.Size() == 0 {
		return ThumbInfo{}, fmt.Errorf("blender drew nothing for %s\n%s", name, s.output())
	}
	return ThumbInfo{Textures: len(opened.Refs), Untextured: drawn.Untextured, Atlas: req.Atlas != ""}, nil
}

// acquire returns an open Blender for bin, starting one when none is idle.
func acquire(ctx context.Context, bin, dir string) (*session, error) {
	if s := pool.take(bin); s != nil {
		return s, nil
	}
	return startSession(ctx, bin, dir)
}

// request is one line to the session script.
type request struct {
	Op       string            `json:"op"`
	Src      string            `json:"src,omitempty"`
	Dest     string            `json:"dest,omitempty"`
	Size     int               `json:"size,omitempty"`
	Textures map[string]string `json:"textures,omitempty"`
	Atlas    string            `json:"atlas,omitempty"`
}

// answer is the session script's reply to one request.
type answer struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
	// Nothing is why a thumb found nothing to draw.
	Nothing    string   `json:"nothing"`
	Meshes     int      `json:"meshes"`
	Bare       int      `json:"bare"`
	Refs       []string `json:"refs"`
	Untextured int      `json:"untextured"`
}

// sessions holds the open Blenders waiting for a file, by executable.
type sessions struct {
	mu   sync.Mutex
	idle map[string][]*session
}

var pool = &sessions{idle: map[string][]*session{}}

// take returns an open, idle Blender for bin, or nil.
func (p *sessions) take(bin string) *session {
	p.mu.Lock()
	defer p.mu.Unlock()
	for list := p.idle[bin]; len(list) > 0; list = p.idle[bin] {
		s := list[len(list)-1]
		p.idle[bin] = list[:len(list)-1]
		s.timer.Stop()
		if s.alive() {
			return s
		}
	}
	return nil
}

// put keeps s for the next file, or closes it when it has died or done its share.
func (p *sessions) put(s *session) {
	if !s.alive() || s.uses >= MaxUses {
		s.close()
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.idle[s.bin] = append(p.idle[s.bin], s)
	s.timer = time.AfterFunc(IdleTimeout, func() {
		p.mu.Lock()
		list := p.idle[s.bin]
		for i, o := range list {
			if o == s {
				p.idle[s.bin] = append(list[:i:i], list[i+1:]...)
				p.mu.Unlock()
				s.close()
				return
			}
		}
		p.mu.Unlock() // taken again in the meantime
	})
}

// session is one Blender, open in --background, converting a file at a time.
type session struct {
	bin   string
	cmd   *exec.Cmd
	stdin io.WriteCloser
	// results carries what follows resultMark, one conversion's answer each.
	results chan string
	// exited is closed once the process is gone.
	exited chan struct{}
	uses   int
	timer  *time.Timer

	mu   sync.Mutex
	tail []string // Blender's last lines of output, for an error message
}

// startSession starts Blender with the session script and waits until it is ready.
// The script goes in dir rather than the system temp directory: a sandboxed Blender —
// the flatpak, on a desktop running `ambar worker` — has a /tmp of its own and cannot
// see the caller's, but it must be able to write the GLB into dir, so it can read it.
func startSession(ctx context.Context, bin, dir string) (*session, error) {
	scriptFile, err := os.CreateTemp(dir, "ambar-blender-*.py")
	if err != nil {
		return nil, fmt.Errorf("blender: temp script: %w", err)
	}
	defer os.Remove(scriptFile.Name()) // Blender has read it once it is ready
	if _, err := scriptFile.WriteString(sessionScript); err != nil {
		scriptFile.Close()
		return nil, err
	}
	scriptFile.Close()

	// Not CommandContext: the Blender outlives the conversion that started it.
	cmd := exec.Command(bin, "--background", "--factory-startup", "--python", scriptFile.Name())
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("blender: start %s: %w", bin, err)
	}

	s := &session{bin: bin, cmd: cmd, stdin: stdin,
		results: make(chan string, 1), exited: make(chan struct{})}
	ready := make(chan struct{})
	go s.read(out, ready)

	timer := time.NewTimer(startTimeout)
	defer timer.Stop()
	select {
	case <-ready:
		return s, nil
	case <-s.exited:
		return nil, fmt.Errorf("blender exited before it was ready\n%s", s.output())
	case <-timer.C:
		s.kill()
		return nil, fmt.Errorf("blender was not ready after %s\n%s", startTimeout, s.output())
	case <-ctx.Done():
		s.kill()
		return nil, ctx.Err()
	}
}

// read sorts Blender's output into the ready signal, answers and the rest, until it
// exits.
func (s *session) read(out io.Reader, ready chan struct{}) {
	r := bufio.NewReader(out)
	for {
		line, err := r.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		switch i := strings.Index(line, resultMark); {
		case i >= 0:
			select {
			case s.results <- line[i+len(resultMark):]:
			default: // nobody asked: a Blender killed with an answer on its way
			}
		case strings.Contains(line, readyMark):
			close(ready)
		case line != "":
			s.mu.Lock()
			s.tail = append(s.tail, line)
			if len(s.tail) > 20 {
				s.tail = s.tail[len(s.tail)-20:]
			}
			s.mu.Unlock()
		}
		if err != nil {
			break
		}
	}
	_ = s.cmd.Wait()
	close(s.exited)
}

// call hands Blender one request and waits for its answer. name is the file it concerns,
// for an error message.
func (s *session) call(ctx context.Context, req request, name string) (answer, error) {
	s.uses++
	s.mu.Lock()
	s.tail = nil
	s.mu.Unlock()

	var line bytes.Buffer
	enc := json.NewEncoder(&line)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(req); err != nil {
		return answer{}, err
	}
	if _, err := s.stdin.Write(line.Bytes()); err != nil {
		s.kill()
		return answer{}, fmt.Errorf("blender is gone: %w\n%s", err, s.output())
	}

	timer := time.NewTimer(ConvertTimeout)
	defer timer.Stop()
	select {
	case raw := <-s.results:
		var res answer
		if err := json.Unmarshal([]byte(raw), &res); err != nil {
			return answer{}, fmt.Errorf("blender answered %q: %w", raw, err)
		}
		switch {
		case res.OK:
			return res, nil
		case res.Nothing != "":
			return res, fmt.Errorf("%w: %s", ErrNothingToDraw, res.Nothing)
		default:
			return res, fmt.Errorf("blender %s failed: %s\n%s", req.Op, res.Error, s.output())
		}
	case <-s.exited:
		return answer{}, fmt.Errorf("blender exited while working on %s\n%s", name, s.output())
	case <-timer.C:
		s.kill()
		return answer{}, fmt.Errorf("blender took more than %s over %s", ConvertTimeout, name)
	case <-ctx.Done():
		s.kill()
		return answer{}, ctx.Err()
	}
}

func (s *session) alive() bool {
	select {
	case <-s.exited:
		return false
	default:
		return true
	}
}

// output is Blender's last lines, for an error message.
func (s *session) output() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := strings.Join(s.tail, "\n")
	if len(out) > 800 {
		out = "…" + out[len(out)-800:]
	}
	return out
}

// close ends the script's loop and lets Blender exit, killing it if it does not.
func (s *session) close() {
	_ = s.stdin.Close()
	go func() {
		select {
		case <-s.exited:
		case <-time.After(10 * time.Second):
			s.kill()
		}
	}()
}

func (s *session) kill() {
	_ = s.cmd.Process.Kill()
}
