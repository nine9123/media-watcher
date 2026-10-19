// Package store keeps the library index and watch progress.
//
// Everything lives in memory behind one RWMutex; a library of a few
// thousand files is a few MB. Two files on disk:
//
//   - CACHE_DIR/library.json - everything derived from the media (scan and
//     probe results, TMDB/AniList metadata). Disposable: delete it and the
//     next scan rebuilds it. Written a few seconds after a change.
//   - CONFIG_DIR/progress.jsonl - watch progress. Small, keep it. Every
//     change is appended as one line and the file is compacted on start.
//
// The Python version kept the same data in SQLite (library.db, state.db).
// When the JSON files do not exist yet those are imported once, so
// switching versions keeps watch progress and does not rescan everything.
//
// Item ids are a hash of (library name, path relative to the library root),
// so progress survives a cache wipe and a change of mount point.
package store

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nine9123/media-watcher/internal/logx"
	"github.com/nine9123/media-watcher/internal/probe"
)

var log = logx.New("mediawatcher.store")

// Probe states of an item.
const (
	Unprobed    = 0
	Probed      = 1
	ProbeFailed = 2
)

type Item struct {
	ID         string         `json:"id"`
	Library    string         `json:"library"`
	RelPath    string         `json:"rel_path"`
	Path       string         `json:"path"`
	Size       int64          `json:"size"`
	Mtime      float64        `json:"mtime"`
	Kind       string         `json:"kind"` // episode | movie
	SeriesID   string         `json:"series_id,omitempty"`
	Season     *int           `json:"season,omitempty"`
	Episode    *int           `json:"episode,omitempty"`
	EpisodeEnd *int           `json:"episode_end,omitempty"`
	Title      string         `json:"title"`
	Year       *int           `json:"year,omitempty"`
	SortName   string         `json:"sort_name"`
	AddedAt    float64        `json:"added_at"`
	Image      string         `json:"image,omitempty"` // sidecar image next to the media
	Probed     int            `json:"probed"`
	ProbeError *string        `json:"probe_error,omitempty"`
	Duration   *float64       `json:"duration,omitempty"`
	StartTime  *float64       `json:"start_time,omitempty"`
	Container  string         `json:"container,omitempty"`
	VideoCodec *string        `json:"video_codec,omitempty"`
	Width      *int           `json:"width,omitempty"`
	Height     *int           `json:"height,omitempty"`
	PixFmt     *string        `json:"pix_fmt,omitempty"`
	Streams    []probe.Stream `json:"streams,omitempty"`
	Skips      []probe.Skip   `json:"skips,omitempty"`
}

type Series struct {
	ID       string  `json:"id"`
	Library  string  `json:"library"`
	Title    string  `json:"title"`
	Year     *int    `json:"year,omitempty"`
	Dir      string  `json:"dir,omitempty"`
	Poster   string  `json:"poster,omitempty"`
	SortName string  `json:"sort_name"`
	AddedAt  float64 `json:"added_at"`
	Missing  bool    `json:"missing,omitempty"` // an empty show folder, kept as a series with no episodes
}

// Meta is a TMDB/AniList lookup for a series or movie, keyed by its id.
type Meta struct {
	ID        string  `json:"id"`
	Kind      string  `json:"kind"` // series | item
	Query     string  `json:"query"`
	TMDBID    *int    `json:"tmdb_id,omitempty"`
	AnilistID *int    `json:"anilist_id,omitempty"`
	Title     *string `json:"title,omitempty"`
	Year      *int    `json:"year,omitempty"`
	Overview  *string `json:"overview,omitempty"`
	PosterURL *string `json:"poster_url,omitempty"`
	ArtAt     float64 `json:"art_at"`
	FetchedAt float64 `json:"fetched_at"`
}

type Progress struct {
	ID        string   `json:"id"`
	Position  float64  `json:"position"`
	Duration  *float64 `json:"duration,omitempty"`
	Watched   bool     `json:"watched"`
	UpdatedAt float64  `json:"updated_at"`
}

func ItemID(library, relPath string) string {
	h := sha1.Sum([]byte(library + "\x00" + relPath))
	return hex.EncodeToString(h[:])[:16]
}

