// Package scan walks the libraries, parses names, probes new files, cleans
// up removed ones, and fetches metadata when it is enabled.
package scan

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nine9123/media-watcher/internal/config"
	"github.com/nine9123/media-watcher/internal/images"
	"github.com/nine9123/media-watcher/internal/logx"
	"github.com/nine9123/media-watcher/internal/metadata"
	"github.com/nine9123/media-watcher/internal/naming"
	"github.com/nine9123/media-watcher/internal/probe"
	"github.com/nine9123/media-watcher/internal/shutdown"
	"github.com/nine9123/media-watcher/internal/store"
)

var log = logx.New("mediawatcher.scan")

// Status is what the UI shows about the scan.
type Status struct {
	Running      bool     `json:"running"`
	Phase        string   `json:"phase"`
	Files        int      `json:"files"`
	ToProbe      int      `json:"to_probe"`
	Probed       int      `json:"probed"`
	ToFetch      int      `json:"to_fetch"`
	Fetched      int      `json:"fetched"`
	LastScan     *float64 `json:"last_scan"`
	LastDuration *float64 `json:"last_duration"`
	Error        *string  `json:"error"`
}

type Scanner struct {
	cfg      *config.Config
	db       *store.Store
	images   *images.Images
	metadata *metadata.Metadata // nil when METADATA_SOURCES is empty

	mu     sync.Mutex
	status Status

	run   sync.Mutex
	wake  chan struct{}
	retry bool
}

func New(cfg *config.Config, db *store.Store, im *images.Images, md *metadata.Metadata) *Scanner {
	return &Scanner{cfg: cfg, db: db, images: im, metadata: md, status: Status{Phase: "idle"},
		wake: make(chan struct{}, 1)}
}

// Status is a copy of the current status.
func (s *Scanner) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

func (s *Scanner) update(fn func(st *Status)) {
	s.mu.Lock()
	fn(&s.status)
	s.mu.Unlock()
}

// SetError records an error for the UI (and the next status poll).
func (s *Scanner) SetError(msg string) { s.update(func(st *Status) { st.Error = &msg }) }

// metadata.Progress
func (s *Scanner) BeginFetch(n int) { s.update(func(st *Status) { st.ToFetch, st.Fetched = n, 0 }) }
func (s *Scanner) Fetched()         { s.update(func(st *Status) { st.Fetched++ }) }
func (s *Scanner) Fail(msg string)  { s.SetError(msg) }

// ---- background loop ----------------------------------------------------------

func (s *Scanner) Start() { go s.loop() }

// Trigger asks for a scan now; retryErrors also probes files that failed before.
func (s *Scanner) Trigger(retryErrors bool) {
	s.mu.Lock()
	s.retry = s.retry || retryErrors
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Scanner) loop() {
	for !shutdown.Stopping() {
		func() {
			defer func() {
				if r := recover(); r != nil { // keep the loop alive no matter what
					log.Errorf("scan failed: %v\n%s", r, debug.Stack())
					s.SetError(toString(r))
				}
			}()
			s.Scan()
		}()
		var timer <-chan time.Time
		var t *time.Timer
		if s.cfg.ScanInterval > 0 {
			t = time.NewTimer(time.Duration(s.cfg.ScanInterval) * time.Second)
			timer = t.C
		}
		select {
		case <-s.wake:
		case <-timer:
		case <-shutdown.Context().Done():
		}
		if t != nil {
			t.Stop()
		}
	}
}

func toString(v any) string {
	if e, ok := v.(error); ok {
		return e.Error()
	}
	if s, ok := v.(string); ok {
		return s
	}
	return "panic"
}

// ---- scanning -------------------------------------------------------------------

