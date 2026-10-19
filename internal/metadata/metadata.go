// Package metadata fetches posters and overviews for series and movies from
// TMDB and AniList.
//
// Sources are tried in METADATA_SOURCES order (default TMDB, then AniList) and
// the first match wins. TMDB needs an API key; AniList needs none, so metadata
// is on unless METADATA_SOURCES is emptied. Runs as one phase of every scan and
// only looks up titles it has not looked up yet, whose folder name changed, or
// whose last lookup is older than METADATA_REFRESH_DAYS, so a scan with nothing
// new makes no requests at all.
//
// A poster next to the media (poster.jpg, folder.jpg, ...) always wins over the
// downloaded one. A wrong match is fixed by putting the id in the folder or
// file name, Jellyfin style: "Show (2020) [tmdbid-12345]", "{tmdb-12345}" or
// "[anilistid-12345]". An id tag skips the search and the other sources.
// Everything is stored in the disposable library cache and CACHE_DIR, so
// wiping the cache only costs one more round of lookups.
package metadata

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nine9123/media-watcher/internal/config"
	"github.com/nine9123/media-watcher/internal/images"
	"github.com/nine9123/media-watcher/internal/logx"
	"github.com/nine9123/media-watcher/internal/shutdown"
	"github.com/nine9123/media-watcher/internal/store"
)

var log = logx.New("mediawatcher.metadata")

const (
	API        = "https://api.themoviedb.org/3"
	ImageBase  = "https://image.tmdb.org/t/p/w500"
	userAgent  = "mediawatcher"
	AnilistAPI = "https://graphql.anilist.co"
)

var (
	// TMDB allows roughly 40 requests a second; one title costs one to three.
	Pause = 100 * time.Millisecond
	// AniList allows 90 requests a minute (30 while it is degraded); one title is one.
	AnilistPause = 2100 * time.Millisecond
)

var (
	idTag      = regexp.MustCompile(`(?i)[\[{]tmdb(?:id)?[-=: ]?(\d+)[\]}]`)
	anilistTag = regexp.MustCompile(`(?i)[\[{]anilist(?:id)?[-=: ]?(\d+)[\]}]`)
)

func tag(re *regexp.Regexp, path string) *int {
	m := re.FindStringSubmatch(path)
	if m == nil {
		return nil
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return nil
	}
	return &n
}

// TMDBIDFromPath finds `[tmdbid-123]` or `{tmdb-123}` anywhere in a path.
func TMDBIDFromPath(path string) *int { return tag(idTag, path) }

// AnilistIDFromPath finds `[anilistid-123]` or `{anilist-123}` anywhere in a path.
func AnilistIDFromPath(path string) *int { return tag(anilistTag, path) }

func year(date string) *int {
	if len(date) < 4 {
		return nil
	}
	n, err := strconv.Atoi(date[:4])
	if err != nil || n < 0 {
		return nil
	}
	return &n
}

// Error means a source could not be reached or refused us; try again on the next scan.
type Error struct {
	Source string
	Msg    string
}

func (e *Error) Error() string { return e.Msg }

// HTTPError is a non-2xx answer from Fetch.
type HTTPError struct{ Code int }

func (e *HTTPError) Error() string { return fmt.Sprintf("HTTP %d", e.Code) }

// Fetch does one request. body nil means GET. Replaceable in tests.
type Fetch func(url string, headers map[string]string, body []byte) ([]byte, error)

var client = &http.Client{Timeout: 20 * time.Second}

// HTTPFetch is the real Fetch.
func HTTPFetch(u string, headers map[string]string, body []byte) ([]byte, error) {
	method := http.MethodGet
	var rd io.Reader
	if body != nil {
		method, rd = http.MethodPost, bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(shutdown.Context(), method, u, rd)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &HTTPError{resp.StatusCode}
	}
	return data, nil
}

// Found is a match from one of the sources.
type Found struct {
	Source    string
	TMDBID    *int
	AnilistID *int
	Title     *string
	Year      *int
	Overview  *string
	Poster    string // TMDB image path ("/abc.jpg") or a full AniList URL; "" for none
}

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Client is a metadata source.
type Client interface {
	Find(kind, title string, year *int, id *int) (*Found, error)
	Image(poster string) ([]byte, error)
}

// ---- TMDB ---------------------------------------------------------------------

// Tmdb is just the calls the scanner needs.
type Tmdb struct {
	Key      string
	Language string
	Fetch    Fetch
}

