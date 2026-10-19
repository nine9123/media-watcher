// Package convert turns files the browser cannot play into a cached MP4,
// in the background.
//
// A file that needs a CPU transcode costs a full encode on every play. The
// first time one is played it is queued here instead: one ffmpeg at a time,
// at low priority, writes an 8-bit H.264 MP4 at the source resolution with
// one audio track (see playback.ConvertedArgs) into CACHE_DIR/converted.
// Once that exists the player uses it as a direct-play file, and it can be
// downloaded to replace the original.
//
//   - One job per item. Opening the same file again, from any browser, gets
//     the job that is already queued or running, never a second one.
//   - Copies not played for ConvertCacheDays are deleted, and the least
//     recently played go first when the cache is over ConvertCacheGB.
//     The file's mtime is its last-played time.
//   - A copy whose source file changed (size or mtime) is dropped.
//   - Queued jobs survive a restart (queue.json); a job that was running
//     starts over, ffmpeg cannot resume a half-written MP4.
package convert

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nine9123/media-watcher/internal/config"
	"github.com/nine9123/media-watcher/internal/logx"
	"github.com/nine9123/media-watcher/internal/naming"
	"github.com/nine9123/media-watcher/internal/playback"
	"github.com/nine9123/media-watcher/internal/probe"
	"github.com/nine9123/media-watcher/internal/shutdown"
)

var log = logx.New("mediawatcher.convert")

// CleanupEvery is the time between expiry sweeps while idle.
const CleanupEvery = time.Hour

const GB = 1 << 30

// Source is what a job needs to know about the original file.
type Source struct {
	Path      string         `json:"path"`
	Container string         `json:"container"`
	Duration  *float64       `json:"duration"`
	Streams   []probe.Stream `json:"streams"`
}

func (s *Source) media() *playback.Media {
	return &playback.Media{Path: s.Path, Container: s.Container, Duration: s.Duration, Streams: s.Streams}
}

type job struct {
	id, title, file string // file: original file name, for display
	item            Source
	state           string // queued | running | failed
	queuedAt        float64
	startedAt       float64
	doneS           float64  // seconds of the source encoded so far
	speed           *float64 // ffmpeg's realtime factor
	err             *string
	proc            *os.Process
	cancelled       bool
}

func (j *job) duration() float64 {
	if j.item.Duration == nil {
		return 0
	}
	return *j.item.Duration
}

// Status is a job (queued, running, failed) or a finished copy (done).
type Status struct {
	ID    string
	Title string
	File  string
	State string
	// jobs
	Error    *string
	Progress float64
	ETA      *float64
	Speed    *float64
	Elapsed  *float64
	Position *int
	// done
	Size       int64
	UsedAt     float64
	CreatedAt  *float64
	ExpiresAt  float64
	AudioIndex *int
}

func (s Status) MarshalJSON() ([]byte, error) {
	if s.State == "done" {
		return json.Marshal(map[string]any{
			"id": s.ID, "title": s.Title, "file": s.File, "state": "done", "size": s.Size,
			"used_at": s.UsedAt, "created_at": s.CreatedAt, "expires_at": s.ExpiresAt,
			"audio_index": s.AudioIndex, "url": "/converted/" + s.ID, "download": "/converted/" + s.ID + "?download=1",
		})
	}
	return json.Marshal(map[string]any{
		"id": s.ID, "title": s.Title, "file": s.File, "state": s.State, "error": s.Error,
		"progress": s.Progress, "eta": s.ETA, "speed": s.Speed, "elapsed": s.Elapsed, "position": s.Position,
	})
}

// Meta is CACHE_DIR/converted/<id>.json, next to <id>.mp4.
type Meta struct {
	Title       string   `json:"title"`
	Name        string   `json:"name"`
	Source      string   `json:"source"`
	SourceSize  int64    `json:"source_size"`
	SourceMtime int64    `json:"source_mtime"`
	Audio       []int    `json:"audio"`
	CreatedAt   *float64 `json:"created_at"`
	size        int64
	usedAt      float64
}

