package metadata_test

// TMDB and AniList metadata against fake APIs: matching, poster download, refresh rules.

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nine9123/media-watcher/internal/config"
	"github.com/nine9123/media-watcher/internal/convert"
	"github.com/nine9123/media-watcher/internal/images"
	"github.com/nine9123/media-watcher/internal/metadata"
	"github.com/nine9123/media-watcher/internal/scan"
	"github.com/nine9123/media-watcher/internal/server"
	"github.com/nine9123/media-watcher/internal/store"
)

var poster = []byte("\xff\xd8fake-jpeg")

func init() {
	metadata.Pause, metadata.AnilistPause = 0, 0
}

func touch(t *testing.T, root, rel string) {
	t.Helper()
	p := filepath.Join(root, rel)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, make([]byte, 2048), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fakeTmdb answers like api.themoviedb.org for a couple of titles and logs every request.
type fakeTmdb struct {
	t      *testing.T
	calls  []string
	down   bool
	poster []byte
	tv     map[string]map[string]any
	movies map[string]map[string]any
}

func newFakeTmdb(t *testing.T) *fakeTmdb {
	return &fakeTmdb{t: t, poster: poster,
		tv: map[string]map[string]any{"Show": {"id": 100, "name": "The Show", "first_air_date": "2020-04-01",
			"overview": "A show.", "poster_path": "/show.jpg"}},
		movies: map[string]map[string]any{"Heat": {"id": 949, "title": "Heat", "release_date": "1995-12-15",
			"overview": "A heist.", "poster_path": "/heat.jpg"}}}
}

func results(hit map[string]any) []byte {
	r := []any{}
	if hit != nil {
		r = append(r, hit)
	}
	raw, _ := json.Marshal(map[string]any{"results": r})
	return raw
}

func (f *fakeTmdb) fetch(u string, headers map[string]string, body []byte) ([]byte, error) {
	f.calls = append(f.calls, u)
	if f.down {
		return nil, errors.New("network unreachable")
	}
	p, _ := url.Parse(u)
	if p.Host == "image.tmdb.org" {
		return f.poster, nil
	}
	q := p.Query()
	if q.Get("api_key") != strings.Repeat("k", 32) {
		f.t.Errorf("api_key %q", q.Get("api_key"))
	}
	switch strings.TrimPrefix(p.Path, "/3") {
	case "/search/tv":
		return results(f.tv[q.Get("query")]), nil
	case "/search/movie":
		return results(f.movies[q.Get("query")]), nil
	case "/tv/555":
		return []byte(`{"id": 555, "name": "Other Show", "overview": "Forced.", "poster_path": "/other.jpg"}`), nil
	}
	return nil, &metadata.HTTPError{Code: 404}
}

// fakeAnilist answers like graphql.anilist.co, fuzzy search included.
type fakeAnilist struct {
	calls []anilistCall
	down  bool
	media []map[string]any
}

type anilistCall struct {
	url  string
	body map[string]any
}

func newFakeAnilist() *fakeAnilist {
	return &fakeAnilist{media: []map[string]any{
		{"id": 154587, "format": "TV", "synonyms": []any{"Frieren at the Funeral"},
			"title": map[string]any{"romaji": "Sousou no Frieren", "english": "Frieren: Beyond Journey's End",
				"native": "葬送のフリーレン"},
			"startDate":   map[string]any{"year": 2023},
			"description": "An elf<br><br>after the <i>journey</i> &amp; more.",
			"coverImage":  map[string]any{"extraLarge": "https://s4.anilist.co/frieren.jpg"}},
		{"id": 777, "format": "MOVIE", "synonyms": []any{},
			"title":     map[string]any{"romaji": "Heat Haze Days", "english": nil, "native": nil},
			"startDate": map[string]any{"year": 2010}, "description": "Not Heat.",
			"coverImage": map[string]any{"large": "https://s4.anilist.co/haze.jpg"}},
	}}
}

func (f *fakeAnilist) fetch(u string, headers map[string]string, body []byte) ([]byte, error) {
	var b map[string]any
	if body != nil {
		json.Unmarshal(body, &b)
	}
	f.calls = append(f.calls, anilistCall{u, b})
	if f.down {
		return nil, errors.New("network unreachable")
	}
	if strings.HasPrefix(u, "https://s4.anilist.co/") {
		return poster, nil
	}
	v := b["variables"].(map[string]any)
	if id, ok := v["id"]; ok {
		for _, m := range f.media {
			if float64(m["id"].(int)) == id.(float64) {
				raw, _ := json.Marshal(map[string]any{"data": map[string]any{"Media": m}})
				return raw, nil
			}
		}
		return nil, &metadata.HTTPError{Code: 404}
	}
	words := strings.Fields(strings.ToLower(v["search"].(string)))
	formats := map[string]bool{}
	for _, x := range v["formats"].([]any) {
		formats[x.(string)] = true
	}
	found := []any{}
	for _, m := range f.media {
		raw, _ := json.Marshal(m)
		dump := strings.ToLower(string(raw))
		hit := false
		for _, w := range words {
			hit = hit || strings.Contains(dump, w)
		}
		if formats[m["format"].(string)] && hit {
			found = append(found, m)
		}
	}
	raw, _ := json.Marshal(map[string]any{"data": map[string]any{"Page": map[string]any{"media": found}}})
	return raw, nil
}

type env struct {
	cfg     *config.Config
	db      *store.Store
	images  *images.Images
	tmdb    *fakeTmdb
	anilist *fakeAnilist
	scanner *scan.Scanner
	app     *server.App
	shows   string
}

func newEnv(t *testing.T) *env {
	dir := t.TempDir()
	shows, movies := filepath.Join(dir, "media", "Shows"), filepath.Join(dir, "media", "Movies")
	touch(t, shows, "Show (2020)/Season 01/Show - S01E01.mkv")
	touch(t, shows, "Unknown Thing/Unknown Thing - S01E01.mkv")
	touch(t, movies, "Heat (1995)/Heat (1995).mkv")
	cfg := config.Defaults()
	cfg.APIKey = "secret-key-123"
	cfg.Libraries = []config.Library{{Name: "Shows", Path: shows, Type: "shows"}, {Name: "Movies", Path: movies, Type: "movies"}}
	cfg.CacheDir, cfg.ConfigDir = filepath.Join(dir, "cache"), filepath.Join(dir, "config")
	cfg.FFmpeg, cfg.FFprobe = "/nonexistent/ffmpeg", "/nonexistent/ffprobe"
	cfg.TMDBAPIKey = strings.Repeat("k", 32)
	db, err := store.Open(cfg.CacheDir, cfg.ConfigDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	im, _ := images.New(&cfg, db)
	ft, fa := newFakeTmdb(t), newFakeAnilist()
	md := metadata.New(&cfg, db, im, &metadata.Tmdb{Key: cfg.TMDBAPIKey, Language: "en-US", Fetch: ft.fetch},
		&metadata.Anilist{Fetch: fa.fetch})
	sc := scan.New(&cfg, db, im, md)
	sc.Scan()
	return &env{&cfg, db, im, ft, fa, sc, server.New(&cfg, db, im, sc, convert.New(&cfg)), shows}
}

func (e *env) seriesID(title string) string {
	var id string
	e.db.View(func(d *store.Data) {
		for _, s := range d.Series {
			if s.Title == title {
				id = s.ID
			}
		}
	})
	return id
}

func (e *env) movieID() string {
	var id string
	e.db.View(func(d *store.Data) {
		for _, it := range d.Items {
			if it.Kind == "movie" {
				id = it.ID
			}
		}
	})
	return id
}

func (e *env) meta(id string) *store.Meta {
	var m *store.Meta
	e.db.View(func(d *store.Data) {
		if x, ok := d.Meta[id]; ok {
			c := *x
			m = &c
		}
	})
	return m
}

func (e *env) get(t *testing.T, path string) map[string]any {
	t.Helper()
	r := httptest.NewRequest("GET", path, nil)
	r.Header.Set("X-Api-Key", e.cfg.APIKey)
	w := httptest.NewRecorder()
	e.app.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("%s: %d %s", path, w.Code, w.Body)
	}
	var v map[string]any
	json.Unmarshal(w.Body.Bytes(), &v)
	return v
}

func (e *env) setFetchedAt(id string, t float64) {
	e.db.Update(func(d *store.Data) {
		for mid, m := range d.Meta {
			if id == "" || mid == id {
				m.FetchedAt = t
			}
		}
	})
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func TestTMDBIDFromPath(t *testing.T) {
	if n := metadata.TMDBIDFromPath("Show (2020) [tmdbid-1399]"); n == nil || *n != 1399 {
		t.Error(n)
	}
	if n := metadata.TMDBIDFromPath("Movies/Heat {tmdb-949}/Heat.mkv"); n == nil || *n != 949 {
		t.Error(n)
	}
	if metadata.TMDBIDFromPath("Show (2020)") != nil || metadata.TMDBIDFromPath("") != nil {
		t.Error("no tag")
	}
	if n := metadata.AnilistIDFromPath("Frieren [anilistid-154587]"); n == nil || *n != 154587 {
		t.Error(n)
	}
	if n := metadata.AnilistIDFromPath("Movies/X {anilist-1}/X.mkv"); n == nil || *n != 1 {
		t.Error(n)
	}
	if metadata.AnilistIDFromPath("Show [tmdbid-1]") != nil {
		t.Error("wrong tag")
	}
}

func TestScanFetchesPostersAndOverviews(t *testing.T) {
	e := newEnv(t)
	show, unknown, heat := e.seriesID("Show"), e.seriesID("Unknown Thing"), e.movieID()
	if m := e.meta(show); m == nil || *m.TMDBID != 100 {
		t.Fatal(m)
	}
	if m := e.meta(heat); *m.Overview != "A heist." {
		t.Error(m)
	}
	if m := e.meta(unknown); m == nil || m.TMDBID != nil { // looked up, no match: remembered
		t.Error(m)
	}
	for _, id := range []string{show, heat} {
		if raw, _ := os.ReadFile(e.images.MetaPosterPath(id)); string(raw) != string(poster) {
			t.Error("poster", id)
		}
	}
	if exists(e.images.MetaPosterPath(unknown)) {
		t.Error("poster for a miss")
	}
	if n := e.scanner.Status().Fetched; n != 3 {
		t.Error(n)
	}
	s := e.get(t, "/api/series/"+show)
	if s["overview"] != "A show." || s["tmdb_id"] != 100.0 || s["art_v"] == nil {
		t.Error(s)
	}
	arts := map[string]bool{}
	for _, x := range e.get(t, "/api/libraries/Shows")["series"].([]any) {
		x := x.(map[string]any)
		arts[x["title"].(string)] = x["art_v"] != nil
	}
	if !arts["Show"] || arts["Unknown Thing"] {
		t.Error(arts)
	}
	if m := e.get(t, "/api/libraries/Movies")["movies"].([]any)[0].(map[string]any); m["art_v"] == nil {
		t.Error(m)
	}
}

func TestSecondScanAsksNothing(t *testing.T) {
	e := newEnv(t)
	e.tmdb.calls, e.anilist.calls = nil, nil
	e.scanner.Scan()
	if len(e.tmdb.calls) != 0 || len(e.anilist.calls) != 0 {
		t.Error(e.tmdb.calls, e.anilist.calls)
	}
}

func TestStaleEntriesAreRefreshed(t *testing.T) {
	e := newEnv(t)
	e.setFetchedAt(e.movieID(), 0)
	art := e.meta(e.movieID()).ArtAt
	e.tmdb.calls = nil
	e.scanner.Scan()
	// Same poster as before: one search, no second download, same art version.
	if len(e.tmdb.calls) != 1 || !strings.Contains(e.tmdb.calls[0], "/search/movie") {
		t.Error(e.tmdb.calls)
	}
	if e.meta(e.movieID()).ArtAt != art {
		t.Error("art_at moved")
	}
}

func TestIDInFolderNameOverridesSearch(t *testing.T) {
	e := newEnv(t)
	os.Rename(filepath.Join(e.shows, "Unknown Thing"), filepath.Join(e.shows, "Unknown Thing [tmdbid-555]"))
	e.scanner.Scan()
	sid := e.seriesID("Unknown Thing")
	s := e.get(t, "/api/series/"+sid)
	if s["tmdb_id"] != 555.0 || s["overview"] != "Forced." || !exists(e.images.MetaPosterPath(sid)) {
		t.Error(s)
	}
}

func TestNetworkFailureKeepsWhatIsThere(t *testing.T) {
	e := newEnv(t)
	show := e.seriesID("Show")
	e.setFetchedAt("", 0)
	e.tmdb.down = true
	e.scanner.Scan()
	if st := e.scanner.Status(); st.Error == nil || !strings.Contains(*st.Error, "metadata") {
		t.Error(st.Error)
	}
	if s := e.get(t, "/api/series/"+show); s["overview"] != "A show." || !exists(e.images.MetaPosterPath(show)) {
		t.Error(s)
	}
	e.db.View(func(d *store.Data) {
		for _, m := range d.Meta {
			if m.FetchedAt != 0 {
				t.Error(m)
			}
		}
	})
}

func TestRemovedTitlesDropTheirMetadata(t *testing.T) {
	e := newEnv(t)
	show := e.seriesID("Show")
	os.RemoveAll(filepath.Join(e.shows, "Show (2020)"))
	e.scanner.Scan()
	if e.meta(show) != nil || exists(e.images.MetaPosterPath(show)) {
		t.Error("kept")
	}
}

func TestBearerTokenAuth(t *testing.T) {
	var seenURL string
	var seen map[string]string
	tm := &metadata.Tmdb{Key: "eyJhbGciOi.token", Fetch: func(u string, h map[string]string, _ []byte) ([]byte, error) {
		seenURL, seen = u, h
		return []byte(`{"results": []}`), nil
	}}
	tm.Find("tv", "x", nil, nil)
	if seen["Authorization"] != "Bearer eyJhbGciOi.token" || strings.Contains(seenURL, "api_key") {
		t.Error(seen, seenURL)
	}
}

func TestHTTPErrorIsATmdbError(t *testing.T) {
	tm := &metadata.Tmdb{Key: "bad", Fetch: func(string, map[string]string, []byte) ([]byte, error) {
		return nil, &metadata.HTTPError{Code: 401}
	}}
	_, err := tm.Find("movie", "Heat", nil, nil)
	var me *metadata.Error
	if !errors.As(err, &me) || me.Source != "tmdb" {
		t.Error(err)
	}
}

func TestDownloadedPosterReplacesFrameGrab(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("needs ffmpeg")
	}
	e := newEnv(t)
	jpg := filepath.Join(t.TempDir(), "p.jpg")
	if err := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "color=red:s=500x750",
		"-frames:v", "1", "-y", jpg).Run(); err != nil {
		t.Fatal(err)
	}
	e.cfg.FFmpeg = "ffmpeg"
	e.tmdb.poster, _ = os.ReadFile(jpg)
	e.db.Update(func(d *store.Data) {
		for _, m := range d.Meta {
			m.FetchedAt, m.PosterURL = 0, nil
		}
	})
	e.scanner.Scan()
	for kind, id := range map[string]string{"series": e.seriesID("Show"), "item": e.movieID()} {
		var out string
		if kind == "series" {
			out = e.images.SeriesImage(id)
		} else {
			out = e.images.ItemImage(id)
		}
		if out != e.images.CachePath(kind, id) {
			t.Fatal(kind, out)
		}
		raw, _ := exec.Command("ffprobe", "-v", "error", "-show_entries", "stream=width,height", "-of", "csv=p=0", out).Output()
		if got := strings.TrimSpace(string(raw)); got != "360,540" && got != "480,720" {
			t.Error(kind, got)
		}
	}
}