func (t *Tmdb) get(path string, params url.Values, out any) (bool, error) {
	headers := map[string]string{"Accept": "application/json", "User-Agent": userAgent}
	if strings.HasPrefix(t.Key, "eyJ") { // v4 read access token
		headers["Authorization"] = "Bearer " + t.Key
	} else { // v3 API key
		params.Set("api_key", t.Key)
	}
	params.Set("language", t.Language)
	raw, err := t.Fetch(API+path+"?"+params.Encode(), headers, nil)
	if err != nil {
		if he, ok := err.(*HTTPError); ok {
			if he.Code == 404 {
				return false, nil
			}
			return false, &Error{"tmdb", fmt.Sprintf("HTTP %d for %s", he.Code, path)}
		}
		return false, &Error{"tmdb", fmt.Sprintf("%s: %v", path, err)}
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return false, &Error{"tmdb", fmt.Sprintf("%s: %v", path, err)}
	}
	return true, nil
}

type tmdbHit struct {
	ID           int    `json:"id"`
	Name         string `json:"name"`
	Title        string `json:"title"`
	FirstAirDate string `json:"first_air_date"`
	ReleaseDate  string `json:"release_date"`
	Overview     string `json:"overview"`
	PosterPath   string `json:"poster_path"`
}

// Find is the best match for a series (kind "tv") or movie, or nil.
func (t *Tmdb) Find(kind, title string, yr *int, tmdbID *int) (*Found, error) {
	var hit *tmdbHit
	if tmdbID != nil && *tmdbID != 0 {
		var h tmdbHit
		ok, err := t.get(fmt.Sprintf("/%s/%d", kind, *tmdbID), url.Values{}, &h)
		if err != nil {
			return nil, err
		}
		if ok {
			hit = &h
		}
	} else {
		yearParam := "year"
		if kind == "tv" {
			yearParam = "first_air_date_year"
		}
		// With the year first (remakes), then without (folders with a wrong year).
		tries := []*int{nil}
		if yr != nil && *yr != 0 {
			tries = []*int{yr, nil}
		}
		for _, y := range tries {
			p := url.Values{"query": {title}, "include_adult": {"false"}}
			if y != nil {
				p.Set(yearParam, strconv.Itoa(*y))
			}
			var res struct {
				Results []tmdbHit `json:"results"`
			}
			if _, err := t.get("/search/"+kind, p, &res); err != nil {
				return nil, err
			}
			if len(res.Results) > 0 {
				hit = &res.Results[0]
				break
			}
		}
	}
	if hit == nil || hit.ID == 0 {
		return nil, nil
	}
	name := hit.Name
	if name == "" {
		name = hit.Title
	}
	date := hit.FirstAirDate
	if date == "" {
		date = hit.ReleaseDate
	}
	id := hit.ID
	return &Found{Source: "tmdb", TMDBID: &id, Title: nonEmpty(name), Year: year(date),
		Overview: nonEmpty(hit.Overview), Poster: hit.PosterPath}, nil
}

func (t *Tmdb) Image(poster string) ([]byte, error) {
	data, err := t.Fetch(ImageBase+poster, map[string]string{"User-Agent": userAgent}, nil)
	if err != nil {
		return nil, &Error{"tmdb", fmt.Sprintf("poster %s: %v", poster, err)}
	}
	return data, nil
}

// ---- AniList ------------------------------------------------------------------

var (
	wordRE   = regexp.MustCompile(`[\p{L}\p{N}_]+`)
	brRE     = regexp.MustCompile(`(?i)<br\s*/?>`)
	tagRE    = regexp.MustCompile(`<[^>]+>`)
	blankRE  = regexp.MustCompile(`\n{3,}`)
	subtitle = regexp.MustCompile(`\s*(?::|\s[-–—~]\s|\()`)
)

// words gives lowercase alphanumeric words: "Frieren: Beyond…" -> "frieren beyond".
func words(s string) string {
	return strings.Join(wordRE.FindAllString(strings.ToLower(s), -1), " ")
}

// plain strips the <br>, <i> and HTML entities AniList descriptions carry.
func plain(description string) *string {
	if description == "" {
		return nil
	}
	s := brRE.ReplaceAllString(description, "\n")
	s = html.UnescapeString(tagRE.ReplaceAllString(s, ""))
	s = strings.TrimSpace(blankRE.ReplaceAllString(s, "\n\n"))
	return nonEmpty(s)
}

const anilistFields = `id format synonyms title { romaji english native } startDate { year }
  description(asHtml: false) coverImage { extraLarge large }`