type Converter struct {
	cfg       *config.Config
	Dir       string
	mu        sync.Mutex
	jobs      map[string]*job // queued, running and failed; done ones are files
	queue     []string
	wake      chan struct{}
	started   bool
	noWorker  bool     // tests: keep jobs queued
	lastSpeed *float64 // for estimating queued jobs
}

func New(cfg *config.Config) *Converter {
	return &Converter{cfg: cfg, Dir: filepath.Join(cfg.CacheDir, "converted"), jobs: map[string]*job{},
		wake: make(chan struct{}, 1)}
}

func now() float64 { return float64(time.Now().UnixNano()) / 1e9 }

func (c *Converter) maxBytes() int64       { return int64(c.cfg.ConvertCacheGB * GB) }
func (c *Converter) maxAge() float64       { return c.cfg.ConvertCacheDays * 86400 }
func (c *Converter) Path(id string) string { return filepath.Join(c.Dir, id+".mp4") }
func (c *Converter) metaPath(id string) string {
	return filepath.Join(c.Dir, id+".json")
}

func (c *Converter) remove(id string) {
	os.Remove(c.Path(id))
	os.Remove(c.metaPath(id))
}

func mtime(st os.FileInfo) float64 { return float64(st.ModTime().UnixNano()) / 1e9 }

// Cached is the metadata of a finished copy, or nil. With source, a copy
// made from a different version of that file is deleted and nil returned.
func (c *Converter) Cached(id, source string) *Meta {
	raw, err := os.ReadFile(c.metaPath(id))
	if err != nil {
		return nil
	}
	var m Meta
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	st, err := os.Stat(c.Path(id))
	if err != nil {
		return nil
	}
	if source != "" {
		src, err := os.Stat(source)
		if err != nil {
			return nil // share not mounted: keep the copy, just don't use it
		}
		if src.Size() != m.SourceSize || src.ModTime().Unix() != m.SourceMtime {
			log.Infof("dropping converted copy of %s: the file changed", source)
			c.remove(id)
			return nil
		}
	}
	m.size, m.usedAt = st.Size(), mtime(st)
	return &m
}

// Touch marks a copy as played now: it expires ConvertCacheDays from here.
func (c *Converter) Touch(id string) {
	t := time.Now()
	os.Chtimes(c.Path(id), t, t)
}

// ---- jobs ----------------------------------------------------------------------

// Request is the status of the copy of item, queueing a conversion if there is none.
//
// A failed job is only tried again with retry, so a file ffmpeg cannot
// convert is not re-encoded every time someone opens it.
func (c *Converter) Request(id string, item Source, title string, retry bool) Status {
	c.mu.Lock()
	if j := c.jobs[id]; j != nil && (j.state != "failed" || !retry) {
		defer c.mu.Unlock()
		return c.jobStatus(j)
	}
	c.mu.Unlock()
	if m := c.Cached(id, item.Path); m != nil {
		return c.cachedStatus(id, m)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if j := c.jobs[id]; j != nil && j.state != "failed" { // a second request may have got here first
		return c.jobStatus(j)
	}
	j := &job{id: id, title: title, file: filepath.Base(item.Path), item: item, state: "queued", queuedAt: now()}
	c.jobs[id] = j
	c.queue = append(c.queue, id)
	c.saveQueue()
	c.ensureWorker()
	c.notify()
	log.Infof("queued conversion of %s", item.Path)
	return c.jobStatus(j)
}

func (c *Converter) notify() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// Status of the job or copy of id, nil if there is neither.
func (c *Converter) Status(id, source string) *Status {
	c.mu.Lock()
	if j := c.jobs[id]; j != nil {
		defer c.mu.Unlock()
		s := c.jobStatus(j)
		return &s
	}
	c.mu.Unlock()
	if m := c.Cached(id, source); m != nil {
		s := c.cachedStatus(id, m)
		return &s
	}
	return nil
}

func (c *Converter) Cancel(id string) bool {
	c.mu.Lock()
	j := c.jobs[id]
	if j == nil {
		c.mu.Unlock()
		return false
	}
	delete(c.jobs, id)
	j.cancelled = true
	c.dequeue(id)
	proc := j.proc
	c.saveQueue()
	c.mu.Unlock()
	if proc != nil {
		proc.Kill()
	}
	log.Infof("cancelled conversion of %s", j.file)
	return true
}

func (c *Converter) dequeue(id string) {
	for i, q := range c.queue {
		if q == id {
			c.queue = append(c.queue[:i:i], c.queue[i+1:]...)
			return
		}
	}
}

// Delete cancels any job and deletes the copy.
func (c *Converter) Delete(id string) {
	c.Cancel(id)
	c.remove(id)
}

// Summary is small enough for every status poll: what is converting right now.
type Summary struct {
	Queued  int     `json:"queued"`
	Running *Status `json:"running"`
	Files   int     `json:"files"`
}

func (c *Converter) Summary() Summary {
	var out Summary
	c.mu.Lock()
	for _, j := range c.jobs {
		if j.state == "queued" {
			out.Queued++
		}
	}
	if r := c.running(); r != nil {
		s := c.jobStatus(r)
		out.Running = &s
	}
	c.mu.Unlock()
	entries, _ := os.ReadDir(c.Dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".mp4") {
			out.Files++
		}
	}
	return out
}

