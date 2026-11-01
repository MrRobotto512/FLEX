package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Season prep: movies and episodes for the time of year are upscaled ahead
// of time, one at a time while nobody is watching, into <data>/upscaled as
// 1080p H.264 files the PS5 plays directly. The AI's first row (the one for
// the holiday or season) picks them: what it lists is prepared, what it no
// longer lists is removed once the season is over; the owner can add and
// remove titles too, and those stay until removed. Topaz Video AI's ffmpeg
// does the upscaling when its folder is set and works on this PC, else the
// AI shaders FLEX already has.

const (
	prepStateQueued  = "queued"
	prepStateWorking = "working"
	prepStateReady   = "ready"
	prepStateFailed  = "failed"
	prepCheck        = time.Hour
	prepDefaultMax   = 12
)

var prepFirst = 3 * time.Minute // before the first look; FLEX_PREP_SOON shortens it for trying out

// PrepItem is one title in the season prep list.
type PrepItem struct {
	Kind    string    `json:"kind"` // "movie" | "episode"
	ID      int64     `json:"id"`
	Title   string    `json:"title"`
	State   string    `json:"state"`
	Source  string    `json:"source"` // "ai" | "user"
	Engine  string    `json:"engine,omitempty"`
	Error   string    `json:"error,omitempty"`
	Added   time.Time `json:"added"`
	Percent int       `json:"percent,omitempty"` // while working
	file    string
}

type prepper struct {
	s      *Server
	engine string // "topaz" | "shaders" | ""
	why    string // when engine is "": what's missing

	mu      sync.Mutex
	working *PrepItem
	percent int
	cancel  context.CancelFunc
}

func newPrepper(s *Server) *prepper {
	if os.Getenv("FLEX_PREP_SOON") != "" {
		prepFirst = 5 * time.Second
	}
	return &prepper{s: s}
}

func (p *prepper) dir() string { return filepath.Join(p.s.cfg.DataDir, "upscaled") }

// Engine says what prepares titles, for the window and /server.
func (p *prepper) Engine() (engine, why string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.engine, p.why
}

// detect picks the engine: Topaz when its ffmpeg runs the upscaling filter
// here, else the shaders when they passed, else nothing.
func (p *prepper) detect(ctx context.Context) {
	engine, why := "", "no AI upscaler on this PC"
	if dir := p.s.cfg.TopazDir; dir != "" {
		if err := topazTest(ctx, dir, p.s.cfg.TopazModel); err == nil {
			engine = "topaz"
		} else {
			why = "Topaz: " + err.Error()
			p.s.log.Warn("season prep: Topaz not usable", "dir", dir, "err", err)
		}
	}
	if engine == "" && p.s.tc.Shader(false) != "" {
		engine = "shaders"
	}
	p.mu.Lock()
	p.engine, p.why = engine, why
	p.mu.Unlock()
	p.s.log.Info("season prep", "engine", engine, "why", why)
}

// topazFFmpeg is Topaz Video AI's own ffmpeg (the only one with tvai_up).
func topazFFmpeg(dir string) string { return filepath.Join(dir, exeName("ffmpeg")) }

// topazEnv points Topaz's ffmpeg at its models, as the GUI would.
func topazEnv() []string {
	env := os.Environ()
	models := os.Getenv("TVAI_MODEL_DIR")
	if models == "" {
		models = filepath.Join(os.Getenv("ProgramData"), "Topaz Labs LLC", "Topaz Video AI", "models")
		env = append(env, "TVAI_MODEL_DIR="+models)
	}
	if os.Getenv("TVAI_MODEL_DATA_DIR") == "" {
		env = append(env, "TVAI_MODEL_DATA_DIR="+models)
	}
	return env
}

func topazFilter(model string, w, h int) string {
	return fmt.Sprintf("tvai_up=model=%s:scale=0:w=%d:h=%d:device=0:vram=1:instances=1", model, w, h)
}

// topazTest upscales half a second of a test picture with Topaz's ffmpeg.
func topazTest(ctx context.Context, dir, model string) error {
	ff := topazFFmpeg(dir)
	if _, err := os.Stat(ff); err != nil {
		return fmt.Errorf("no ffmpeg.exe in %s", dir)
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	cmd := noWindow(exec.CommandContext(ctx, ff, "-hide_banner", "-loglevel", "error", "-nostdin", "-y",
		"-f", "lavfi", "-i", "testsrc2=s=640x480:r=24:d=0.5", "-vf", topazFilter(model, 1440, 1080), "-f", "null", "-"))
	cmd.Env = topazEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		if msg := ffmpegError(string(out)); msg != "" {
			return errors.New(msg)
		}
		return err
	}
	return nil
}

