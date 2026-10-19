package server

// End-to-end tests against a real server on localhost.
//
// ffprobe/ffmpeg are pointed at a missing binary: the scan, browsing, auth,
// progress and byte-range paths must all work without them.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nine9123/media-watcher/internal/config"
	"github.com/nine9123/media-watcher/internal/convert"
	"github.com/nine9123/media-watcher/internal/images"
	"github.com/nine9123/media-watcher/internal/probe"
	"github.com/nine9123/media-watcher/internal/scan"
	"github.com/nine9123/media-watcher/internal/store"
)

const key = "secret-key-123"

func touch(t *testing.T, root, rel string, data []byte) string {
	t.Helper()
	if data == nil {
		data = make([]byte, 2048)
	}
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func heatData() []byte {
	b := make([]byte, 0, 256*40)
	for i := 0; i < 40; i++ {
		for c := 0; c < 256; c++ {
			b = append(b, byte(c))
		}
	}
	return b
}

type env struct {
	t       *testing.T
	cfg     *config.Config
	db      *store.Store
	images  *images.Images
	scanner *scan.Scanner
	app     *App
	srv     *httptest.Server
	shows   string
	movies  string
}

func testConfig(dir string, libs ...config.Library) *config.Config {
	cfg := config.Defaults()
	cfg.APIKey = key
	cfg.Libraries = libs
	cfg.CacheDir, cfg.ConfigDir = filepath.Join(dir, "cache"), filepath.Join(dir, "config")
	cfg.FFmpeg, cfg.FFprobe = "/nonexistent/ffmpeg", "/nonexistent/ffprobe"
	cfg.MetadataSources = nil
	return &cfg
}

func start(t *testing.T, cfg *config.Config) *env {
	t.Helper()
	db, err := store.Open(cfg.CacheDir, cfg.ConfigDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	im, err := images.New(cfg, db)
	if err != nil {
		t.Fatal(err)
	}
	sc := scan.New(cfg, db, im, nil)
	sc.Scan()
	app := New(cfg, db, im, sc, convert.New(cfg))
	srv := httptest.NewServer(app)
	t.Cleanup(srv.Close)
	return &env{t: t, cfg: cfg, db: db, images: im, scanner: sc, app: app, srv: srv}
}

func newEnv(t *testing.T) *env {
	dir := t.TempDir()
	shows, movies := filepath.Join(dir, "media", "Shows"), filepath.Join(dir, "media", "Movies")
	touch(t, shows, "Show (2020)/Season 01/Show - S01E01 - One.mkv", nil)
	touch(t, shows, "Show (2020)/Season 01/Show - S01E02 - Two.mkv", nil)
	touch(t, shows, "Show (2020)/Season 02/Show - S02E01.mkv", nil)
	touch(t, shows, "Show (2020)/Specials/Show - S00E01.mkv", nil)
	touch(t, shows, "Show (2020)/Season 01/Show - S01E01 - One-sample.mkv", nil)
	touch(t, shows, "Show (2020)/@eaDir/Show - S01E01.mkv", nil)
	touch(t, shows, "[Grp] Show - 03 [1080p].mkv", nil) // loose file joins "Show"
	touch(t, movies, "Heat (1995)/Heat (1995).mp4", heatData())
	cfg := testConfig(dir, config.Library{Name: "Shows", Path: shows, Type: "shows"},
		config.Library{Name: "Movies", Path: movies, Type: "movies"})
	e := start(t, cfg)
	e.shows, e.movies = shows, movies
	return e
}

func (e *env) req(method, path string, body any, headers map[string]string, withKey bool) (*http.Response, []byte) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	r, _ := http.NewRequest(method, e.srv.URL+path, rd)
	if withKey {
		r.Header.Set("X-Api-Key", key)
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	resp, err := http.DefaultTransport.RoundTrip(r)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp, raw
}

func (e *env) post(path string, body any) []byte {
	e.t.Helper()
	resp, raw := e.req("POST", path, body, nil, true)
	if resp.StatusCode != 200 {
		e.t.Fatalf("POST %s: %d %s", path, resp.StatusCode, raw)
	}
	return raw
}

type obj = map[string]any

func (e *env) get(path string) obj {
	e.t.Helper()
	resp, raw := e.req("GET", path, nil, nil, true)
	if resp.StatusCode != 200 {
		e.t.Fatalf("GET %s: %d %s", path, resp.StatusCode, raw)
	}
	var v obj
	if err := json.Unmarshal(raw, &v); err != nil {
		e.t.Fatal(err)
	}
	return v
}

func list(v any) []obj {
	var out []obj
	for _, x := range v.([]any) {
		out = append(out, x.(obj))
	}
	return out
}

func pluck(items []obj, keys ...string) []any {
	var out []any
	for _, it := range items {
		if len(keys) == 1 {
			out = append(out, it[keys[0]])
			continue
		}
		var row []any
		for _, k := range keys {
			row = append(row, it[k])
		}
		out = append(out, row)
	}
	return out
}

func same(t *testing.T, got, want any) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("got  %v\nwant %v", got, want)
	}
}