type Overview struct {
	Jobs  []Status `json:"jobs"`
	Files []Status `json:"files"`
	Used  int64    `json:"used"`
	Max   int64    `json:"max"`
	Days  float64  `json:"days"`
	Auto  bool     `json:"auto"`
}

func (c *Converter) Overview() Overview {
	out := Overview{Jobs: []Status{}, Files: []Status{}, Max: c.maxBytes(), Days: c.cfg.ConvertCacheDays,
		Auto: c.cfg.AutoConvert}
	c.mu.Lock()
	jobs := make([]*job, 0, len(c.jobs))
	for _, j := range c.jobs {
		jobs = append(jobs, j)
	}
	rank := map[string]int{"running": 0, "queued": 1, "failed": 2}
	pos := map[string]int{}
	for i, id := range c.queue {
		pos[id] = i
	}
	sort.Slice(jobs, func(a, b int) bool {
		ja, jb := jobs[a], jobs[b]
		if rank[ja.state] != rank[jb.state] {
			return rank[ja.state] < rank[jb.state]
		}
		if pos[ja.id] != pos[jb.id] {
			return pos[ja.id] < pos[jb.id]
		}
		return ja.queuedAt < jb.queuedAt
	})
	for _, j := range jobs {
		out.Jobs = append(out.Jobs, c.jobStatus(j))
	}
	c.mu.Unlock()
	for _, f := range c.allCached() {
		out.Files = append(out.Files, c.cachedStatus(f.id, f.meta))
		out.Used += f.meta.size
	}
	sort.SliceStable(out.Files, func(a, b int) bool { return out.Files[a].UsedAt > out.Files[b].UsedAt })
	return out
}

// ---- status ----------------------------------------------------------------------

func (c *Converter) running() *job {
	for _, j := range c.jobs {
		if j.state == "running" {
			return j
		}
	}
	return nil
}

func fp(f float64) *float64 { return &f }