// Scan runs one full scan; it returns at once if one is already running.
func (s *Scanner) Scan() {
	if !s.run.TryLock() {
		return
	}
	defer s.run.Unlock()
	defer debug.FreeOSMemory()
	defer s.update(func(st *Status) { st.Running, st.Phase = false, "idle" })
	t0 := time.Now()
	var retry bool
	s.update(func(st *Status) {
		retry, s.retry = s.retry, false
		*st = Status{Running: true, Phase: "walking", LastScan: st.LastScan, LastDuration: st.LastDuration}
	})
	for _, lib := range s.cfg.Libraries {
		s.ScanLibrary(lib)
	}
	s.dropUnknownLibraries()
	s.update(func(st *Status) { st.Phase = "probing" })
	s.ProbePending(retry)
	if s.metadata != nil {
		s.update(func(st *Status) { st.Phase = "metadata" })
		func() {
			defer func() { // never worth failing a scan over
				if r := recover(); r != nil {
					log.Errorf("metadata failed: %v", r)
					s.SetError("metadata: " + toString(r))
				}
			}()
			s.metadata.Refresh(s)
		}()
	}
	s.update(func(st *Status) { st.Phase = "thumbnails" })
	s.PregenerateImages()
	now := unixNow()
	d := math.Round(time.Since(t0).Seconds()*10) / 10
	s.update(func(st *Status) { st.LastScan, st.LastDuration = &now, &d })
}

func unixNow() float64 { return float64(time.Now().UnixNano()) / 1e9 }

type dirListing struct {
	rel     string
	listing map[string]string // lower-case name -> real name, used to find sidecar images
	videos  []string
}

// walkLibrary lists every directory under root. Unreadable directories are
// skipped, not fatal: a NAS share with one broken folder must not take the
// whole library down.
//
// Symlinked folders are followed, except one that leads back to a folder
// the walk is already inside ("Show/link -> .."), which would never end.
func walkLibrary(root string) []dirListing {
	type dir struct {
		rel  string
		path []string // real paths of this folder and every folder above it
	}
	var out []dirListing
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		realRoot = root
	}
	stack := []dir{{"", []string{realRoot}}}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		rel := cur.rel
		real := cur.path[len(cur.path)-1]
		full := root
		if rel != "" {
			full = filepath.Join(root, rel)
		}
		entries, err := os.ReadDir(full)
		if err != nil {
			log.Warnf("cannot read %s: %v", full, err)
			continue
		}
		d := dirListing{rel: rel, listing: map[string]string{}}
		var subdirs []dir
		for _, e := range entries {
			name := e.Name()
			isDir := e.IsDir()
			childReal := filepath.Join(real, name)
			if e.Type()&os.ModeSymlink != 0 {
				st, err := os.Stat(filepath.Join(full, name))
				if err != nil {
					continue
				}
				isDir = st.IsDir()
				if isDir {
					if childReal, err = filepath.EvalSymlinks(filepath.Join(full, name)); err != nil {
						continue
					}
					if slices.Contains(cur.path, childReal) {
						log.Warnf("not following %s: it leads back to %s", filepath.Join(full, name), childReal)
						continue
					}
				}
			}
			if isDir {
				if !naming.IsIgnoredDir(name) {
					childRel := name
					if rel != "" {
						childRel = rel + "/" + name
					}
					subdirs = append(subdirs, dir{childRel, append(cur.path[:len(cur.path):len(cur.path)], childReal)})
				}
				continue
			}
			d.listing[strings.ToLower(name)] = name
			if naming.IsVideo(name) && !strings.HasPrefix(name, ".") && !naming.IsExtra(name) {
				d.videos = append(d.videos, name)
			}
		}
		for i := len(subdirs) - 1; i >= 0; i-- { // visit in name order
			stack = append(stack, subdirs[i])
		}
		out = append(out, d)
	}
	return out
}

type seriesInfo struct {
	title   string
	year    *int
	dir     string
	dirs    []string // merged top-level folders, dir first
	missing bool
}

