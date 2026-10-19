package server

// Tests that run the real ffmpeg/ffprobe; skipped where they are not installed.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nine9123/media-watcher/internal/config"
)

func needsFFmpeg(t *testing.T) {
	for _, b := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(b); err != nil {
			t.Skip("ffmpeg/ffprobe not installed")
		}
	}
}

func ffmpeg(t *testing.T, args ...string) {
	t.Helper()
	out, err := exec.Command("ffmpeg", append([]string{"-v", "error", "-y"}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("ffmpeg: %v %s", err, out)
	}
}

func ffprobeStreams(t *testing.T, path string) []map[string]any {
	t.Helper()
	raw, err := exec.Command("ffprobe", "-v", "error", "-show_streams", "-of", "json", path).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", path, err)
	}
	var v struct {
		Streams []map[string]any `json:"streams"`
	}
	json.Unmarshal(raw, &v)
	return v.Streams
}

func realEnv(t *testing.T, mod func(*config.Config), lib config.Library) *env {
	cfg := testConfig(t.TempDir(), lib)
	cfg.FFmpeg, cfg.FFprobe = "ffmpeg", "ffprobe"
	if mod != nil {
		mod(cfg)
	}
	return start(t, cfg)
}

func TestMPEG2IsConvertedOnceThenPlayedDirectly(t *testing.T) {
	needsFFmpeg(t)
	movies := filepath.Join(t.TempDir(), "media", "Movies")
	src := filepath.Join(movies, "Old (1990)", "Old (1990).mkv")
	os.MkdirAll(filepath.Dir(src), 0o755)
	ffmpeg(t, "-f", "lavfi", "-i", "testsrc=size=320x180:rate=25",
		"-f", "lavfi", "-i", "sine=frequency=440", "-f", "lavfi", "-i", "sine=frequency=880",
		"-t", "4", "-map", "0", "-map", "1", "-map", "2", "-c:v", "mpeg2video", "-c:a", "ac3",
		"-metadata:s:a:0", "language=eng", "-metadata:s:a:1", "language=jpn", src)
	e := realEnv(t, func(c *config.Config) { c.MaxHeight = 120 }, // live transcode only
		config.Library{Name: "Movies", Path: movies, Type: "movies"})
	e.app.Converter.Start()

	mid := e.movieID()
	info := e.get("/api/items/" + mid + "/playback")
	same(t, info["mode"], "transcode") // still playable while the copy is made
	c := info["conversion"].(obj)
	if s := c["state"]; s != "queued" && s != "running" && s != "done" {
		t.Fatal(c)
	}
	same(t, c["title"], "Old (1990)")
	e.get("/api/items/" + mid + "/playback") // a second visit: no second job

	var st obj
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		st = e.get("/api/conversions/" + mid)["conversion"].(obj)
		if st["state"] != "queued" && st["state"] != "running" {
			break
		}
	}
	if st["state"] != "done" {
		t.Fatal(st)
	}
	same(t, e.get("/api/status")["converting"], obj{"queued": 0, "running": nil, "files": 1})

	info = e.get("/api/items/" + mid + "/playback")
	same(t, []any{info["mode"], info["url"], info["audio_index"]}, []any{"direct", "/converted/" + mid, 2}) // Japanese, the copy's only track
	// Another audio track than the copy's: back to the original.
	same(t, e.get("/api/items/" + mid + "/playback?audio=1")["mode"], "transcode")

	r, body := e.req("GET", "/converted/"+mid+"?download=1", nil, nil, true)
	if r.StatusCode != 200 || r.Header.Get("Accept-Ranges") != "bytes" ||
		!strings.Contains(r.Header.Get("Content-Disposition"), "Old%20%281990%29.mp4") {
		t.Fatal(r.StatusCode, r.Header)
	}
	out := filepath.Join(t.TempDir(), "out.mp4")
	os.WriteFile(out, body, 0o644)
	streams := ffprobeStreams(t, out)
	var got []any
	for _, s := range streams {
		lang, _ := s["tags"].(map[string]any)["language"]
		got = append(got, []any{s["codec_name"], lang, s["disposition"].(map[string]any)["default"]})
	}
	// Only the Japanese track, as AAC so a browser plays it.
	same(t, got, []any{[]any{"h264", "und", 1}, []any{"aac", "jpn", 1}})
	// Source resolution, not MAX_HEIGHT.
	same(t, []any{streams[0]["pix_fmt"], streams[0]["width"], streams[0]["height"]}, []any{"yuv420p", 320, 180})

	files := list(e.get("/api/conversions")["files"])
	same(t, []any{len(files), files[0]["id"], files[0]["size"]}, []any{1, mid, len(body)})
	if r, _ := e.req("POST", "/api/conversions/"+mid+"/delete", obj{}, nil, true); r.StatusCode != 200 {
		t.Error(r.StatusCode)
	}
	if r, _ := e.req("GET", "/converted/"+mid, nil, nil, true); r.StatusCode != 404 {
		t.Error(r.StatusCode)
	}
}

func TestRemuxStreamIsAFragmentedMP4(t *testing.T) {
	needsFFmpeg(t)
	movies := filepath.Join(t.TempDir(), "media", "Movies")
	src := filepath.Join(movies, "Film (2001).mkv")
	os.MkdirAll(movies, 0o755)
	ffmpeg(t, "-f", "lavfi", "-i", "testsrc=size=320x180:rate=25", "-f", "lavfi", "-i", "sine=frequency=440",
		"-t", "6", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-g", "25", "-c:a", "ac3", src)
	e := realEnv(t, nil, config.Library{Name: "Movies", Path: movies, Type: "movies"})
	mid := e.movieID()

	info := e.get("/api/items/" + mid + "/playback?t=2&sid=abc")
	same(t, []any{info["mode"], info["video"], info["audio"]}, []any{"remux", "copy", "aac"})
	if s := info["start"].(float64); s < 1.9 || s > 2.01 { // snapped to the keyframe at 2 s
		t.Error(s)
	}
	r, body := e.req("GET", info["url"].(string), nil, nil, true)
	if r.StatusCode != 200 || r.Header.Get("Content-Type") != "video/mp4" || r.Header.Get("Transfer-Encoding") != "" {
		t.Fatal(r.StatusCode, r.Header)
	}
	out := filepath.Join(t.TempDir(), "s.mp4")
	os.WriteFile(out, body, 0o644)
	var codecs []any
	for _, s := range ffprobeStreams(t, out) {
		codecs = append(codecs, s["codec_name"])
	}
	same(t, codecs, []any{"h264", "aac"})
	if !strings.Contains(string(body), "moof") {
		t.Error("not fragmented")
	}
	if r, _ := e.req("GET", info["url"].(string), nil, map[string]string{"Range": "bytes=100-"}, true); r.StatusCode != 416 {
		t.Error(r.StatusCode)
	}
	if r, _ := e.req("POST", "/api/streams/abc/stop", obj{}, nil, true); r.StatusCode != 200 {
		t.Error(r.StatusCode)
	}
}