// run keeps the list in step with the AI's seasonal row and works the queue.
func (p *prepper) run(ctx context.Context) {
	timer := time.NewTimer(prepFirst) // after the shaders' test and the first scan
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if p.engine == "" && p.s.tc.Shader(false) != "" {
			p.detect(ctx)
		}
		p.pick()
		p.work(ctx)
		timer.Reset(prepCheck)
	}
}

// pick queues what the AI's seasonal row lists (this server's own SD titles)
// and drops AI picks it no longer lists.
func (p *prepper) pick() {
	if !p.s.cfg.PrepEnabled {
		return
	}
	th := p.s.themes.current()
	var order []string // the row's order: the AI leads with the best fits
	wanted := map[string]bool{}
	if th != nil && len(th.Rows) > 0 {
		if name, _, _ := season(time.Now()); name != "" { // only in a season; the first row is its row
			order = th.Rows[0].Items
			for _, id := range order {
				wanted[id] = true
			}
		}
	}
	items, _ := p.s.db.PrepList()
	have := map[string]bool{}
	for _, it := range items {
		have[it.Kind+fmt.Sprint(it.ID)] = true
		if it.Source == "ai" && !wanted[p.s.db.PrepKey(it.Kind, it.ID)] {
			p.remove(it)
		}
	}
	max := p.s.cfg.PrepMax
	if max <= 0 {
		max = prepDefaultMax
	}
	n := len(items)
	for _, key := range order {
		if n >= max {
			break
		}
		kind, id, ok := p.s.db.PrepLookup(key)
		if !ok || have[kind+fmt.Sprint(id)] || !p.worth(kind, id) {
			continue
		}
		if p.s.db.PrepAdd(kind, id, "ai") == nil {
			n++
		}
	}
}

// worth: this server's own file, below HD.
func (p *prepper) worth(kind string, id int64) bool {
	if id >= peerStride {
		return false
	}
	path, mi := p.s.db.itemMedia(kind, id)
	return path != "" && mi != nil && mi.Video.Height > 0 && mi.Video.Height < 1080*9/10
}

// Add puts a title on the list by hand; it stays until removed.
func (p *prepper) Add(kind string, id int64) error {
	if !p.worth(kind, id) {
		return errors.New("only this PC's own movies and episodes below HD can be prepared")
	}
	return p.s.db.PrepAdd(kind, id, "user")
}

func (p *prepper) Remove(kind string, id int64) {
	items, _ := p.s.db.PrepList()
	for _, it := range items {
		if it.Kind == kind && it.ID == id {
			p.remove(it)
		}
	}
}

func (p *prepper) remove(it PrepItem) {
	p.mu.Lock()
	if p.working != nil && p.working.Kind == it.Kind && p.working.ID == it.ID && p.cancel != nil {
		p.cancel()
	}
	p.mu.Unlock()
	if it.file != "" {
		os.Remove(it.file)
	}
	p.s.db.PrepDelete(it.Kind, it.ID)
	p.s.log.Info("season prep: removed", "kind", it.Kind, "id", it.ID)
}

// work prepares queued titles one at a time, while nobody is watching.
func (p *prepper) work(ctx context.Context) {
	for ctx.Err() == nil {
		if len(p.s.tc.Sessions()) > 0 || p.engine == "" {
			return
		}
		items, _ := p.s.db.PrepList()
		var next *PrepItem
		for i := range items {
			if items[i].State == prepStateQueued {
				next = &items[i]
				break
			}
		}
		if next == nil {
			return
		}
		p.prepare(ctx, *next)
	}
}