func (e *env) seriesID() string {
	return list(e.get("/api/libraries/Shows")["series"])[0]["id"].(string)
}
func (e *env) movieID() string {
	return list(e.get("/api/libraries/Movies")["movies"])[0]["id"].(string)
}

func TestAuth(t *testing.T) {
	e := newEnv(t)
	if r, _ := e.req("GET", "/api/home", nil, nil, false); r.StatusCode != 401 {
		t.Error(r.StatusCode)
	}
	if r, _ := e.req("GET", "/api/home", nil, map[string]string{"X-Api-Key": "wrong"}, false); r.StatusCode != 401 {
		t.Error(r.StatusCode)
	}
	if r, _ := e.req("GET", "/healthz", nil, nil, false); r.StatusCode != 200 {
		t.Error(r.StatusCode)
	}
	if r, body := e.req("GET", "/", nil, nil, false); r.StatusCode != 200 || !bytes.Contains(body, []byte("<html")) {
		t.Error(r.StatusCode) // UI shell itself is public, data is not
	}
	r, _ := e.req("POST", "/api/login", obj{"key": key}, nil, false)
	if r.StatusCode != 200 {
		t.Fatal(r.StatusCode)
	}
	cookie := strings.Split(r.Header.Get("Set-Cookie"), ";")[0]
	if strings.Contains(cookie, key) {
		t.Error("cookie holds the key")
	}
	// The same cookie the Python version handed out: HMAC-SHA256(key, "mediawatcher-session").
	if cookie != "mw_session=092bcb64b5495cf8d1a74b19aad05fd77c2215f718f342e258e6b3a7448320e8" {
		t.Error(cookie)
	}
	if r, _ := e.req("GET", "/api/home", nil, map[string]string{"Cookie": cookie}, false); r.StatusCode != 200 {
		t.Error(r.StatusCode)
	}
	if r, _ := e.req("GET", "/api/home?api_key="+key, nil, nil, false); r.StatusCode != 200 {
		t.Error(r.StatusCode)
	}
}

func TestLibraryGroupingAndCleanup(t *testing.T) {
	e := newEnv(t)
	lib := e.get("/api/libraries/Shows")
	series := list(lib["series"])
	same(t, pluck(series, "title"), []any{"Show"})
	same(t, series[0]["episodes"], 5) // sample + @eaDir skipped, loose file merged
	same(t, len(list(lib["movies"])), 0)
	movies := list(e.get("/api/libraries/Movies")["movies"])
	same(t, pluck(movies, "title", "year"), []any{[]any{"Heat", 1995}})

	s := e.get("/api/series/" + series[0]["id"].(string))
	eps := list(s["episodes"])
	same(t, pluck(eps, "season", "episode"), []any{[]any{1, 1}, []any{1, 2}, []any{1, 3}, []any{2, 1}, []any{0, 1}}) // specials last
	same(t, s["next_id"], eps[0]["id"])

	os.Remove(filepath.Join(e.shows, "Show (2020)/Season 02/Show - S02E01.mkv"))
	e.scanner.Scan()
	same(t, list(e.get("/api/libraries/Shows")["series"])[0]["episodes"], 4)
}