func SeriesID(library, key string) string {
	h := sha1.Sum([]byte("series\x00" + library + "\x00" + key))
	return hex.EncodeToString(h[:])[:16]
}

// Data is everything in the store. Only touch it inside View or Update.
type Data struct {
	Items    map[string]*Item
	Series   map[string]*Series
	Meta     map[string]*Meta
	Progress map[string]*Progress
}

type Store struct {
	mu   sync.RWMutex
	data Data
	// Bumped after every change. The JSON views use it as their ETag, so a
	// revisit with nothing changed costs nothing.
	generation atomic.Int64

	libraryPath  string
	progressPath string
	dirty        atomic.Bool
	flushMu      sync.Mutex // one library.json writer at a time
	logMu        sync.Mutex
	logFile      *os.File
	logLines     int
	stop         chan struct{}
	done         chan struct{}
}

// FlushEvery is how long a library change may wait before library.json is written.
var FlushEvery = 3 * time.Second

// Open loads (or imports) both files and starts the background writer.
func Open(cacheDir, configDir string) (*Store, error) {
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{
		data: Data{Items: map[string]*Item{}, Series: map[string]*Series{},
			Meta: map[string]*Meta{}, Progress: map[string]*Progress{}},
		libraryPath:  filepath.Join(cacheDir, "library.json"),
		progressPath: filepath.Join(configDir, "progress.jsonl"),
		stop:         make(chan struct{}),
		done:         make(chan struct{}),
	}
	if err := s.loadLibrary(filepath.Join(cacheDir, "library.db")); err != nil {
		return nil, err
	}
	if err := s.loadProgress(filepath.Join(configDir, "state.db")); err != nil {
		return nil, err
	}
	go s.flusher()
	return s, nil
}

func (s *Store) Generation() int64 { return s.generation.Load() }

// View runs fn with the data read-locked. Copy out what you need: pointers
// into Data must not be used after fn returns.
func (s *Store) View(fn func(d *Data)) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	fn(&s.data)
}

// Update runs fn with the library write-locked and schedules library.json
// to be written. Replace slices (Streams, Skips) instead of changing their
// elements; readers may still hold copies of an Item.
func (s *Store) Update(fn func(d *Data)) {
	s.mu.Lock()
	fn(&s.data)
	s.generation.Add(1)
	s.mu.Unlock()
	s.dirty.Store(true)
}

// GetItem returns a copy of an item.
func (s *Store) GetItem(id string) (Item, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if it, ok := s.data.Items[id]; ok {
		return *it, true
	}
	return Item{}, false
}

// SetProgress stores progress records and appends them to progress.jsonl.
func (s *Store) SetProgress(recs ...Progress) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, p := range recs {
		_ = enc.Encode(p)
	}
	// The log lock is held across both steps, so lines land in the file in
	// the order the map saw them: the last line for an id is its value.
	s.logMu.Lock()
	defer s.logMu.Unlock()
	s.mu.Lock()
	for i := range recs {
		p := recs[i]
		s.data.Progress[p.ID] = &p
	}
	s.generation.Add(1)
	n := len(s.data.Progress)
	s.mu.Unlock()

	if s.logFile == nil {
		return errors.New("store is closed")
	}
	if _, err := s.logFile.Write(buf.Bytes()); err != nil {
		return fmt.Errorf("writing %s: %w", s.progressPath, err)
	}
	s.logLines += len(recs)
	if s.logLines > 4*n+1000 {
		return s.compactLocked()
	}
	return nil
}

// ---- library.json -----------------------------------------------------------

type libraryFile struct {
	Version  int       `json:"version"`
	Items    []*Item   `json:"items"`
	Series   []*Series `json:"series"`
	Metadata []*Meta   `json:"metadata"`
}