// mergeSeriesDirs maps series-folder keys that name the same show onto one canonical key.
//
// Folders merge when their cleaned titles match and their years are equal.
// A folder with no year joins the one dated folder of that title, but stays
// on its own if there are several (remakes: "Show (2005)", "Show (2020)").
func mergeSeriesDirs(series map[string]*seriesInfo) map[string]string {
	type gk struct {
		title string
		year  int // -1 = none
	}
	groups := map[gk][]string{}
	var order []gk
	keys := make([]string, 0, len(series))
	for k := range series {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		s := series[k]
		g := gk{naming.TitleKey(s.title), -1}
		if s.year != nil {
			g.year = *s.year
		}
		if _, ok := groups[g]; !ok {
			order = append(order, g)
		}
		groups[g] = append(groups[g], k)
	}
	for _, g := range order {
		if g.year != -1 {
			continue
		}
		var dated []gk
		for _, o := range order {
			if _, ok := groups[o]; ok && o.title == g.title && o.year != -1 {
				dated = append(dated, o)
			}
		}
		if len(dated) == 1 {
			groups[dated[0]] = append(groups[dated[0]], groups[g]...)
			delete(groups, g)
		}
	}
	merge := map[string]string{}
	for _, keys := range groups {
		if len(keys) < 2 {
			continue
		}
		sort.Slice(keys, func(i, j int) bool {
			ni, nj := series[keys[i]].year == nil, series[keys[j]].year == nil
			if ni != nj {
				return !ni
			}
			return keys[i] < keys[j]
		})
		for _, k := range keys[1:] {
			merge[k] = keys[0]
		}
	}
	return merge
}

type row struct {
	item      store.Item
	seriesKey string
	series    *seriesInfo
	changed   bool
}

type parsedFile struct {
	dir    *dirListing
	name   string
	rel    string
	parsed naming.Parsed
}