func TestAnilistIsTheFallbackWhenTmdbMisses(t *testing.T) {
	e := newEnv(t)
	// Show and Heat matched on TMDB; only the TMDB miss went to AniList.
	var searched []string
	for _, c := range e.anilist.calls {
		if c.body != nil {
			searched = append(searched, c.body["variables"].(map[string]any)["search"].(string))
		}
	}
	if strings.Join(searched, "|") != "Unknown Thing" {
		t.Error(searched)
	}

	touch(t, e.shows, "Frieren (2023)/Frieren - S01E01.mkv")
	e.tmdb.calls, e.anilist.calls = nil, nil
	e.scanner.Scan()
	sid := e.seriesID("Frieren")
	s := e.get(t, "/api/series/"+sid)
	if s["anilist_id"] != 154587.0 || s["tmdb_id"] != nil || s["overview"] != "An elf\n\nafter the journey & more." {
		t.Error(s)
	}
	if raw, _ := os.ReadFile(e.images.MetaPosterPath(sid)); string(raw) != string(poster) {
		t.Error("poster")
	}
	// TMDB was asked first (with and without the year), then AniList once.
	if len(e.tmdb.calls) != 2 {
		t.Error(e.tmdb.calls)
	}
	for _, c := range e.tmdb.calls {
		if !strings.Contains(c, "/search/tv") {
			t.Error(c)
		}
	}
	if len(e.anilist.calls) != 2 || e.anilist.calls[0].url != metadata.AnilistAPI || e.anilist.calls[1].url != "https://s4.anilist.co/frieren.jpg" {
		t.Error(e.anilist.calls)
	}
}