func (p *prepper) prepare(ctx context.Context, it PrepItem) {
	path, mi := p.s.db.itemMedia(it.Kind, it.ID)
	if path == "" || mi == nil {
		p.s.db.PrepDelete(it.Kind, it.ID)
		return
	}
	os.MkdirAll(p.dir(), 0o755)
	out := filepath.Join(p.dir(), fmt.Sprintf("%s%d.mp4", it.Kind, it.ID))
	tmp := out + ".part.mp4"
	cctx, cancel := context.WithCancel(ctx)
	p.mu.Lock()
	p.working, p.cancel, p.percent = &it, cancel, 0
	p.mu.Unlock()
	p.s.db.PrepState(it.Kind, it.ID, prepStateWorking, "", p.engine, "")
	p.s.log.Info("season prep: upscaling", "title", it.Title, "engine", p.engine)
	err := p.upscale(cctx, it.Kind, it.ID, path, mi, tmp)
	cancel()
	p.mu.Lock()
	p.working, p.cancel = nil, nil
	p.mu.Unlock()
	if err != nil {
		os.Remove(tmp)
		if ctx.Err() != nil || cctx.Err() != nil {
			return
		}
		p.s.log.Warn("season prep: failed", "title", it.Title, "err", err)
		p.s.db.PrepState(it.Kind, it.ID, prepStateFailed, "", p.engine, err.Error())
		return
	}
	if err := os.Rename(tmp, out); err != nil {
		p.s.db.PrepState(it.Kind, it.ID, prepStateFailed, "", p.engine, err.Error())
		return
	}
	p.s.db.PrepState(it.Kind, it.ID, prepStateReady, out, p.engine, "")
	p.s.log.Info("season prep: ready", "title", it.Title, "file", out)
}

// upscale writes an HD H.264 + AAC MP4 of path to out.
func (p *prepper) upscale(ctx context.Context, kind string, id int64, path string, mi *MediaInfo, out string) error {
	w := mi.Video.Width * 1080 / mi.Video.Height / 2 * 2
	ff := p.s.cfg.FFmpeg
	var env []string
	var vf, hw []string
	switch p.engine {
	case "topaz":
		ff, env = topazFFmpeg(p.s.cfg.TopazDir), topazEnv()
		hw = []string{"-hwaccel", "auto"}
		vf = []string{"bwdif=mode=send_frame:deint=interlaced", topazFilter(p.s.cfg.TopazModel, w, 1080)}
	default:
		shader := p.s.tc.Shader(p.s.db.Animated(kind, id))
		hw = []string{"-init_hw_device", "vulkan=vk", "-filter_hw_device", "vk", "-hwaccel", "auto"}
		vf = []string{"bwdif=mode=send_frame:deint=interlaced", aiFilter(shader, w, 1080, "yuv420p")}
	}
	enc := []string{"-c:v", "libx264", "-preset", "medium", "-crf", "17"}
	if p.s.tc.Encoder() == "h264_nvenc" && p.engine != "topaz" {
		enc = []string{"-c:v", "h264_nvenc", "-preset", "p6", "-rc", "vbr", "-cq", "19", "-b:v", "0", "-maxrate", "20M", "-bufsize", "40M"}
	}
	args := append([]string{"-hide_banner", "-loglevel", "error", "-nostdin", "-y", "-progress", "pipe:1"}, hw...)
	args = append(args, "-i", path, "-map", "0:v:0", "-map", "0:a:0?", "-vf", strings.Join(vf, ","))
	args = append(args, enc...)
	args = append(args, "-profile:v", "high", "-level", "4.1", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-b:a", "256k", "-ac", "2", "-movflags", "+faststart", out)
	cmd := noWindow(exec.CommandContext(ctx, ff, args...))
	if env != nil {
		cmd.Env = env
	}
	stdout, _ := cmd.StdoutPipe()
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	go p.progress(stdout, mi.DurationS)
	if err := cmd.Wait(); err != nil {
		if msg := ffmpegError(stderr.String()); msg != "" {
			return errors.New(msg)
		}
		return err
	}
	return nil
}

// progress reads ffmpeg's -progress lines for the percentage shown.
func (p *prepper) progress(r interface{ Read([]byte) (int, error) }, duration float64) {
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		for _, line := range strings.Split(string(buf[:n]), "\n") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(line), "out_time_us="); ok && duration > 0 {
				var us int64
				fmt.Sscan(v, &us)
				p.mu.Lock()
				p.percent = min(99, int(float64(us)/1e6/duration*100))
				p.mu.Unlock()
			}
		}
		if err != nil {
			return
		}
	}
}

// List is the prep list for the window and the API.
func (p *prepper) List() []PrepItem {
	items, _ := p.s.db.PrepList()
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range items {
		if p.working != nil && items[i].Kind == p.working.Kind && items[i].ID == p.working.ID {
			items[i].Percent = p.percent
		}
	}
	return items
}

// Ready is the prepared HD file for a title, "" when there is none.
func (p *prepper) Ready(kind string, id int64) string {
	for _, it := range p.List() {
		if it.Kind == kind && it.ID == id && it.State == prepStateReady {
			if _, err := os.Stat(it.file); err == nil {
				return it.file
			}
		}
	}
	return ""
}