var (
	anilistSearch = `query ($search: String, $formats: [MediaFormat]) {
  Page(perPage: 10) { media(search: $search, type: ANIME, format_in: $formats,
    sort: SEARCH_MATCH, isAdult: false) { ` + anilistFields + ` } } }`
	anilistByID    = "query ($id: Int) { Media(id: $id, type: ANIME) { " + anilistFields + " } }"
	anilistFormats = map[string][]string{
		"tv":    {"TV", "TV_SHORT", "ONA", "OVA", "SPECIAL"},
		"movie": {"MOVIE", "OVA", "ONA", "SPECIAL"},
	}
)

type anilistMedia struct {
	ID       int      `json:"id"`
	Format   string   `json:"format"`
	Synonyms []string `json:"synonyms"`
	Title    struct {
		Romaji  string `json:"romaji"`
		English string `json:"english"`
		Native  string `json:"native"`
	} `json:"title"`
	StartDate struct {
		Year *int `json:"year"`
	} `json:"startDate"`
	Description string `json:"description"`
	CoverImage  struct {
		ExtraLarge string `json:"extraLarge"`
		Large      string `json:"large"`
	} `json:"coverImage"`
}

// Anilist is anime only, no key needed.
//
// AniList's search is fuzzy and will happily turn "Heat" into some anime,
// so a result only counts when one of its titles or synonyms is the folder
// title, with or without a subtitle ("Frieren" -> "Frieren: Beyond Journey's
// End", but not "Heat" -> "Heat Haze Days"). Among those, see rank in Find.
type Anilist struct {
	Fetch Fetch
}

func (a *Anilist) post(query string, variables map[string]any, out any) (bool, error) {
	headers := map[string]string{"Accept": "application/json", "Content-Type": "application/json",
		"User-Agent": userAgent}
	body, _ := json.Marshal(map[string]any{"query": query, "variables": variables})
	raw, err := a.Fetch(AnilistAPI, headers, body)
	if err != nil {
		if he, ok := err.(*HTTPError); ok {
			if he.Code == 404 { // Media(id:) that does not exist
				return false, nil
			}
			return false, &Error{"anilist", fmt.Sprintf("HTTP %d", he.Code)}
		}
		return false, &Error{"anilist", err.Error()}
	}
	var res struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return false, &Error{"anilist", err.Error()}
	}
	hasData := len(res.Data) > 0 && string(res.Data) != "null"
	if len(res.Errors) > 0 && !hasData {
		return false, &Error{"anilist", res.Errors[0].Message}
	}
	if !hasData {
		return false, nil
	}
	if err := json.Unmarshal(res.Data, out); err != nil {
		return false, &Error{"anilist", err.Error()}
	}
	return true, nil
}

// matches is 2 when one of the titles matches, 1 for only a synonym, else 0.
func matches(m *anilistMedia, title string) int {
	want := words(title)
	if want == "" {
		return 0
	}
	hit := func(names []string) bool {
		for _, n := range names {
			if n == "" {
				continue
			}
			main := n
			if loc := subtitle.FindStringIndex(n); loc != nil {
				main = n[:loc[0]]
			}
			// The whole title, or its main part before a subtitle.
			if want == words(n) || want == words(main) {
				return true
			}
		}
		return false
	}
	if hit([]string{m.Title.Romaji, m.Title.English, m.Title.Native}) {
		return 2
	}
	if hit(m.Synonyms) {
		return 1
	}
	return 0
}

// Find has the same contract as Tmdb.Find, with Poster a full image URL.
func (a *Anilist) Find(kind, title string, yr *int, anilistID *int) (*Found, error) {
	var hit *anilistMedia
	if anilistID != nil && *anilistID != 0 {
		var res struct {
			Media *anilistMedia `json:"Media"`
		}
		if _, err := a.post(anilistByID, map[string]any{"id": *anilistID}, &res); err != nil {
			return nil, err
		}
		hit = res.Media
	} else {
		formats := anilistFormats[kind]
		var res struct {
			Page struct {
				Media []anilistMedia `json:"media"`
			} `json:"Page"`
		}
		if _, err := a.post(anilistSearch, map[string]any{"search": title, "formats": formats}, &res); err != nil {
			return nil, err
		}
		// Year (±1), then a real title over a synonym (commercials and
		// specials borrow the film's name), then the main format.
		rank := func(m *anilistMedia) [3]int {
			near := 0
			if y := m.StartDate.Year; yr != nil && y != nil && *y-*yr <= 1 && *yr-*y <= 1 {
				near = 1
			}
			main := 0
			if m.Format == formats[0] {
				main = 1
			}
			return [3]int{near, matches(m, title), main}
		}
		var best [3]int
		for i := range res.Page.Media {
			m := &res.Page.Media[i]
			if matches(m, title) == 0 {
				continue
			}
			// The first of equals wins: AniList's own search order.
			if r := rank(m); hit == nil || greater(r, best) {
				hit, best = m, r
			}
		}
	}
	if hit == nil || hit.ID == 0 {
		return nil, nil
	}
	name := hit.Title.English
	if name == "" {
		name = hit.Title.Romaji
	}
	if name == "" {
		name = hit.Title.Native
	}
	poster := hit.CoverImage.ExtraLarge
	if poster == "" {
		poster = hit.CoverImage.Large
	}
	id := hit.ID
	return &Found{Source: "anilist", AnilistID: &id, Title: nonEmpty(name), Year: hit.StartDate.Year,
		Overview: plain(hit.Description), Poster: poster}, nil
}

