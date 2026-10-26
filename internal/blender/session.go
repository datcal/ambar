package blender

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
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

// sessionScript runs inside Blender: it reads {"src", "dest"} lines on stdin, imports
// each source (FBX or .blend) into an empty scene and exports a single GLB, and answers
// every line with resultMark and {"ok", "error"}. The answer starts on a new line, as
// Blender's own output does not always end with one.
const sessionScript = `
import bpy, sys, os, json, traceback

def convert(src, dest):
    ext = os.path.splitext(src)[1].lower()
    bpy.ops.wm.read_factory_settings(use_empty=True)
    if ext == ".blend":
        bpy.ops.wm.open_mainfile(filepath=src)
    elif ext == ".fbx":
        bpy.ops.import_scene.fbx(filepath=src)
    else:
        raise ValueError("not an FBX or .blend: " + ext)
    bpy.ops.export_scene.gltf(filepath=dest, export_format='GLB')

print("\n` + readyMark + `", flush=True)
while True:
    line = sys.stdin.readline()
    if not line:
        break
    try:
        req = json.loads(line)
        convert(req["src"], req["dest"])
        out = {"ok": True}
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
	s := pool.take(bin)
	if s == nil {
		var err error
		if s, err = startSession(ctx, bin, filepath.Dir(dest)); err != nil {
			return err
		}
	}
	err := s.convert(ctx, src, dest)
	pool.put(s)
	if err != nil {
		return err
	}
	if info, statErr := os.Stat(dest); statErr != nil || info.Size() == 0 {
		return fmt.Errorf("blender produced no output for %s\n%s", filepath.Base(src), s.output())
	}
	return nil
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

// convert hands Blender one file and waits for its answer.
func (s *session) convert(ctx context.Context, src, dest string) error {
	s.uses++
	s.mu.Lock()
	s.tail = nil
	s.mu.Unlock()

	var req bytes.Buffer
	enc := json.NewEncoder(&req)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(map[string]string{"src": src, "dest": dest}); err != nil {
		return err
	}
	if _, err := s.stdin.Write(req.Bytes()); err != nil {
		s.kill()
		return fmt.Errorf("blender is gone: %w\n%s", err, s.output())
	}

	timer := time.NewTimer(ConvertTimeout)
	defer timer.Stop()
	select {
	case raw := <-s.results:
		var res struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		}
		if err := json.Unmarshal([]byte(raw), &res); err != nil {
			return fmt.Errorf("blender answered %q: %w", raw, err)
		}
		if !res.OK {
			return fmt.Errorf("blender convert failed: %s\n%s", res.Error, s.output())
		}
		return nil
	case <-s.exited:
		return fmt.Errorf("blender exited while converting %s\n%s", filepath.Base(src), s.output())
	case <-timer.C:
		s.kill()
		return fmt.Errorf("blender took more than %s over %s", ConvertTimeout, filepath.Base(src))
	case <-ctx.Done():
		s.kill()
		return ctx.Err()
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
