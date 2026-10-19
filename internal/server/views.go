package server

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/nine9123/media-watcher/internal/store"
)

// The JSON views. Everything here runs inside store.View and copies what
// it returns, so it never holds on to store data after the lock.

const (
	watchedRatio = 0.90
	resumeMin    = 30.0
)

// ItemJSON is an item as list views show it.
type ItemJSON struct {
	ID          string   `json:"id"`
	Kind        string   `json:"kind"`
	Title       string   `json:"title"`
	Year        *int     `json:"year"`
	SeriesID    *string  `json:"series_id"`
	SeriesTitle *string  `json:"series_title"`
	Season      *int     `json:"season"`
	Episode     *int     `json:"episode"`
	EpisodeEnd  *int     `json:"episode_end"`
	Duration    *float64 `json:"duration"`
	Position    float64  `json:"position"`
	Watched     bool     `json:"watched"`
	VideoCodec  *string  `json:"video_codec"`
	Height      *int     `json:"height"`
	Library     string   `json:"library"`
	File        string   `json:"file"`
	Size        int64    `json:"size"`
	AddedAt     float64  `json:"added_at"`
	ProbeError  *string  `json:"probe_error"`
	ArtV        *string  `json:"art_v"`
}

// artV changes whenever a downloaded poster does; the UI puts it in the
// image URL so browsers drop their day-long cached copy.
func artV(t float64) *string {
	if t == 0 {
		return nil
	}
	s := strconv.FormatInt(int64(t), 16)
	return &s
}

func metaArt(d *store.Data, id string) *string {
	if m, ok := d.Meta[id]; ok {
		return artV(m.ArtAt)
	}
	return nil
}