func greater(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}

func (a *Anilist) Image(poster string) ([]byte, error) {
	data, err := a.Fetch(poster, map[string]string{"User-Agent": userAgent}, nil)
	if err != nil {
		return nil, &Error{"anilist", fmt.Sprintf("poster %s: %v", poster, err)}
	}
	return data, nil
}

// ---- refresh ------------------------------------------------------------------

// Sources are the configured sources that can actually be used, in order.
func Sources(cfg *config.Config) []string {
	out := []string{}
	for _, s := range cfg.MetadataSources {
		if s == "anilist" || (s == "tmdb" && cfg.TMDBAPIKey != "") {
			out = append(out, s)
		}
	}
	return out
}

// Progress is told how the lookups go; the scanner's status implements it.
type Progress interface {
	BeginFetch(n int)
	Fetched()
	Fail(msg string)
}

type Metadata struct {
	cfg     *config.Config
	db      *store.Store
	images  *images.Images
	sources []string
	clients map[string]Client
}

// New uses the real HTTP clients unless tmdb/anilist are given (tests).
func New(cfg *config.Config, db *store.Store, im *images.Images, tmdb, anilist Client) *Metadata {
	if tmdb == nil {
		tmdb = &Tmdb{Key: cfg.TMDBAPIKey, Language: cfg.TMDBLanguage, Fetch: HTTPFetch}
	}
	if anilist == nil {
		anilist = &Anilist{Fetch: HTTPFetch}
	}
	return &Metadata{cfg: cfg, db: db, images: im, sources: Sources(cfg),
		clients: map[string]Client{"tmdb": tmdb, "anilist": anilist}}
}

type target struct {
	kind, id, title   string
	year              *int
	tmdbID, anilistID *int
}

// targets are every series and movie.
func (md *Metadata) targets() []target {
	var series, movies []*target
	sortName := map[*target]string{}
	md.db.View(func(d *store.Data) {
		for _, s := range d.Series {
			t := &target{"series", s.ID, s.Title, s.Year, TMDBIDFromPath(s.Dir), AnilistIDFromPath(s.Dir)}
			series = append(series, t)
			sortName[t] = s.SortName
		}
		for _, it := range d.Items {
			if it.Kind == "movie" {
				t := &target{"item", it.ID, it.Title, it.Year, TMDBIDFromPath(it.RelPath), AnilistIDFromPath(it.RelPath)}
				movies = append(movies, t)
				sortName[t] = it.SortName
			}
		}
	})
	var out []target
	for _, group := range [][]*target{series, movies} {
		sort.Slice(group, func(i, j int) bool {
			if sortName[group[i]] != sortName[group[j]] {
				return sortName[group[i]] < sortName[group[j]]
			}
			return group[i].id < group[j].id
		})
		for _, t := range group {
			out = append(out, *t)
		}
	}
	return out
}

// find is the first match over the sources; an id tag pins its own source.
func (md *Metadata) find(t target) (*Found, error) {
	kind := "movie"
	if t.kind == "series" {
		kind = "tv"
	}
	has := func(s string) bool {
		for _, x := range md.sources {
			if x == s {
				return true
			}
		}
		return false
	}
	if t.anilistID != nil && *t.anilistID != 0 && has("anilist") {
		return md.findIn("anilist", kind, t, t.anilistID)
	}
	if t.tmdbID != nil && *t.tmdbID != 0 && has("tmdb") {
		return md.findIn("tmdb", kind, t, t.tmdbID)
	}
	for _, s := range md.sources {
		f, err := md.findIn(s, kind, t, nil)
		if err != nil || f != nil {
			return f, err
		}
	}
	return nil, nil
}

func (md *Metadata) findIn(source, kind string, t target, id *int) (*Found, error) {
	f, err := md.clients[source].Find(kind, t.title, t.year, id)
	if source == "tmdb" {
		time.Sleep(Pause)
	} else {
		time.Sleep(AnilistPause)
	}
	return f, err
}