func TestEmptyShowFolderIsAMissingSeries(t *testing.T) {
	e := newEnv(t)
	os.MkdirAll(filepath.Join(e.shows, "Later Show (2024)", "Season 01"), 0o755)
	os.MkdirAll(filepath.Join(e.shows, "Show"), 0o755) // same title as an existing series
	os.MkdirAll(filepath.Join(e.movies, "Empty Film (2001)"), 0o755)
	e.scanner.Scan()
	lib := list(e.get("/api/libraries/Shows")["series"])
	same(t, pluck(lib, "title", "episodes"), []any{[]any{"Later Show", 0}, []any{"Show", 5}})
	same(t, len(list(e.get("/api/libraries/Movies")["series"])), 0)
	libs := map[string]any{}
	for _, l := range list(e.get("/api/home")["libraries"]) {
		libs[l["name"].(string)] = l["series"]
	}
	same(t, libs, map[string]any{"Shows": 2, "Movies": 0})
	sid := lib[0]["id"].(string)
	s := e.get("/api/series/" + sid)
	same(t, []any{s["title"], s["year"], len(list(s["episodes"]))}, []any{"Later Show", 2024, 0})

	// Episodes arrive: same series, now with an episode.
	touch(t, e.shows, "Later Show (2024)/Season 01/Later Show - S01E01.mkv", nil)
	e.scanner.Scan()
	lib = list(e.get("/api/libraries/Shows")["series"])
	same(t, []any{lib[0]["id"], lib[0]["episodes"]}, []any{sid, 1})

	// The folder is deleted: the series goes.
	os.RemoveAll(filepath.Join(e.shows, "Later Show (2024)"))
	e.scanner.Scan()
	same(t, pluck(list(e.get("/api/libraries/Shows")["series"]), "title"), []any{"Show"})

	os.MkdirAll(filepath.Join(e.shows, "Gone Soon"), 0o755)
	e.scanner.Scan()
	same(t, len(list(e.get("/api/libraries/Shows")["series"])), 2)
	os.Remove(filepath.Join(e.shows, "Gone Soon"))
	e.scanner.Scan()
	same(t, pluck(list(e.get("/api/libraries/Shows")["series"]), "title"), []any{"Show"})
}

func TestUnmountedShareDoesNotWipeLibrary(t *testing.T) {
	e := newEnv(t)
	filepath.Walk(e.movies, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			os.Remove(p)
		}
		return nil
	})
	e.scanner.Scan()
	same(t, len(list(e.get("/api/libraries/Movies")["movies"])), 1)
}

func TestProgressContinueAndNextUp(t *testing.T) {
	e := newEnv(t)
	sid := e.seriesID()
	eps := list(e.get("/api/series/" + sid)["episodes"])
	e1, e2 := eps[0]["id"].(string), eps[1]["id"].(string)

	same(t, string(e.post("/api/items/"+e1+"/progress", obj{"position": 300, "duration": 1440})), `{"watched":false}`)
	same(t, pluck(list(e.get("/api/home")["continue"]), "id"), []any{e1})

	same(t, string(e.post("/api/items/"+e1+"/progress", obj{"position": 1350, "duration": 1440})), `{"watched":true}`)
	home := e.get("/api/home")
	same(t, len(list(home["continue"])), 0)
	same(t, pluck(list(home["next_up"]), "id"), []any{e2})
	same(t, e.get("/api/series/" + sid)["next_id"], e2)

	e.post("/api/series/"+sid+"/watched", obj{"watched": true, "season": 1})
	same(t, pluck(list(e.get("/api/series/" + sid)["episodes"]), "watched"), []any{true, true, true, false, false})
}

func TestProgressSurvivesCacheWipe(t *testing.T) {
	e := newEnv(t)
	mid := e.movieID()
	e.post("/api/items/"+mid+"/progress", obj{"position": 600, "duration": 6000})
	db2, err := store.Open(filepath.Join(t.TempDir(), "cache2"), e.cfg.ConfigDir)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	db2.View(func(d *store.Data) { same(t, d.Progress[mid].Position, 600) })
}

func TestByteRanges(t *testing.T) {
	e := newEnv(t)
	mid := e.movieID()
	data := heatData()
	r, raw := e.req("GET", "/file/"+mid, nil, nil, true)
	if r.StatusCode != 200 || !bytes.Equal(raw, data) || r.Header.Get("Accept-Ranges") != "bytes" {
		t.Error(r.StatusCode, r.Header)
	}
	if r.Header.Get("Content-Type") != "video/mp4" {
		t.Error(r.Header.Get("Content-Type"))
	}
	r, raw = e.req("GET", "/file/"+mid, nil, map[string]string{"Range": "bytes=10-19"}, true)
	if r.StatusCode != 206 || !bytes.Equal(raw, data[10:20]) || r.Header.Get("Content-Range") != fmt.Sprintf("bytes 10-19/%d", len(data)) {
		t.Error(r.StatusCode, r.Header)
	}
	r, raw = e.req("GET", "/file/"+mid, nil, map[string]string{"Range": "bytes=-5"}, true)
	if r.StatusCode != 206 || !bytes.Equal(raw, data[len(data)-5:]) {
		t.Error(r.StatusCode)
	}
	r, _ = e.req("GET", "/file/"+mid, nil, map[string]string{"Range": fmt.Sprintf("bytes=%d-", len(data))}, true)
	if r.StatusCode != 416 {
		t.Error(r.StatusCode)
	}
	r, raw = e.req("GET", "/file/"+mid+"?download=1", nil, nil, true)
	if cd := r.Header.Get("Content-Disposition"); cd != "attachment; filename*=UTF-8''Heat%20%281995%29.mp4" {
		t.Error(cd)
	}
}