func strp(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func itemJSON(d *store.Data, it *store.Item) ItemJSON {
	out := ItemJSON{
		ID: it.ID, Kind: it.Kind, Title: it.Title, Year: it.Year, SeriesID: strp(it.SeriesID),
		Season: it.Season, Episode: it.Episode, EpisodeEnd: it.EpisodeEnd, Duration: it.Duration,
		VideoCodec: it.VideoCodec, Height: it.Height, Library: it.Library, File: filepath.Base(it.Path),
		Size: it.Size, AddedAt: it.AddedAt, ProbeError: it.ProbeError, ArtV: metaArt(d, it.ID),
	}
	if s, ok := d.Series[it.SeriesID]; ok {
		t := s.Title
		out.SeriesTitle = &t
	}
	if p, ok := d.Progress[it.ID]; ok {
		out.Position, out.Watched = p.Position, p.Watched
	}
	return out
}

// episodes of a series in play order.
func episodes(d *store.Data, sid string) []*store.Item {
	var eps []*store.Item
	for _, it := range d.Items {
		if it.SeriesID == sid && sid != "" {
			eps = append(eps, it)
		}
	}
	sort.Slice(eps, func(i, j int) bool { return store.EpisodeLess(eps[i], eps[j]) })
	return eps
}

func watched(d *store.Data, id string) bool {
	p, ok := d.Progress[id]
	return ok && p.Watched
}

func resumePoint(d *store.Data, it *store.Item) float64 {
	p, ok := d.Progress[it.ID]
	if !ok || p.Watched || p.Position < resumeMin {
		return 0
	}
	if it.Duration != nil && *it.Duration > 0 && p.Position > *it.Duration*watchedRatio {
		return 0
	}
	return p.Position
}

func sortItems(items []*store.Item) {
	sort.Slice(items, func(i, j int) bool {
		if items[i].SortName != items[j].SortName {
			return items[i].SortName < items[j].SortName
		}
		return items[i].ID < items[j].ID
	})
}

func sortSeries(series []*store.Series) {
	sort.Slice(series, func(i, j int) bool {
		if series[i].SortName != series[j].SortName {
			return series[i].SortName < series[j].SortName
		}
		return series[i].ID < series[j].ID
	})
}

type libraryJSON struct {
	Name   string `json:"name"`
	Type   string `json:"type"`
	Series int    `json:"series"`
	Movies int    `json:"movies"`
}

type recentSeries struct {
	Type    string  `json:"type"`
	ID      string  `json:"id"`
	Title   string  `json:"title"`
	AddedAt float64 `json:"added_at"`
	ArtV    *string `json:"art_v"`
}

type recentMovie struct {
	Type string `json:"type"`
	ItemJSON
}

type homeJSON struct {
	Continue  []ItemJSON    `json:"continue"`
	NextUp    []ItemJSON    `json:"next_up"`
	Recent    []any         `json:"recent"`
	Libraries []libraryJSON `json:"libraries"`
	Problems  int           `json:"problems"`
	Status    any           `json:"status"`
}

func (a *App) home(d *store.Data) homeJSON {
	out := homeJSON{Continue: []ItemJSON{}, NextUp: []ItemJSON{}, Recent: []any{}}

	var cont []*store.Item
	for id, p := range d.Progress {
		if it, ok := d.Items[id]; ok && !p.Watched && p.Position >= resumeMin {
			cont = append(cont, it)
		}
	}
	byUpdate := func(items []*store.Item) {
		sort.Slice(items, func(i, j int) bool {
			ti, tj := d.Progress[items[i].ID].UpdatedAt, d.Progress[items[j].ID].UpdatedAt
			if ti != tj {
				return ti > tj
			}
			return items[i].ID < items[j].ID
		})
	}
	byUpdate(cont)
	if len(cont) > 24 {
		cont = cont[:24]
	}
	contIDs, contSeries := map[string]bool{}, map[string]bool{}
	for _, it := range cont {
		out.Continue = append(out.Continue, itemJSON(d, it))
		contIDs[it.ID] = true
		if it.SeriesID != "" {
			contSeries[it.SeriesID] = true
		}
	}

	// Next up: for recently active series, the first unwatched episode after
	// the furthest watched one (specials aside).
	active := map[string]float64{}
	for id, p := range d.Progress {
		if it, ok := d.Items[id]; ok && it.SeriesID != "" && p.UpdatedAt >= active[it.SeriesID] {
			active[it.SeriesID] = p.UpdatedAt
		}
	}
	sids := make([]string, 0, len(active))
	for sid := range active {
		sids = append(sids, sid)
	}
	sort.Slice(sids, func(i, j int) bool {
		if active[sids[i]] != active[sids[j]] {
			return active[sids[i]] > active[sids[j]]
		}
		return sids[i] < sids[j]
	})
	if len(sids) > 30 {
		sids = sids[:30]
	}
	for _, sid := range sids {
		var regular []*store.Item
		for _, e := range episodes(d, sid) {
			if e.Season == nil || *e.Season != 0 {
				regular = append(regular, e)
			}
		}
		last := -1
		for n, e := range regular {
			if watched(d, e.ID) {
				last = n
			}
		}
		if last < 0 {
			continue
		}
		for _, e := range regular[last+1:] {
			if !watched(d, e.ID) {
				if !contSeries[sid] && !contIDs[e.ID] && len(out.NextUp) < 24 {
					out.NextUp = append(out.NextUp, itemJSON(d, e))
				}
				break
			}
		}
	}

	// Recently added: newest series (by their newest episode) and movies.
	newest := map[string]*store.Item{}
	for _, it := range d.Items {
		key := it.SeriesID
		if key == "" {
			key = "\x00" + it.ID
		}
		if cur, ok := newest[key]; !ok || it.AddedAt > cur.AddedAt || (it.AddedAt == cur.AddedAt && it.ID < cur.ID) {
			newest[key] = it
		}
	}
	groups := make([]*store.Item, 0, len(newest))
	for _, it := range newest {
		groups = append(groups, it)
	}
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].AddedAt != groups[j].AddedAt {
			return groups[i].AddedAt > groups[j].AddedAt
		}
		return groups[i].ID < groups[j].ID
	})
	if len(groups) > 24 {
		groups = groups[:24]
	}
	for _, it := range groups {
		if it.SeriesID != "" {
			title := ""
			if s, ok := d.Series[it.SeriesID]; ok {
				title = s.Title
			}
			out.Recent = append(out.Recent, recentSeries{"series", it.SeriesID, title, it.AddedAt, metaArt(d, it.SeriesID)})
		} else {
			out.Recent = append(out.Recent, recentMovie{"movie", itemJSON(d, it)})
		}
	}
	out.Libraries = a.libraries(d)
	for _, it := range d.Items {
		if it.Probed == store.ProbeFailed {
			out.Problems++
		}
	}
	return out
}