// jobStatus: caller holds c.mu.
func (c *Converter) jobStatus(j *job) Status {
	out := Status{ID: j.id, Title: j.title, File: j.file, State: j.state, Error: j.err, Speed: j.speed}
	switch j.state {
	case "running":
		elapsed := now() - j.startedAt
		p := 0.0
		if d := j.duration(); d > 0 {
			p = min(1, j.doneS/d)
		}
		out.Progress, out.Elapsed, out.ETA = p, &elapsed, c.remaining(j, elapsed, p)
	case "queued":
		pos := 0
		for i, id := range c.queue {
			if id == j.id {
				pos = i
				break
			}
		}
		n := pos + 1
		out.Position = &n
		// Rough: what is left of the running job, then each queued one at
		// the speed the last job ran at (or the running one runs at).
		wait := fp(0)
		r := c.running()
		if r != nil {
			relapsed := now() - r.startedAt
			p := 0.0
			if d := r.duration(); d > 0 {
				p = min(1, r.doneS/d)
			}
			wait = c.remaining(r, relapsed, p)
		}
		speed := c.lastSpeed
		if speed == nil && r != nil {
			speed = r.speed
		}
		for _, other := range c.queue[:min(pos+1, len(c.queue))] {
			d := c.jobs[other].duration()
			if wait == nil || speed == nil || *speed == 0 || d == 0 {
				wait = nil
				break
			}
			wait = fp(*wait + d / *speed)
		}
		out.ETA = wait
	}
	return out
}

func (c *Converter) remaining(j *job, elapsed, p float64) *float64 {
	if p >= 0.01 && elapsed >= 5 {
		return fp(max(0, elapsed*(1-p)/p))
	}
	if d := j.duration(); j.speed != nil && *j.speed != 0 && d > 0 {
		return fp(max(0, (d-j.doneS) / *j.speed))
	}
	return nil
}

func (c *Converter) cachedStatus(id string, m *Meta) Status {
	s := Status{ID: id, Title: m.Title, File: m.Name, State: "done", Size: m.size, UsedAt: m.usedAt,
		CreatedAt: m.CreatedAt, ExpiresAt: m.usedAt + c.maxAge()}
	if len(m.Audio) > 0 {
		a := m.Audio[0]
		s.AudioIndex = &a
	}
	return s
}

// ---- worker ----------------------------------------------------------------------

type savedJob struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	File  string `json:"file"`
	Item  Source `json:"item"`
}

// Start picks up jobs queued before a restart and starts the worker.
func (c *Converter) Start() {
	os.MkdirAll(c.Dir, 0o755)
	entries, _ := os.ReadDir(c.Dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".part") { // from a conversion cut off by a restart
			os.Remove(filepath.Join(c.Dir, e.Name()))
		}
	}
	var saved []savedJob
	if raw, err := os.ReadFile(filepath.Join(c.Dir, "queue.json")); err == nil {
		json.Unmarshal(raw, &saved)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range saved {
		if c.jobs[s.ID] == nil {
			c.jobs[s.ID] = &job{id: s.ID, title: s.Title, file: s.File, item: s.Item, state: "queued", queuedAt: now()}
			c.queue = append(c.queue, s.ID)
		}
	}
	if len(c.queue) > 0 {
		log.Infof("resuming %d queued conversion(s)", len(c.queue))
	}
	c.ensureWorker()
}

// ensureWorker: caller holds c.mu.
func (c *Converter) ensureWorker() {
	if c.started || c.noWorker {
		return
	}
	c.started = true
	go c.loop()
}

// saveQueue: caller holds c.mu. Running first, so it starts first after a restart.
func (c *Converter) saveQueue() {
	var pending []*job
	for _, j := range c.jobs {
		if j.state == "queued" || j.state == "running" {
			pending = append(pending, j)
		}
	}
	pos := map[string]int{}
	for i, id := range c.queue {
		pos[id] = i
	}
	sort.SliceStable(pending, func(a, b int) bool {
		ra, rb := pending[a].state != "running", pending[b].state != "running"
		if ra != rb {
			return !ra
		}
		if pending[a].queuedAt != pending[b].queuedAt {
			return pending[a].queuedAt < pending[b].queuedAt
		}
		return pos[pending[a].id] < pos[pending[b].id]
	})
	data := []savedJob{}
	for _, j := range pending {
		data = append(data, savedJob{j.id, j.title, j.file, j.item})
	}
	raw, _ := json.Marshal(data)
	if err := os.MkdirAll(c.Dir, 0o755); err == nil {
		tmp := filepath.Join(c.Dir, "queue.json.tmp")
		if err = os.WriteFile(tmp, raw, 0o644); err == nil {
			err = os.Rename(tmp, filepath.Join(c.Dir, "queue.json"))
		}
		if err != nil {
			log.Warnf("cannot save conversion queue: %v", err)
		}
	}
}

