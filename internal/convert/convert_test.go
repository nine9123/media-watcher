package convert

// One job per file, the cache limits. The real conversion through the HTTP
// server is in package server (TestMPEG2IsConvertedOnceThenPlayedDirectly).

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nine9123/media-watcher/internal/config"
	"github.com/nine9123/media-watcher/internal/probe"
)

func sp(s string) *string { return &s }
func ip(i int) *int       { return &i }

var streams = []probe.Stream{
	{Index: 0, Type: "video", Codec: "mpeg2video", PixFmt: sp("yuv420p"), Height: ip(360)},
	{Index: 1, Type: "audio", Codec: "ac3", Language: "jpn", Channels: ip(2)},
}

func conv(t *testing.T, dir string, mod ...func(*config.Config)) *Converter {
	cfg := config.Defaults()
	cfg.APIKey = "secret-key-123"
	cfg.CacheDir = filepath.Join(dir, "cache")
	cfg.FFmpeg = "/nonexistent/ffmpeg"
	for _, m := range mod {
		m(&cfg)
	}
	return New(&cfg)
}

func source(t *testing.T, dir, name string) Source {
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, make([]byte, 100), 0o644); err != nil {
		t.Fatal(err)
	}
	d := 60.0
	return Source{Path: p, Container: "matroska,webm", Duration: &d, Streams: streams}
}

// fakeCopy is a finished copy, as run leaves it.
func fakeCopy(t *testing.T, c *Converter, id string, item Source, size int64, usedAt float64) {
	os.MkdirAll(c.Dir, 0o755)
	if err := os.WriteFile(c.Path(id), make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(item.Path)
	raw, _ := json.Marshal(map[string]any{"title": id, "name": "a.mp4", "source": item.Path,
		"source_size": st.Size(), "source_mtime": st.ModTime().Unix(), "audio": []int{1}, "created_at": usedAt})
	os.WriteFile(filepath.Join(c.Dir, id+".json"), raw, 0o644)
	tm := time.Unix(0, int64(usedAt*1e9))
	os.Chtimes(c.Path(id), tm, tm)
}

func TestSameFileIsQueuedOnce(t *testing.T) {
	dir := t.TempDir()
	c := conv(t, dir)
	c.noWorker = true // keep the jobs queued
	it := source(t, dir, "a.mkv")
	first := c.Request("a1", it, "A", false)
	again := c.Request("a1", it, "A", false)
	if first.State != "queued" || again.State != "queued" || *first.Position != 1 {
		t.Error(first, again)
	}
	if len(c.queue) != 1 || len(c.jobs) != 1 {
		t.Error(c.queue)
	}
	if other := c.Request("b2", source(t, dir, "b.mkv"), "B", false); *other.Position != 2 {
		t.Error(other)
	}
	var saved []savedJob
	raw, _ := os.ReadFile(filepath.Join(c.Dir, "queue.json"))
	json.Unmarshal(raw, &saved)
	if len(saved) != 2 || saved[0].ID != "a1" || saved[1].ID != "b2" {
		t.Error(saved)
	}

	// After a restart the queue is picked up again, still one job per file.
	c2 := conv(t, dir)
	c2.noWorker = true
	c2.Start()
	if len(c2.queue) != 2 || c2.queue[0] != "a1" || c2.queue[1] != "b2" {
		t.Error(c2.queue)
	}
	c2.Request("a1", it, "A", false)
	if len(c2.queue) != 2 {
		t.Error(c2.queue)
	}
	if !c2.Cancel("a1") || len(c2.queue) != 1 || c2.queue[0] != "b2" || c2.jobs["a1"] != nil {
		t.Error(c2.queue)
	}
}

func TestFailedJobIsOnlyRetriedOnRequest(t *testing.T) {
	dir := t.TempDir()
	c := conv(t, dir) // ffmpeg is missing, so every job fails
	it := source(t, dir, "a.mkv")
	c.Request("a1", it, "A", false)
	deadline := time.Now().Add(5 * time.Second)
	for c.Status("a1", "").State != "failed" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	st := c.Status("a1", "")
	if st.State != "failed" || st.Error == nil {
		t.Fatal(st)
	}
	if s := c.Request("a1", it, "A", false); s.State != "failed" { // opening it again does not re-run
		t.Error(s)
	}
	if s := c.Request("a1", it, "A", true); s.State != "queued" && s.State != "running" && s.State != "failed" {
		t.Error(s)
	}
	if c.Summary().Queued > 1 {
		t.Error(c.Summary())
	}
}

func TestCachedCopyIsUsedUntilTheSourceChanges(t *testing.T) {
	dir := t.TempDir()
	c := conv(t, dir)
	it := source(t, dir, "a.mkv")
	fakeCopy(t, c, "a1", it, 10, now())
	st := c.Request("a1", it, "A", false)
	if st.State != "done" || *st.AudioIndex != 1 || len(c.jobs) != 0 {
		t.Error(st)
	}
	raw, _ := json.Marshal(st)
	var m map[string]any
	json.Unmarshal(raw, &m)
	if m["url"] != "/converted/a1" || m["download"] != "/converted/a1?download=1" || m["size"] != 10.0 {
		t.Error(m)
	}
	f, _ := os.OpenFile(it.Path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("more") // replaced or re-encoded by hand
	f.Close()
	if c.Status("a1", it.Path) != nil {
		t.Error("stale copy used")
	}
	if _, err := os.Stat(c.Path("a1")); err == nil {
		t.Error("stale copy kept")
	}
}

func TestExpiredAndLeastRecentlyPlayedCopiesAreDropped(t *testing.T) {
	dir := t.TempDir()
	c := conv(t, dir, func(cfg *config.Config) { cfg.ConvertCacheDays, cfg.ConvertCacheGB = 14, 25.0/GB }) // 25 bytes
	n := now()
	it := source(t, dir, "a.mkv")
	fakeCopy(t, c, "old", it, 5, n-15*86400)
	fakeCopy(t, c, "lru", it, 10, n-3600)
	fakeCopy(t, c, "new", it, 10, n-60)
	fakeCopy(t, c, "newer", it, 10, n)
	c.Cleanup()
	ids := func() []string {
		var out []string
		for _, f := range c.Overview().Files {
			out = append(out, f.ID)
		}
		return out
	}
	if got := ids(); len(got) != 2 || got[0] != "newer" || got[1] != "new" {
		t.Error(got)
	}
	c.Touch("new") // played again: now the most recent
	c.makeRoom(10, "")
	if got := ids(); len(got) != 1 || got[0] != "new" {
		t.Error(got)
	}
}

func TestRestoredJobWithAFinishedCopyIsNotRedone(t *testing.T) {
	// A restart between writing the copy and saving the queue.
	dir := t.TempDir()
	c := conv(t, dir)
	c.noWorker = true
	it := source(t, dir, "a.mkv")
	fakeCopy(t, c, "a1", it, 10, now())
	j := &job{id: "a1", file: "a.mkv", item: it, state: "running", startedAt: now()}
	c.jobs["a1"] = j
	c.run(j) // would fail: there is no ffmpeg
	if len(c.jobs) != 0 || j.err != nil {
		t.Errorf("jobs %v, err %v", c.jobs, j.err)
	}
	if c.Cached("a1", it.Path) == nil {
		t.Error("the copy is gone")
	}
}