func sameMtime(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

// ScanLibrary brings the store in line with what is on disk for one library.
func (s *Scanner) ScanLibrary(lib config.Library) {
	root := lib.Path
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		log.Warnf("library %s: %s is missing, keeping existing entries", lib.Name, root)
		s.SetError(lib.Name + ": " + root + " not found")
		return
	}

	type old struct {
		size    int64
		mtime   float64
		addedAt float64
		image   string
	}
	existing := map[string]old{}
	oldPosters := map[string]string{}
	s.db.View(func(d *store.Data) {
		for _, it := range d.Items {
			if it.Library == lib.Name {
				existing[it.ID] = old{it.Size, it.Mtime, it.AddedAt, it.Image}
			}
		}
		for _, se := range d.Series {
			if se.Library == lib.Name {
				oldPosters[se.ID] = se.Poster
			}
		}
	})

	listings := walkLibrary(root)
	dirs := map[string]map[string]string{}
	var files []parsedFile
	for i := range listings {
		d := &listings[i]
		dirs[d.rel] = d.listing
		for _, name := range d.videos {
			rel := name
			if d.rel != "" {
				rel = d.rel + "/" + name
			}
			files = append(files, parsedFile{d, name, rel, naming.ParsePath(rel, lib.Type, len(d.videos))})
		}
	}
	// A show folder with recognised episodes claims every other video under
	// it too ("Show/Pilot.mkv", "Show/Arc 2/Some Title.mkv"), so those don't
	// end up as loose movies cluttering the library.
	showDirs := map[string]bool{}
	for _, f := range files {
		if f.parsed.Kind == "episode" && f.parsed.SeriesDir != "" {
			showDirs[f.parsed.SeriesDir] = true
		}
	}
	if lib.Type == "auto" && len(showDirs) > 0 {
		for i, f := range files {
			if top, _, _ := strings.Cut(f.dir.rel, "/"); f.parsed.Kind == "movie" && f.dir.rel != "" && showDirs[top] {
				files[i].parsed = naming.ParsePath(f.rel, "shows", len(f.dir.videos))
			}
		}
	}

	var rows []*row
	series := map[string]*seriesInfo{}
	var seriesOrder []string
	loose := map[string][]*row{}
	var looseOrder []string
	for _, f := range files {
		full := filepath.Join(root, f.rel)
		st, err := os.Stat(full)
		if err != nil {
			continue
		}
		p := f.parsed
		stem, _ := naming.SplitExt(f.name)
		mtime := statMtime(st)
		r := &row{item: store.Item{
			ID: store.ItemID(lib.Name, f.rel), Library: lib.Name, RelPath: f.rel, Path: full,
			Size: st.Size(), Mtime: mtime, Kind: p.Kind, Season: p.Season, Episode: p.Episode,
			EpisodeEnd: p.EpisodeEnd, Title: p.Title, Year: p.Year,
		}}
		var img string
		if p.Kind == "episode" {
			img = images.FindImage(f.dir.listing, stem+"-thumb", stem)
			si := &seriesInfo{title: p.SeriesTitle, year: p.SeriesYear, dir: p.SeriesDir}
			if p.SeriesDir != "" {
				if _, ok := series[p.SeriesDir]; !ok {
					series[p.SeriesDir] = si
					seriesOrder = append(seriesOrder, p.SeriesDir)
				}
			} else {
				k := naming.TitleKey(p.SeriesTitle)
				if _, ok := loose[k]; !ok {
					looseOrder = append(looseOrder, k)
				}
				loose[k] = append(loose[k], r)
			}
			r.seriesKey, r.series = p.SeriesDir, si
			season, episode := 0, 0
			if p.Season != nil {
				season = *p.Season
			}
			if p.Episode != nil {
				episode = *p.Episode
			}
			r.item.SortName = fmt.Sprintf("%04d%05d", season, episode) + strings.ToLower(f.name)
			if r.item.Title == "" {
				if p.Episode != nil {
					r.item.Title = "Episode " + strconv.Itoa(*p.Episode)
				} else {
					r.item.Title = stem
				}
			}
		} else {
			img = images.FindImage(f.dir.listing, stem+"-poster", stem, stem+"-thumb")
			if img == "" && f.dir.rel != "" && len(f.dir.videos) == 1 {
				img = images.FindImage(f.dir.listing, images.PosterNames...)
			}
			r.item.SortName = naming.SortKey(p.Title)
		}
		if img != "" {
			r.item.Image = filepath.Join(root, f.dir.rel, img)
		}
		o, had := existing[r.item.ID]
		r.changed = !had || o.size != st.Size() || !sameMtime(o.mtime, mtime)
		if had {
			r.item.AddedAt = o.addedAt
		} else {
			r.item.AddedAt = max(mtime, statCtime(st))
		}
		rows = append(rows, r)
	}

	// Several top-level folders of one show ("Show.S01.1080p-GRP",
	// "Show.S02.1080p-GRP") become one series. A folder without a year
	// joins the folder with a year only when there is no other candidate.
	if merge := mergeSeriesDirs(series); len(merge) > 0 {
		for _, r := range rows {
			if t, ok := merge[r.seriesKey]; ok && r.item.Kind == "episode" {
				r.seriesKey = t
			}
		}
		keys := make([]string, 0, len(merge))
		for k := range merge {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			target := series[merge[k]]
			if target.dirs == nil {
				target.dirs = []string{merge[k]}
			}
			target.dirs = append(target.dirs, series[k].dir)
			delete(series, k)
		}
	}

	// Loose episode files ("[Grp] Show - 01.mkv" in the library root) join a
	// folder series with the same title when there is exactly one.
	byTitle := map[string][]string{}
	for _, k := range seriesOrder {
		if se, ok := series[k]; ok {
			byTitle[naming.TitleKey(se.title)] = append(byTitle[naming.TitleKey(se.title)], k)
		}
	}
	for _, tk := range looseOrder {
		targets := byTitle[tk]
		for _, r := range loose[tk] {
			if len(targets) == 1 {
				r.seriesKey = targets[0]
			} else {
				r.seriesKey = "loose:" + tk
				if _, ok := series[r.seriesKey]; !ok {
					series[r.seriesKey] = r.series
					seriesOrder = append(seriesOrder, r.seriesKey)
				}
			}
		}
	}

	// Top-level folders with no video in them yet are shows that should be
	// there but aren't: list them as series with no episodes ("missing"),
	// unless a series of that title already exists.
	if lib.Type != "movies" {
		used := map[string]bool{}
		for _, r := range rows {
			if top, _, ok := strings.Cut(r.item.RelPath, "/"); ok {
				used[top] = true
			}
		}
		titles := map[string]bool{}
		for _, se := range series {
			titles[naming.TitleKey(se.title)] = true
		}
		var names []string
		for d := range dirs {
			names = append(names, d)
		}
		sort.Strings(names)
		for _, d := range names {
			if d == "" || strings.Contains(d, "/") || used[d] || series[d] != nil {
				continue
			}
			title, year, ok := naming.SeriesFromDir(d)
			if !ok || title == "" || titles[naming.TitleKey(title)] {
				continue
			}
			titles[naming.TitleKey(title)] = true
			series[d] = &seriesInfo{title: title, year: year, dir: d, missing: true}
		}
	}

	now := unixNow()
	seriesIDs := map[string]string{}
	posters := map[string]string{}
	missing := map[string]bool{}
	var newSeries []store.Series
	for key, si := range series {
		sid := store.SeriesID(lib.Name, key)
		seriesIDs[key] = sid
		poster := ""
		ds := si.dirs
		if ds == nil && si.dir != "" {
			ds = []string{si.dir}
		}
		for _, d := range ds {
			if img := images.FindImage(dirs[d], images.PosterNames...); img != "" {
				poster = filepath.Join(root, d, img)
				break
			}
		}
		posters[sid] = poster
		if si.missing {
			missing[sid] = true
		}
		newSeries = append(newSeries, store.Series{ID: sid, Library: lib.Name, Title: si.title, Year: si.year,
			Dir: si.dir, Poster: poster, SortName: naming.SortKey(si.title), AddedAt: now, Missing: si.missing})
	}
	for _, r := range rows {
		if r.item.Kind == "episode" {
			r.item.SeriesID = seriesIDs[r.seriesKey]
		}
	}

	// Auto-cleanup. A share that is mounted but empty (NAS asleep, SMB
	// hiccup) must not wipe the library, so only delete when we saw files.
	seen := map[string]bool{}
	for _, r := range rows {
		seen[r.item.ID] = true
	}
	var removed []string
	if len(rows) > 0 {
		for id := range existing {
			if !seen[id] {
				removed = append(removed, id)
			}
		}
	}

	s.db.Update(func(d *store.Data) {
		for i := range newSeries {
			ns := newSeries[i]
			if old, ok := d.Series[ns.ID]; ok {
				ns.AddedAt = old.AddedAt
			}
			d.Series[ns.ID] = &ns
		}
		// Empty folders that are gone (or have episodes now) lose their mark,
		// so dropEmptySeries removes the ones left with no episodes.
		for _, se := range d.Series {
			if se.Library == lib.Name && se.Missing && !missing[se.ID] {
				se.Missing = false
			}
		}
		for _, r := range rows {
			it := r.item
			if old, ok := d.Items[it.ID]; ok {
				nu := *old
				nu.Path, nu.Size, nu.Mtime, nu.Kind, nu.SeriesID = it.Path, it.Size, it.Mtime, it.Kind, it.SeriesID
				nu.Season, nu.Episode, nu.EpisodeEnd, nu.Title, nu.Year = it.Season, it.Episode, it.EpisodeEnd, it.Title, it.Year
				nu.SortName, nu.Image = it.SortName, it.Image
				if r.changed {
					nu.Probed, nu.ProbeError = store.Unprobed, nil
				}
				d.Items[it.ID] = &nu
			} else {
				d.Items[it.ID] = &it
			}
		}
		for _, id := range removed {
			delete(d.Items, id)
		}
	})
	for _, id := range removed {
		s.images.Remove("item", id)
	}
	// Cached images whose source changed since they were made.
	for _, r := range rows {
		o, had := existing[r.item.ID]
		if had && (r.changed || o.image != r.item.Image) {
			s.images.Remove("item", r.item.ID)
		} else {
			s.images.DropStale("item", r.item.ID, r.item.Image)
		}
	}
	for sid, poster := range posters {
		if old, ok := oldPosters[sid]; ok && old != poster {
			s.images.Remove("series", sid)
		} else {
			s.images.DropStale("series", sid, poster)
		}
	}
	s.dropEmptySeries()
	s.update(func(st *Status) { st.Files += len(rows) })
	if len(removed) > 0 {
		log.Infof("library %s: removed %d missing files", lib.Name, len(removed))
	}
}