func TestKeepaliveSurvivesErrorWithBody(t *testing.T) {
	e := newEnv(t)
	c, err := net.Dial("tcp", e.srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	body := `{"position": 1}`
	fmt.Fprintf(c, "POST /api/items/0000/progress HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
	fmt.Fprintf(c, "GET /api/home HTTP/1.1\r\nHost: x\r\nX-Api-Key: %s\r\n\r\n", key)
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(c)
	for _, want := range []int{401, 200} {
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		if resp.StatusCode != want {
			t.Errorf("got %d, want %d", resp.StatusCode, want)
		}
	}
}

func TestUnprobeableFileReportsError(t *testing.T) {
	e := newEnv(t)
	r, raw := e.req("GET", "/api/items/"+e.movieID()+"/playback", nil, nil, true)
	if r.StatusCode != 500 || !bytes.Contains(raw, []byte("cannot read file")) {
		t.Error(r.StatusCode, string(raw))
	}
}

func TestSearch(t *testing.T) {
	e := newEnv(t)
	same(t, pluck(list(e.get("/api/search?q=hea")["movies"]), "title"), []any{"Heat"})
	same(t, pluck(list(e.get("/api/search?q=SHO")["series"]), "title"), []any{"Show"})
	same(t, len(list(e.get("/api/search?q=")["series"])), 0)
}

func TestHomeRecentGroupsSeries(t *testing.T) {
	e := newEnv(t)
	recent := list(e.get("/api/home")["recent"])
	got := map[string]bool{}
	for _, r := range recent {
		got[r["type"].(string)+":"+r["title"].(string)] = true
	}
	same(t, got, map[string]bool{"movie:Heat": true, "series:Show": true})
	for _, r := range recent {
		if r["type"] == "movie" && (r["file"] != "Heat (1995).mp4" || r["watched"] != false) {
			t.Error(r)
		}
	}
}

func TestCachedImageServedAndDroppedWhenMediaChanges(t *testing.T) {
	e := newEnv(t)
	mid := e.movieID()
	cached := filepath.Join(e.cfg.CacheDir, "images", "item-"+mid+".jpg")
	if r, _ := e.req("GET", "/img/item/"+mid, nil, nil, true); r.StatusCode != 404 {
		t.Error(r.StatusCode) // no ffmpeg, nothing cached
	}
	os.WriteFile(cached, []byte("\xff\xd8jpeg"), 0o644)
	if r, raw := e.req("GET", "/img/item/"+mid, nil, nil, true); r.StatusCode != 200 || string(raw) != "\xff\xd8jpeg" {
		t.Error(r.StatusCode)
	}
	e.scanner.Scan() // unchanged file: cache kept
	if _, err := os.Stat(cached); err != nil {
		t.Error("cache dropped")
	}
	touch(t, e.movies, "Heat (1995)/Heat (1995).mp4", []byte("new"))
	e.scanner.Scan()
	if _, err := os.Stat(cached); err == nil {
		t.Error("cache kept")
	}
}

func TestNewSidecarPosterReplacesCachedSeriesImage(t *testing.T) {
	e := newEnv(t)
	sid := e.seriesID()
	cached := filepath.Join(e.cfg.CacheDir, "images", "series-"+sid+".jpg")
	os.WriteFile(cached, []byte("old"), 0o644)
	touch(t, e.shows, "Show (2020)/poster.jpg", []byte("poster"))
	e.scanner.Scan()
	if _, err := os.Stat(cached); err == nil {
		t.Error("cache kept")
	}
}

func TestNestedShowFoldersGroupIntoOneSeries(t *testing.T) {
	dir := t.TempDir()
	anime := filepath.Join(dir, "media", "Anime")
	for _, p := range []string{
		"Show/Show S01/[Grp] Show - 01 [1080p].mkv", "Show/Show S01/[Grp] Show - 02 [1080p].mkv",
		"Show/Show S02/[Grp] Show - 01 [1080p].mkv", "Show/Show.S02E02.1080p-GRP/abc123.mkv",
		"Show/Pilot.mkv", // no number, still part of the show
		"Other.S01.1080p-GRP/Other.S01E01.1080p-GRP.mkv", "Other.S02.1080p-GRP/Other.S02E01.1080p-GRP.mkv",
		"Lone Film (2001)/Lone Film (2001).mkv",
	} {
		touch(t, anime, p, nil)
	}
	cfg := testConfig(dir, config.Library{Name: "Anime", Path: anime, Type: "auto"})
	db, _ := store.Open(cfg.CacheDir, cfg.ConfigDir)
	defer db.Close()
	im, _ := images.New(cfg, db)
	scan.New(cfg, db, im, nil).ScanLibrary(cfg.Libraries[0])

	series := map[string]string{}
	eps := map[string][]any{}
	var movies []string
	db.View(func(d *store.Data) {
		for _, s := range d.Series {
			series[s.Title] = s.ID
		}
		for _, it := range d.Items {
			if it.Kind == "episode" {
				ep := any(nil)
				if it.Episode != nil {
					ep = *it.Episode
				}
				eps[it.RelPath] = []any{it.SeriesID, *it.Season, ep}
			} else {
				movies = append(movies, it.Title)
			}
		}
	})
	same(t, len(series), 2)
	same(t, eps["Show/Show S02/[Grp] Show - 01 [1080p].mkv"], []any{series["Show"], 2, 1})
	same(t, eps["Show/Show.S02E02.1080p-GRP/abc123.mkv"], []any{series["Show"], 2, 2})
	same(t, eps["Show/Pilot.mkv"][0], series["Show"])
	same(t, eps["Other.S02.1080p-GRP/Other.S02E01.1080p-GRP.mkv"], []any{series["Other"], 2, 1})
	same(t, movies, []string{"Lone Film"})
}

func TestViewsAnswer304UntilTheStoreChanges(t *testing.T) {
	e := newEnv(t)
	lib := "/api/libraries/Shows"
	r, _ := e.req("GET", lib, nil, nil, true)
	etag := r.Header.Get("ETag")
	if r.StatusCode != 200 || etag == "" || r.Header.Get("Cache-Control") != "no-cache" {
		t.Fatal(r.Header)
	}
	r, raw := e.req("GET", lib, nil, map[string]string{"If-None-Match": etag}, true)
	if r.StatusCode != 304 || len(raw) != 0 {
		t.Error(r.StatusCode)
	}
	eid := list(e.get("/api/series/" + e.seriesID())["episodes"])[0]["id"].(string)
	e.post("/api/items/"+eid+"/progress", obj{"position": 300, "duration": 1440})
	r, raw = e.req("GET", lib, nil, map[string]string{"If-None-Match": etag}, true)
	if r.StatusCode != 200 || r.Header.Get("ETag") == etag {
		t.Error(r.StatusCode)
	}
	var v obj
	json.Unmarshal(raw, &v)
	same(t, list(v["series"])[0]["episodes"], 5)
}

func TestHomeEtagFollowsScanStatus(t *testing.T) {
	e := newEnv(t)
	r, _ := e.req("GET", "/api/home", nil, nil, true)
	etag := r.Header.Get("ETag")
	if r, _ := e.req("GET", "/api/home", nil, map[string]string{"If-None-Match": etag}, true); r.StatusCode != 304 {
		t.Error(r.StatusCode)
	}
	e.scanner.SetError("library Shows is missing")
	r, raw := e.req("GET", "/api/home", nil, map[string]string{"If-None-Match": etag}, true)
	var v obj
	json.Unmarshal(raw, &v)
	if r.StatusCode != 200 || v["status"].(obj)["error"] != "library Shows is missing" {
		t.Error(r.StatusCode, string(raw))
	}
}

func sp(s string) *string   { return &s }
func ip(i int) *int         { return &i }
func fp(f float64) *float64 { return &f }

func (e *env) setProbed(id string, container string, duration float64, streams []probe.Stream, skips []probe.Skip) {
	e.db.Update(func(d *store.Data) {
		it := *d.Items[id]
		it.Probed, it.Duration, it.Container, it.Streams, it.Skips = store.Probed, fp(duration), container, streams, skips
		d.Items[id] = &it
	})
}

func TestPlaybackInfoHasNextCardAndPreferredLanguage(t *testing.T) {
	e := newEnv(t)
	eps := list(e.get("/api/series/" + e.seriesID())["episodes"])
	e1, e2 := eps[0]["id"].(string), eps[1]["id"].(string)
	e.setProbed(e1, "matroska,webm", 1440, []probe.Stream{
		{Index: 0, Type: "video", Codec: "h264", PixFmt: sp("yuv420p"), Profile: sp("High")},
		{Index: 1, Type: "audio", Codec: "aac", Language: "jpn", Channels: ip(2)},
		{Index: 2, Type: "audio", Codec: "aac", Language: "eng", Channels: ip(2)},
	}, nil)
	info := e.get("/api/items/" + e1 + "/playback")
	same(t, info["audio_index"], 1) // AUDIO_LANGUAGES default: Japanese
	same(t, info["next"].(obj)["id"], e2)
	same(t, info["next"].(obj)["episode"], 2)
	same(t, info["mode"], "remux")
	same(t, info["mime"], `video/mp4; codecs="avc1.640028,mp4a.40.2"`)
	info = e.get("/api/items/" + e1 + "/playback?alang=eng")
	if info["audio_index"] != 2.0 || !strings.Contains(info["url"].(string), "audio=2") {
		t.Error(info)
	}
}

func TestFailedThumbnailIsNotRetriedUntilTheFileChanges(t *testing.T) {
	e := newEnv(t)
	mid := e.movieID()
	marker := filepath.Join(e.cfg.CacheDir, "images", "item-"+mid+".none")
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("the scan should have tried once (no ffmpeg here) and given up")
	}
	if r, _ := e.req("GET", "/img/item/"+mid, nil, nil, true); r.StatusCode != 404 {
		t.Error(r.StatusCode)
	}
	calls := 0
	e.images.Frame = func(string, *float64, string) bool { calls++; return false }
	e.scanner.Scan()
	if calls != 0 {
		t.Error("retried")
	}
	time.Sleep(10 * time.Millisecond) // a newer mtime than the marker
	touch(t, e.movies, "Heat (1995)/Heat (1995).mp4", []byte("new"))
	e.scanner.Scan()
	if _, err := os.Stat(marker); calls != 1 || err != nil {
		t.Error(calls, err)
	}
}

func TestIdleKeepaliveConnectionIsClosed(t *testing.T) {
	e := newEnv(t)
	srv := e.app.HTTPServer("")
	srv.IdleTimeout = 300 * time.Millisecond
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	go srv.Serve(ln)
	defer srv.Close()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprint(c, "GET /healthz HTTP/1.1\r\nHost: x\r\n\r\n")
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	all, err := io.ReadAll(c) // returns once the server hangs up
	if err != nil || !strings.Contains(string(all), `{"ok":true}`) {
		t.Error(err, string(all))
	}
}

func TestNextUpFollowsTheFurthestWatchedEpisode(t *testing.T) {
	e := newEnv(t)
	eps := map[string]string{}
	for _, ep := range list(e.get("/api/series/" + e.seriesID())["episodes"]) {
		eps[fmt.Sprint(ep["season"], ep["episode"])] = ep["id"].(string)
	}
	e.post("/api/items/"+eps["0 1"]+"/watched", obj{"watched": true})
	same(t, len(list(e.get("/api/home")["next_up"])), 0) // a watched special starts nothing
	e.post("/api/items/"+eps["1 2"]+"/watched", obj{"watched": true})
	e.post("/api/items/"+eps["1 3"]+"/watched", obj{"watched": true})
	same(t, pluck(list(e.get("/api/home")["next_up"]), "id"), []any{eps["2 1"]})
	e.post("/api/items/"+eps["2 1"]+"/watched", obj{"watched": true})
	same(t, len(list(e.get("/api/home")["next_up"])), 0) // E01 skipped long ago does not come back
}

func TestProblemsListsUnreadableFilesAndMissingLibraries(t *testing.T) {
	e := newEnv(t)
	// ffprobe is missing in these tests, so every file failed to probe.
	same(t, e.get("/api/home")["problems"], 6)
	d := e.get("/api/problems")
	files := list(d["files"])
	same(t, len(files), 6)
	same(t, len(d["missing"].([]any)), 0)
	for _, f := range files {
		if f["library"] == "Movies" && (f["path"] != "Heat (1995)/Heat (1995).mp4" || f["error"] == nil) {
			t.Error(f)
		}
	}
	os.Rename(e.movies, e.movies+".gone")
	e.scanner.Scan()
	d = e.get("/api/problems")
	same(t, d["missing"], []any{obj{"name": "Movies", "path": e.movies}})
	if !strings.Contains(d["last_error"].(string), "Movies") {
		t.Error(d["last_error"])
	}
}

func TestPlaybackInfoHasSkips(t *testing.T) {
	e := newEnv(t)
	mid := e.movieID()
	e.setProbed(mid, "mov,mp4,m4a,3gp,3g2,mj2", 6000, []probe.Stream{
		{Index: 0, Type: "video", Codec: "h264", PixFmt: sp("yuv420p"), Profile: sp("High")},
		{Index: 1, Type: "audio", Codec: "aac", Channels: ip(2)},
	}, []probe.Skip{{Kind: "intro", Start: 60, End: 150}})
	info := e.get("/api/items/" + mid + "/playback")
	same(t, info["skips"], []any{obj{"kind": "intro", "start": 60, "end": 150}})
	same(t, info["mode"], "direct")
	same(t, info["url"], "/file/"+mid)
}

func TestPlaybackInfoSuggestsConvertingCPUTranscodes(t *testing.T) {
	e := newEnv(t)
	e.cfg.AutoConvert = false
	eps := list(e.get("/api/series/" + e.seriesID())["episodes"])
	hevc := []probe.Stream{{Index: 0, Type: "video", Codec: "hevc", PixFmt: sp("yuv420p10le")},
		{Index: 1, Type: "audio", Codec: "aac", Language: "jpn", Channels: ip(2)}}
	for _, ep := range eps { // every episode HEVC, but only Season 01 shares the folder
		e.setProbed(ep["id"].(string), "matroska,webm", 1440, hevc, nil)
	}
	first := eps[0]["id"].(string)
	info := e.get("/api/items/" + first + "/playback")
	conv := info["convert"].(obj)
	same(t, conv["files"], 2)
	if !strings.HasSuffix(conv["folder"].(string), "Season 01") ||
		strings.Split(conv["folder_command"].(string), "\n")[0] != conv["file_command"] ||
		!strings.Contains(conv["file_command"].(string), "libx264") {
		t.Error(conv)
	}
	same(t, info["mode"], "transcode")
	same(t, info["mime"], `video/mp4; codecs="avc1.640028,mp4a.40.2"`)
	same(t, info["conversion"], nil) // AUTO_CONVERT off: nothing queued
	// Plays without a video transcode in a browser that decodes 10-bit HEVC.
	same(t, e.get("/api/items/" + first + "/playback?vcaps=hevc,hevc10")["convert"], nil)
	same(t, e.get("/api/items/" + first + "/playback?force=transcode")["convert"], nil)
	e.cfg.HWAccel = "vaapi"
	same(t, e.get("/api/items/" + first + "/playback")["convert"], nil)
}

func TestItemDetailAndGzip(t *testing.T) {
	e := newEnv(t)
	mid := e.movieID()
	d := e.get("/api/items/" + mid)
	if d["path"] != "Heat (1995)/Heat (1995).mp4" || !reflect.DeepEqual(d["streams"], []any{}) || d["overview"] != nil {
		t.Error(d)
	}
	r, raw := e.req("GET", "/app.js", nil, map[string]string{"Accept-Encoding": "gzip"}, false)
	if r.Header.Get("Content-Encoding") != "gzip" || len(raw) < 100 {
		t.Error(r.Header)
	}
	if r, _ := e.req("GET", "/app.js", nil, map[string]string{"If-None-Match": r.Header.Get("ETag")}, false); r.StatusCode != 304 {
		t.Error(r.StatusCode)
	}
	if r, _ := e.req("GET", "/api/nope", nil, nil, true); r.StatusCode != 404 {
		t.Error(r.StatusCode)
	}
	if r, raw := e.req("POST", "/api/items/"+mid+"/progress", "[1]", nil, true); r.StatusCode != 400 {
		t.Error(r.StatusCode, string(raw))
	}
}