func (a *App) libraries(d *store.Data) []libraryJSON {
	series, movies := map[string]int{}, map[string]int{}
	// From the series table, so empty show folders (no episodes) count too.
	for _, s := range d.Series {
		series[s.Library]++
	}
	for _, it := range d.Items {
		if it.Kind == "movie" {
			movies[it.Library]++
		}
	}
	out := []libraryJSON{}
	for _, lib := range a.Cfg.Libraries {
		out = append(out, libraryJSON{lib.Name, lib.Type, series[lib.Name], movies[lib.Name]})
	}
	return out
}

type librarySeries struct {
	ID       string   `json:"id"`
	Title    string   `json:"title"`
	Year     *int     `json:"year"`
	SortName string   `json:"sort_name"`
	Episodes int      `json:"episodes"`
	Watched  int      `json:"watched"`
	AddedAt  *float64 `json:"added_at"`
	PlayedAt *float64 `json:"played_at"`
	ArtV     *string  `json:"art_v"`
}

type libraryView struct {
	Name   string          `json:"name"`
	Series []librarySeries `json:"series"`
	Movies []ItemJSON      `json:"movies"`
}

func (a *App) library(d *store.Data, name string) (libraryView, bool) {
	known := false
	for _, lib := range a.Cfg.Libraries {
		known = known || lib.Name == name
	}
	if !known {
		return libraryView{}, false
	}
	out := libraryView{Name: name, Series: []librarySeries{}, Movies: []ItemJSON{}}
	var series []*store.Series
	for _, s := range d.Series {
		if s.Library == name {
			series = append(series, s)
		}
	}
	sortSeries(series)
	bySeries := map[string][]*store.Item{}
	var movies []*store.Item
	for _, it := range d.Items {
		if it.SeriesID != "" {
			bySeries[it.SeriesID] = append(bySeries[it.SeriesID], it)
		}
		if it.Library == name && it.Kind == "movie" {
			movies = append(movies, it)
		}
	}
	for _, s := range series {
		ls := librarySeries{ID: s.ID, Title: s.Title, Year: s.Year, SortName: s.SortName, ArtV: metaArt(d, s.ID)}
		for _, it := range bySeries[s.ID] {
			ls.Episodes++
			if ls.AddedAt == nil || it.AddedAt > *ls.AddedAt {
				t := it.AddedAt
				ls.AddedAt = &t
			}
			if p, ok := d.Progress[it.ID]; ok {
				if p.Watched {
					ls.Watched++
				}
				if ls.PlayedAt == nil || p.UpdatedAt > *ls.PlayedAt {
					t := p.UpdatedAt
					ls.PlayedAt = &t
				}
			}
		}
		out.Series = append(out.Series, ls)
	}
	sortItems(movies)
	for _, it := range movies {
		out.Movies = append(out.Movies, itemJSON(d, it))
	}
	return out, true
}

type seriesView struct {
	ID        string     `json:"id"`
	Title     string     `json:"title"`
	Year      *int       `json:"year"`
	Library   string     `json:"library"`
	Overview  *string    `json:"overview"`
	TMDBID    *int       `json:"tmdb_id"`
	AnilistID *int       `json:"anilist_id"`
	ArtV      *string    `json:"art_v"`
	Episodes  []ItemJSON `json:"episodes"`
	NextID    *string    `json:"next_id"`
}

func (a *App) series(d *store.Data, sid string) (seriesView, bool) {
	s, ok := d.Series[sid]
	if !ok {
		return seriesView{}, false
	}
	out := seriesView{ID: s.ID, Title: s.Title, Year: s.Year, Library: s.Library, Episodes: []ItemJSON{}}
	if m, ok := d.Meta[sid]; ok {
		out.Overview, out.TMDBID, out.AnilistID, out.ArtV = m.Overview, m.TMDBID, m.AnilistID, artV(m.ArtAt)
	}
	eps := episodes(d, sid)
	var resume *store.Item
	for _, e := range eps {
		if resumePoint(d, e) > 0 {
			resume = e
			break
		}
	}
	if resume == nil {
		var regular []*store.Item
		for _, e := range eps {
			if e.Season == nil || *e.Season != 0 {
				regular = append(regular, e)
			}
		}
		last := -1
		for n, e := range regular {
			if watched(d, e.ID) {
				last = n
			}
		}
		for _, e := range regular[last+1:] {
			if !watched(d, e.ID) {
				resume = e
				break
			}
		}
		if resume == nil && len(regular) > 0 && last == -1 {
			resume = regular[0]
		}
	}
	for _, e := range eps {
		out.Episodes = append(out.Episodes, itemJSON(d, e))
	}
	if resume != nil {
		id := resume.ID
		out.NextID = &id
	}
	return out, true
}