func (s *Scanner) dropEmptySeries() {
	var gone []string
	s.db.View(func(d *store.Data) {
		has := map[string]bool{}
		for _, it := range d.Items {
			if it.SeriesID != "" {
				has[it.SeriesID] = true
			}
		}
		for id, se := range d.Series {
			if !se.Missing && !has[id] {
				gone = append(gone, id)
			}
		}
	})
	if len(gone) == 0 {
		return
	}
	s.db.Update(func(d *store.Data) {
		for _, id := range gone {
			delete(d.Series, id)
		}
	})
	for _, id := range gone {
		s.images.Remove("series", id)
	}
}

func (s *Scanner) dropUnknownLibraries() {
	names := map[string]bool{}
	for _, lib := range s.cfg.Libraries {
		names[lib.Name] = true
	}
	stale := false
	s.db.View(func(d *store.Data) {
		for _, it := range d.Items {
			stale = stale || !names[it.Library]
		}
		for _, se := range d.Series {
			stale = stale || !names[se.Library]
		}
	})
	if !stale {
		return
	}
	s.db.Update(func(d *store.Data) {
		for id, it := range d.Items {
			if !names[it.Library] {
				delete(d.Items, id)
			}
		}
		for id, se := range d.Series {
			if !names[se.Library] {
				delete(d.Series, id)
			}
		}
	})
}