func find(t *testing.T, a *metadata.Anilist, kind, title string, year *int) *metadata.Found {
	t.Helper()
	f, err := a.Find(kind, title, year, nil)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func ip(n int) *int { return &n }

func TestAnilistNeedsAMatchingTitle(t *testing.T) {
	a := &metadata.Anilist{Fetch: newFakeAnilist().fetch}
	// Fuzzy search finds "Heat Haze Days", which is not "Heat" ...
	if f := find(t, a, "movie", "Heat", ip(1995)); f != nil {
		t.Error(f)
	}
	// ... but a title that starts with the folder name counts.
	hit := find(t, a, "tv", "Frieren", nil)
	if hit == nil || *hit.AnilistID != 154587 || *hit.Title != "Frieren: Beyond Journey's End" || *hit.Year != 2023 ||
		hit.Poster != "https://s4.anilist.co/frieren.jpg" {
		t.Error(hit)
	}
	if f := find(t, a, "tv", "Sousou no Frieren", ip(2023)); f == nil || *f.AnilistID != 154587 {
		t.Error(f)
	}
	if f := find(t, a, "tv", "Frieren at the Funeral", nil); f == nil || *f.AnilistID != 154587 {
		t.Error(f)
	}
	if f := find(t, a, "tv", "Frieren at the", nil); f != nil {
		t.Error(f)
	}
}

func TestAnilistTagOverridesTmdb(t *testing.T) {
	e := newEnv(t)
	os.Rename(filepath.Join(e.shows, "Show (2020)"), filepath.Join(e.shows, "Show (2020) [anilistid-154587]"))
	e.tmdb.calls = nil
	e.scanner.Scan()
	s := e.get(t, "/api/series/"+e.seriesID("Show"))
	if s["anilist_id"] != 154587.0 || s["tmdb_id"] != nil || len(e.tmdb.calls) != 0 {
		t.Error(s, e.tmdb.calls)
	}
}

func TestAnilistFailureKeepsWhatIsThere(t *testing.T) {
	e := newEnv(t)
	unknown := e.seriesID("Unknown Thing")
	e.setFetchedAt(unknown, 0)
	e.anilist.down = true
	e.scanner.Scan()
	if st := e.scanner.Status(); st.Error == nil || !strings.Contains(*st.Error, "metadata") {
		t.Error(st.Error)
	}
	if e.meta(unknown).FetchedAt != 0 {
		t.Error("fetched_at moved")
	}
}

func TestAnilistErrors(t *testing.T) {
	a := &metadata.Anilist{Fetch: func(string, map[string]string, []byte) ([]byte, error) {
		return nil, &metadata.HTTPError{Code: 429}
	}}
	var me *metadata.Error
	if _, err := a.Find("tv", "Frieren", nil, nil); !errors.As(err, &me) || me.Source != "anilist" {
		t.Error(err)
	}
	b := &metadata.Anilist{Fetch: newFakeAnilist().fetch}
	if f, err := b.Find("tv", "x", nil, ip(999)); f != nil || err != nil { // 404 by id
		t.Error(f, err)
	}
}

func TestAnilistOnlyWithoutTmdbKey(t *testing.T) {
	dir := t.TempDir()
	shows := filepath.Join(dir, "media", "Shows")
	touch(t, shows, "Frieren/Frieren - 01.mkv")
	cfg := config.Defaults()
	cfg.APIKey = "secret-key-123"
	cfg.Libraries = []config.Library{{Name: "Shows", Path: shows, Type: "shows"}}
	cfg.CacheDir, cfg.ConfigDir = filepath.Join(dir, "cache"), filepath.Join(dir, "config")
	cfg.FFmpeg, cfg.FFprobe = "/nonexistent/ffmpeg", "/nonexistent/ffprobe"
	if s := metadata.Sources(&cfg); strings.Join(s, ",") != "anilist" {
		t.Fatal(s)
	}
	noTmdb := func(string, map[string]string, []byte) ([]byte, error) {
		t.Error("TMDB asked without a key")
		return nil, errors.New("no")
	}
	db, _ := store.Open(cfg.CacheDir, cfg.ConfigDir)
	defer db.Close()
	im, _ := images.New(&cfg, db)
	md := metadata.New(&cfg, db, im, &metadata.Tmdb{Fetch: noTmdb}, &metadata.Anilist{Fetch: newFakeAnilist().fetch})
	scan.New(&cfg, db, im, md).Scan()
	db.View(func(d *store.Data) {
		for _, m := range d.Meta {
			if m.AnilistID == nil || *m.AnilistID != 154587 {
				t.Error(m)
			}
		}
	})
}

func TestSourcesConfig(t *testing.T) {
	base := map[string]string{"API_KEY": "secret-key-123", "LIBRARIES": ""}
	load := func(extra map[string]string) config.Config {
		env := map[string]string{}
		for k, v := range base {
			env[k] = v
		}
		for k, v := range extra {
			env[k] = v
		}
		c, err := config.Load(env)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	join := func(c config.Config) string { return strings.Join(metadata.Sources(&c), ",") }
	c := load(nil)
	if strings.Join(c.MetadataSources, ",") != "tmdb,anilist" || join(c) != "anilist" {
		t.Error(c.MetadataSources)
	}
	c = load(map[string]string{"TMDB_DEFAULT_API_KEY": "baked"})
	if c.TMDBAPIKey != "baked" || join(c) != "tmdb,anilist" {
		t.Error(c.TMDBAPIKey)
	}
	if c = load(map[string]string{"TMDB_DEFAULT_API_KEY": "baked", "TMDB_API_KEY": "mine"}); c.TMDBAPIKey != "mine" {
		t.Error(c.TMDBAPIKey)
	}
	if c = load(map[string]string{"TMDB_API_KEY": "mine", "METADATA_SOURCES": "anilist, tmdb"}); join(c) != "anilist,tmdb" {
		t.Error(join(c))
	}
	if join(load(map[string]string{"METADATA_SOURCES": ""})) != "" || join(load(map[string]string{"METADATA_SOURCES": "none"})) != "" {
		t.Error("off")
	}
	env := map[string]string{"API_KEY": "secret-key-123", "METADATA_SOURCES": "imdb"}
	if _, err := config.Load(env); err == nil {
		t.Error("imdb accepted")
	}
	if _, err := config.Load(map[string]string{"API_KEY": "short"}); err == nil {
		t.Error("short key accepted")
	}
}

func TestAnilistPrefersARealTitleOverASynonym(t *testing.T) {
	fake := newFakeAnilist()
	fake.media = append([]map[string]any{{"id": 5, "format": "SPECIAL", "synonyms": []any{"Your Name."},
		"title": map[string]any{"romaji": "Some Commercial"}, "startDate": map[string]any{"year": 2016}}}, fake.media...)
	fake.media = append(fake.media, map[string]any{"id": 21519, "format": "MOVIE", "synonyms": []any{},
		"title":     map[string]any{"romaji": "Kimi no Na wa.", "english": "Your Name."},
		"startDate": map[string]any{"year": 2016}})
	if f := find(t, &metadata.Anilist{Fetch: fake.fetch}, "movie", "Your Name", ip(2016)); f == nil || *f.AnilistID != 21519 {
		t.Error(f)
	}
}