func (c *Converter) loop() {
	c.Cleanup()
	for !shutdown.Stopping() {
		c.mu.Lock()
		empty := len(c.queue) == 0
		c.mu.Unlock()
		if empty {
			select {
			case <-c.wake:
			case <-time.After(CleanupEvery):
			case <-shutdown.Context().Done():
				return
			}
		}
		c.mu.Lock()
		var j *job
		for len(c.queue) > 0 && j == nil {
			j = c.jobs[c.queue[0]]
			c.queue = c.queue[1:]
		}
		if j != nil {
			j.state, j.startedAt, j.err = "running", now(), nil
		}
		c.mu.Unlock()
		if j == nil {
			c.Cleanup()
			continue
		}
		c.run(j)
		c.Cleanup()
	}
}

func (c *Converter) fail(j *job, msg string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	j.proc = nil
	if c.jobs[j.id] == j {
		j.state, j.err = "failed", &msg
	}
	c.saveQueue()
}

func (c *Converter) finish(j *job) {
	c.mu.Lock()
	defer c.mu.Unlock()
	j.proc = nil
	if c.jobs[j.id] == j {
		delete(c.jobs, j.id)
	}
	c.saveQueue()
}

// ring keeps the last lines written to it.
type ring struct {
	mu    sync.Mutex
	lines []string
}

func (r *ring) add(s string) {
	r.mu.Lock()
	r.lines = append(r.lines, s)
	if len(r.lines) > 20 {
		r.lines = r.lines[1:]
	}
	r.mu.Unlock()
}

func (r *ring) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.lines, "\n")
}