// ---- probing --------------------------------------------------------------------

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// ProbeItem runs ffprobe on one item and stores the result; false when it failed.
func (s *Scanner) ProbeItem(id, path string) bool {
	info, err := probe.Probe(shutdown.Context(), s.cfg.FFprobe, path)
	if err != nil {
		if shutdown.Stopping() {
			return false // ffprobe was killed with us; probe it again next start
		}
		msg := truncate(err.Error(), 500)
		s.db.Update(func(d *store.Data) {
			if it, ok := d.Items[id]; ok {
				nu := *it
				nu.Probed, nu.ProbeError = store.ProbeFailed, &msg
				d.Items[id] = &nu
			}
		})
		return false
	}
	s.db.Update(func(d *store.Data) {
		if it, ok := d.Items[id]; ok {
			nu := *it
			st := info.StartTime
			nu.Probed, nu.ProbeError = store.Probed, nil
			nu.Duration, nu.StartTime, nu.Container, nu.VideoCodec = info.Duration, &st, info.Container, info.VideoCodec
			nu.Width, nu.Height, nu.PixFmt, nu.Streams, nu.Skips = info.Width, info.Height, info.PixFmt, info.Streams, info.Skips
			d.Items[id] = &nu
		}
	})
	return true
}

// ProbePending probes every item not probed yet; retryErrors also the failed ones.
func (s *Scanner) ProbePending(retryErrors bool) {
	type todo struct {
		id, path string
		added    float64
	}
	var list []todo
	s.db.View(func(d *store.Data) {
		for _, it := range d.Items {
			if it.Probed == store.Unprobed || (retryErrors && it.Probed != store.Probed) {
				list = append(list, todo{it.ID, it.Path, it.AddedAt})
			}
		}
	})
	// Newest first: freshly added episodes become playable soonest.
	sort.Slice(list, func(i, j int) bool { return list[i].added > list[j].added })
	s.update(func(st *Status) { st.ToProbe = len(list) })
	if len(list) == 0 {
		return
	}
	work := make(chan todo)
	var wg sync.WaitGroup
	for w := 0; w < s.cfg.ProbeWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range work {
				if shutdown.Stopping() {
					continue // left unprobed, picked up by the scan on next start
				}
				s.ProbeItem(t.id, t.path)
				s.update(func(st *Status) { st.Probed++ })
			}
		}()
	}
	for _, t := range list {
		work <- t
	}
	close(work)
	wg.Wait()
}

// PregenerateImages warms the image cache so the first library visit is instant.
func (s *Scanner) PregenerateImages() {
	type entry struct {
		id, key string
		added   float64
	}
	var series, items []entry
	s.db.View(func(d *store.Data) {
		for _, se := range d.Series {
			series = append(series, entry{se.ID, se.SortName, 0})
		}
		for _, it := range d.Items {
			items = append(items, entry{it.ID, "", it.AddedAt})
		}
	})
	sort.Slice(series, func(i, j int) bool { return series[i].key < series[j].key })
	sort.Slice(items, func(i, j int) bool { return items[i].added > items[j].added })
	for _, e := range series {
		if shutdown.Stopping() {
			return
		}
		if _, err := os.Stat(s.images.CachePath("series", e.id)); err != nil {
			s.images.SeriesImage(e.id)
		}
	}
	for _, e := range items {
		if shutdown.Stopping() {
			return
		}
		if _, err := os.Stat(s.images.CachePath("item", e.id)); err != nil && !s.images.Failed("item", e.id) {
			s.images.ItemImage(e.id)
		}
	}
}