func (s *Store) loadLibrary(legacy string) error {
	raw, err := os.ReadFile(s.libraryPath)
	if errors.Is(err, os.ErrNotExist) {
		if _, err := os.Stat(legacy); err == nil {
			if err := importLibrary(legacy, &s.data); err != nil {
				log.Warnf("cannot import %s, the next scan rebuilds it: %v", legacy, err)
				s.data.Items, s.data.Series, s.data.Meta = map[string]*Item{}, map[string]*Series{}, map[string]*Meta{}
			} else {
				log.Infof("imported %d items, %d series from %s", len(s.data.Items), len(s.data.Series), legacy)
				return s.Flush()
			}
		}
		return nil
	}
	if err != nil {
		return err
	}
	var f libraryFile
	if err := json.Unmarshal(raw, &f); err != nil {
		// Disposable: start over rather than refuse to start.
		log.Warnf("%s is unreadable, rebuilding it: %v", s.libraryPath, err)
		return nil
	}
	for _, it := range f.Items {
		s.data.Items[it.ID] = it
	}
	for _, se := range f.Series {
		s.data.Series[se.ID] = se
	}
	for _, m := range f.Metadata {
		s.data.Meta[m.ID] = m
	}
	return nil
}

// Flush writes library.json now if anything changed.
func (s *Store) Flush() error {
	s.flushMu.Lock()
	defer s.flushMu.Unlock()
	s.dirty.Store(false)
	s.mu.RLock()
	f := libraryFile{Version: 1, Items: make([]*Item, 0, len(s.data.Items)),
		Series: make([]*Series, 0, len(s.data.Series)), Metadata: make([]*Meta, 0, len(s.data.Meta))}
	for _, it := range s.data.Items {
		f.Items = append(f.Items, it)
	}
	for _, se := range s.data.Series {
		f.Series = append(f.Series, se)
	}
	for _, m := range s.data.Meta {
		f.Metadata = append(f.Metadata, m)
	}
	raw, err := json.Marshal(f)
	s.mu.RUnlock()
	if err != nil {
		return err
	}
	if err := writeAtomic(s.libraryPath, raw); err != nil {
		s.dirty.Store(true)
		return err
	}
	return nil
}

func (s *Store) flusher() {
	defer close(s.done)
	t := time.NewTicker(FlushEvery)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			if s.dirty.Load() {
				if err := s.Flush(); err != nil {
					log.Warnf("cannot write %s: %v", s.libraryPath, err)
				}
			}
		}
	}
}

// Close writes outstanding changes and closes the progress log.
func (s *Store) Close() error {
	select {
	case <-s.stop:
		return nil
	default:
	}
	close(s.stop)
	<-s.done
	var err error
	if s.dirty.Load() {
		err = s.Flush()
	}
	s.logMu.Lock()
	defer s.logMu.Unlock()
	if s.logFile != nil {
		if e := s.logFile.Close(); err == nil {
			err = e
		}
		s.logFile = nil
	}
	return err
}

func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// ---- progress.jsonl ---------------------------------------------------------

func (s *Store) loadProgress(legacy string) error {
	f, err := os.Open(s.progressPath)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if _, err := os.Stat(legacy); err == nil {
			n, err := importProgress(legacy, s.data.Progress)
			if err != nil {
				// Watch progress is the one thing worth keeping: don't start
				// over silently and then overwrite nothing, but don't refuse
				// to run either. The old file stays where it is.
				log.Errorf("cannot import watch progress from %s: %v", legacy, err)
			} else {
				log.Infof("imported watch progress of %d items from %s", n, legacy)
			}
		}
	case err != nil:
		return err
	default:
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			var p Progress
			if json.Unmarshal(sc.Bytes(), &p) != nil || p.ID == "" {
				continue // a line cut off by a crash
			}
			s.data.Progress[p.ID] = &p
		}
		err := sc.Err()
		f.Close()
		if err != nil {
			return err
		}
	}
	s.logMu.Lock()
	defer s.logMu.Unlock()
	return s.compactLocked()
}

// compactLocked rewrites progress.jsonl with one line per item. Caller holds logMu.
func (s *Store) compactLocked() error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	s.mu.RLock()
	n := len(s.data.Progress)
	for _, p := range s.data.Progress {
		_ = enc.Encode(p)
	}
	s.mu.RUnlock()
	if err := writeAtomic(s.progressPath, buf.Bytes()); err != nil {
		return err
	}
	if s.logFile != nil {
		s.logFile.Close()
	}
	f, err := os.OpenFile(s.progressPath, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		s.logFile = nil
		return err
	}
	s.logFile, s.logLines = f, n
	return nil
}