func (c *Converter) run(j *job) {
	src := j.item.Path
	st, err := os.Stat(src)
	if err != nil {
		c.fail(j, "cannot read file: "+errText(err))
		return
	}
	os.MkdirAll(c.Dir, 0o755)
	c.makeRoom(min(st.Size(), c.maxBytes()), j.id)
	part := c.Path(j.id) + ".part"
	args, err := playback.ConvertedArgs(c.cfg, j.item.media(), c.cfg.AudioLanguages, part)
	if err != nil {
		c.fail(j, err.Error())
		return
	}
	if nice, err := exec.LookPath("nice"); err == nil {
		args = append([]string{nice, "-n", "10"}, args...) // live streams come first
	}
	log.Infof("converting %s", src)
	cmd := exec.CommandContext(shutdown.Context(), args[0], args[1:]...)
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		c.fail(j, "cannot start ffmpeg: "+err.Error())
		return
	}
	c.mu.Lock()
	j.proc = cmd.Process
	cancelled := j.cancelled
	c.mu.Unlock()
	if cancelled {
		cmd.Process.Kill()
	}
	var errs ring
	drained := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			errs.add(sc.Text())
		}
		close(drained)
	}()
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		key, value, _ := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		switch key {
		case "out_time_us":
			if n, err := strconv.ParseInt(value, 10, 64); err == nil {
				c.mu.Lock()
				j.doneS = max(0, float64(n)/1e6)
				c.mu.Unlock()
			}
		case "speed":
			if f, err := strconv.ParseFloat(strings.TrimSuffix(value, "x"), 64); err == nil && strings.HasSuffix(value, "x") {
				c.mu.Lock()
				if f != 0 {
					j.speed = &f
				} else {
					j.speed = nil
				}
				c.mu.Unlock()
			}
		}
	}
	<-drained
	werr := cmd.Wait()
	c.mu.Lock()
	cancelled = j.cancelled
	c.mu.Unlock()
	if cancelled || shutdown.Stopping() {
		os.Remove(part)
		if !cancelled {
			return // stays in queue.json, starts again after the restart
		}
		c.finish(j)
		return
	}
	if werr != nil {
		os.Remove(part)
		msg := strings.TrimSpace(errs.String())
		if msg == "" {
			msg = fmt.Sprintf("ffmpeg exited with %v", werr)
		}
		log.Warnf("conversion of %s failed: %s", src, msg)
		if len(msg) > 500 {
			msg = msg[len(msg)-500:]
		}
		c.fail(j, msg)
		return
	}
	st2, err := os.Stat(src)
	if err != nil || st2.Size() != st.Size() || st2.ModTime().Unix() != st.ModTime().Unix() {
		os.Remove(part)
		c.fail(j, "the file changed while it was being converted")
		return
	}
	pst, err := os.Stat(part)
	if err != nil {
		c.fail(j, "ffmpeg wrote no file")
		return
	}
	if size := pst.Size(); size > c.maxBytes() {
		os.Remove(part)
		c.fail(j, fmt.Sprintf("the converted file (%.1f GB) is larger than the whole conversion cache (%s GB)",
			float64(size)/GB, strconv.FormatFloat(c.cfg.ConvertCacheGB, 'g', -1, 64)))
		return
	}
	elapsed := now() - j.startedAt
	if d := j.duration(); d > 0 && elapsed > 0 {
		c.mu.Lock()
		c.lastSpeed = fp(d / elapsed)
		c.mu.Unlock()
	}
	if err := os.Rename(part, c.Path(j.id)); err != nil {
		c.fail(j, err.Error())
		return
	}
	stem, _ := naming.SplitExt(j.file)
	m := Meta{Title: j.title, Name: stem + ".mp4", Source: src, SourceSize: st.Size(),
		SourceMtime: st.ModTime().Unix(), Audio: []int{}, CreatedAt: fp(now())}
	if a := playback.ConvertedAudio(j.item.Streams, c.cfg.AudioLanguages); a != nil {
		m.Audio = []int{a.Index}
	}
	raw, _ := json.Marshal(m)
	tmp := c.metaPath(j.id) + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err == nil {
		os.Rename(tmp, c.metaPath(j.id))
	}
	log.Infof("converted %s in %.0fs (%.1f MB)", src, elapsed, float64(pst.Size())/1e6)
	c.finish(j)
}

func errText(err error) string {
	if pe, ok := err.(*os.PathError); ok {
		return pe.Err.Error()
	}
	return err.Error()
}

// ---- cache limits ----------------------------------------------------------------

type cachedFile struct {
	id   string
	meta *Meta
}

func (c *Converter) allCached() []cachedFile {
	entries, err := os.ReadDir(c.Dir)
	if err != nil {
		return nil
	}
	var out []cachedFile
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".mp4") {
			continue
		}
		id := strings.TrimSuffix(name, ".mp4")
		if m := c.Cached(id, ""); m != nil {
			out = append(out, cachedFile{id, m})
		} else if _, err := os.Stat(c.metaPath(id)); err != nil {
			os.Remove(filepath.Join(c.Dir, name)) // copy without metadata: unusable
		}
	}
	return out
}

// Cleanup deletes expired copies, then the least recently played ones over the limit.
func (c *Converter) Cleanup() {
	t := now()
	for _, f := range c.allCached() {
		if t-f.meta.usedAt > c.maxAge() {
			log.Infof("converted copy of %s expired", f.meta.Source)
			c.remove(f.id)
		}
	}
	c.makeRoom(0, "")
}

func (c *Converter) makeRoom(need int64, keep string) {
	files := c.allCached()
	sort.SliceStable(files, func(a, b int) bool { return files[a].meta.usedAt < files[b].meta.usedAt })
	var total int64
	for _, f := range files {
		total += f.meta.size
	}
	for _, f := range files {
		if total+need <= c.maxBytes() {
			break
		}
		if f.id == keep {
			continue
		}
		log.Infof("conversion cache full: dropping copy of %s", f.meta.Source)
		c.remove(f.id)
		total -= f.meta.size
	}
}