func optStr(p *int) string {
	if p == nil || *p == 0 {
		return ""
	}
	return strconv.Itoa(*p)
}

// Refresh looks up everything that is new or due.
func (md *Metadata) Refresh(progress Progress) {
	targets := md.targets()
	live := map[string]bool{}
	for _, t := range targets {
		live[t.id] = true
	}
	md.dropOrphans(live)
	known := map[string]store.Meta{}
	md.db.View(func(d *store.Data) {
		for id, m := range d.Meta {
			known[id] = *m
		}
	})
	cutoff := float64(time.Now().Unix()) - float64(md.cfg.MetadataRefreshDays)*86400
	// The sources are part of the key: adding a TMDB key or a source
	// looks everything up again, including earlier misses.
	used := strings.Join(md.sources, "+")
	type job struct {
		t     target
		query string
		old   *store.Meta
	}
	var todo []job
	for _, t := range targets {
		query := fmt.Sprintf("%s|%s|%s|%s|%s", t.title, optStr(t.year), optStr(t.tmdbID), optStr(t.anilistID), used)
		old, ok := known[t.id]
		if !ok || old.Query != query || old.FetchedAt < cutoff {
			j := job{t: t, query: query}
			if ok {
				o := old
				j.old = &o
			}
			todo = append(todo, j)
		}
	}
	if progress != nil {
		progress.BeginFetch(len(todo))
	}
	for _, j := range todo {
		if shutdown.Stopping() {
			return // the rest is still due, so the next scan fetches it
		}
		if err := md.updateOne(j.t, j.query, j.old); err != nil {
			// Offline, bad key, rate limited: leave everything as it is.
			log.Warnf("metadata lookup stopped: %v", err)
			if progress != nil {
				progress.Fail("metadata: " + err.Error())
			}
			return
		}
		if progress != nil {
			progress.Fetched()
		}
	}
}

func (md *Metadata) updateOne(t target, query string, old *store.Meta) error {
	found, err := md.find(t)
	if err != nil {
		return err
	}
	var posterURL *string
	if found != nil && found.Poster != "" {
		p := found.Poster
		posterURL = &p
	}
	oldURL := ""
	if old != nil && old.PosterURL != nil {
		oldURL = *old.PosterURL
	}
	path := md.images.MetaPosterPath(t.id)
	_, statErr := os.Stat(path)
	switch {
	case posterURL != nil && (old == nil || oldURL != *posterURL || statErr != nil):
		data, err := md.clients[found.Source].Image(*posterURL)
		if err != nil {
			return err
		}
		if err := os.WriteFile(path+".tmp", data, 0o644); err != nil {
			return err
		}
		if err := os.Rename(path+".tmp", path); err != nil {
			return err
		}
		md.images.Remove(t.kind, t.id) // served from disk without a lookup: drop it now
	case posterURL == nil && statErr == nil:
		os.Remove(path)
		md.images.Remove(t.kind, t.id)
	}
	now := float64(time.Now().UnixNano()) / 1e9
	// art_at versions the image URL: only a different poster changes it.
	var oldPtr *string
	if old != nil {
		oldPtr = old.PosterURL
	}
	changed := (oldPtr == nil) != (posterURL == nil) || (oldPtr != nil && *oldPtr != *posterURL)
	m := store.Meta{ID: t.id, Kind: t.kind, Query: query, PosterURL: posterURL, FetchedAt: now}
	if found != nil {
		m.TMDBID, m.AnilistID, m.Title, m.Year, m.Overview = found.TMDBID, found.AnilistID, found.Title, found.Year, found.Overview
	}
	switch {
	case changed:
		m.ArtAt = now
	case old != nil:
		m.ArtAt = old.ArtAt
	}
	md.db.Update(func(d *store.Data) { d.Meta[t.id] = &m })
	if found != nil {
		id := found.TMDBID
		if id == nil {
			id = found.AnilistID
		}
		title := ""
		if found.Title != nil {
			title = *found.Title
		}
		log.Infof("metadata %s %q -> %s %d %q", t.kind, t.title, found.Source, *id, title)
	} else {
		log.Infof("metadata %s %q: no match", t.kind, t.title)
	}
	return nil
}

func (md *Metadata) dropOrphans(live map[string]bool) {
	var gone []string
	md.db.View(func(d *store.Data) {
		for id := range d.Meta {
			if !live[id] {
				gone = append(gone, id)
			}
		}
	})
	if len(gone) == 0 {
		return
	}
	md.db.Update(func(d *store.Data) {
		for _, id := range gone {
			delete(d.Meta, id)
		}
	})
	for _, id := range gone {
		os.Remove(md.images.MetaPosterPath(id))
	}
}