type searchSeries struct {
	ID      string  `json:"id"`
	Title   string  `json:"title"`
	Year    *int    `json:"year"`
	Library string  `json:"library"`
	ArtV    *string `json:"art_v"`
}

type searchView struct {
	Series []searchSeries `json:"series"`
	Movies []ItemJSON     `json:"movies"`
}

// asciiLower folds like SQLite's LIKE: ASCII letters only.
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

func (a *App) search(d *store.Data, q string) searchView {
	out := searchView{Series: []searchSeries{}, Movies: []ItemJSON{}}
	q = asciiLower(strings.TrimSpace(q))
	if q == "" {
		return out
	}
	like := func(s string) bool { return strings.Contains(asciiLower(s), q) }
	var series []*store.Series
	for _, s := range d.Series {
		if like(s.Title) {
			series = append(series, s)
		}
	}
	sortSeries(series)
	for i, s := range series {
		if i == 50 {
			break
		}
		out.Series = append(out.Series, searchSeries{s.ID, s.Title, s.Year, s.Library, metaArt(d, s.ID)})
	}
	var movies []*store.Item
	for _, it := range d.Items {
		if it.Kind == "movie" && (like(it.Title) || like(it.RelPath)) {
			movies = append(movies, it)
		}
	}
	sortItems(movies)
	for i, it := range movies {
		if i == 50 {
			break
		}
		out.Movies = append(out.Movies, itemJSON(d, it))
	}
	return out
}

type problemFile struct {
	ID          string  `json:"id"`
	Library     string  `json:"library"`
	Path        string  `json:"path"`
	SeriesTitle *string `json:"series_title"`
	Error       *string `json:"error"`
}

type missingLibrary struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type problemsView struct {
	Files     []problemFile    `json:"files"`
	Missing   []missingLibrary `json:"missing"`
	LastError *string          `json:"last_error"`
	Running   bool             `json:"running"`
}

// problems are files ffprobe could not read, and library folders that are not there.
func (a *App) problems(d *store.Data) problemsView {
	out := problemsView{Files: []problemFile{}, Missing: []missingLibrary{}}
	var failed []*store.Item
	for _, it := range d.Items {
		if it.Probed == store.ProbeFailed {
			failed = append(failed, it)
		}
	}
	sort.Slice(failed, func(i, j int) bool {
		if failed[i].Library != failed[j].Library {
			return failed[i].Library < failed[j].Library
		}
		return failed[i].RelPath < failed[j].RelPath
	})
	for i, it := range failed {
		if i == 500 {
			break
		}
		f := problemFile{ID: it.ID, Library: it.Library, Path: it.RelPath, Error: it.ProbeError}
		if s, ok := d.Series[it.SeriesID]; ok {
			t := s.Title
			f.SeriesTitle = &t
		}
		out.Files = append(out.Files, f)
	}
	for _, lib := range a.Cfg.Libraries {
		if st, err := os.Stat(lib.Path); err != nil || !st.IsDir() {
			out.Missing = append(out.Missing, missingLibrary{lib.Name, lib.Path})
		}
	}
	return out
}

// displayTitle is "Show S01E02 · Title" or "Movie (1995)", for the conversions list.
func displayTitle(d *store.Data, it *store.Item) string {
	if s, ok := d.Series[it.SeriesID]; ok {
		ep := ""
		if it.Episode != nil {
			if it.Season != nil && *it.Season == 0 {
				ep = "SP"
			} else {
				season := 0
				if it.Season != nil {
					season = *it.Season
				}
				ep = "S" + pad2(season)
			}
			ep += "E" + pad2(*it.Episode)
		}
		var parts []string
		if head := strings.TrimSpace(s.Title + " " + ep); head != "" {
			parts = append(parts, head)
		}
		if it.Title != "" {
			parts = append(parts, it.Title)
		}
		return strings.Join(parts, " · ")
	}
	if it.Year != nil && *it.Year != 0 {
		return it.Title + " (" + strconv.Itoa(*it.Year) + ")"
	}
	return it.Title
}

func pad2(n int) string {
	if n >= 0 && n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}
