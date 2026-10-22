// Package images makes posters and thumbnails, cached on the SSD.
//
// Sidecar images next to the media (poster.jpg, folder.jpg, <name>-thumb.jpg)
// win, then a poster downloaded from TMDB/AniList (series and movies, see
// package metadata); otherwise a frame is grabbed with ffmpeg. Everything is
// downscaled once so library grids never pull multi-megabyte posters over SMB.
package images

import (
	"context"
	"fmt"
	"hash/fnv"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nine9123/media-watcher/internal/config"
	"github.com/nine9123/media-watcher/internal/shutdown"
	"github.com/nine9123/media-watcher/internal/store"
)

var PosterNames = []string{"poster", "folder", "cover", "show", "default"}

var imageExts = []string{".jpg", ".jpeg", ".png", ".webp"}

const (
	posterWidth = 360
	thumbWidth  = 480
	lockStripes = 64
)

// FindImage looks for stem+ext in a directory listing that maps lower-case
// file name -> real file name.
func FindImage(listing map[string]string, stems ...string) string {
	for _, stem := range stems {
		for _, ext := range imageExts {
			if real, ok := listing[strings.ToLower(stem+ext)]; ok {
				return real
			}
		}
	}
	return ""
}

type Images struct {
	cfg     *config.Config
	db      *store.Store
	Dir     string
	MetaDir string
	sem     chan struct{}
	// A fixed set of striped locks: one per path would grow forever.
	locks [lockStripes]sync.Mutex
	// Frame grabs a thumbnail from the video; replaceable in tests.
	Frame func(path string, duration *float64, out string) bool
}

func New(cfg *config.Config, db *store.Store) (*Images, error) {
	im := &Images{cfg: cfg, db: db, Dir: filepath.Join(cfg.CacheDir, "images"),
		MetaDir: filepath.Join(cfg.CacheDir, "metadata"), sem: make(chan struct{}, cfg.ThumbWorkers)}
	for _, d := range []string{im.Dir, im.MetaDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	im.Frame = im.frame
	return im, nil
}

func (im *Images) lock(key string) *sync.Mutex {
	h := fnv.New32a()
	h.Write([]byte(key))
	return &im.locks[h.Sum32()%lockStripes]
}

func (im *Images) CachePath(kind, id string) string {
	return filepath.Join(im.Dir, kind+"-"+id+".jpg")
}

// MetaPosterPath is the full-size poster downloaded by package metadata for a series or movie.
func (im *Images) MetaPosterPath(id string) string { return filepath.Join(im.MetaDir, id+".jpg") }

func (im *Images) metaPoster(id string) string {
	p := im.MetaPosterPath(id)
	if exists(p) {
		return p
	}
	return ""
}

// FailedPath is a zero-byte marker: no image could be made from the current source.
func (im *Images) FailedPath(kind, id string) string {
	return filepath.Join(im.Dir, kind+"-"+id+".none")
}

func (im *Images) Failed(kind, id string) bool { return exists(im.FailedPath(kind, id)) }

func (im *Images) Remove(kind, id string) {
	os.Remove(im.CachePath(kind, id))
	os.Remove(im.FailedPath(kind, id))
}

// DropStale removes a cached image that is older than the sidecar it was made from.
//
// Cached images are served without a store lookup, so the scanner calls
// this (and Remove) to keep them in step with the media.
func (im *Images) DropStale(kind, id, source string) {
	if source == "" {
		return
	}
	for _, p := range []string{im.CachePath(kind, id), im.FailedPath(kind, id)} {
		if exists(p) && !fresh(p, source) {
			im.Remove(kind, id)
			return
		}
	}
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func fresh(out, source string) bool {
	a, err := os.Stat(out)
	if err != nil {
		return false
	}
	b, err := os.Stat(source)
	if err != nil {
		return false
	}
	return !a.ModTime().Before(b.ModTime())
}

func (im *Images) run(args []string, out string) bool {
	tmp := out + ".tmp.jpg"
	im.sem <- struct{}{}
	ctx, cancel := context.WithTimeout(shutdown.Context(), 60*time.Second)
	err := exec.CommandContext(ctx, args[0], append(args[1:], "-y", tmp)...).Run()
	cancel()
	<-im.sem
	if st, e := os.Stat(tmp); err == nil && e == nil && st.Size() > 0 {
		return os.Rename(tmp, out) == nil
	}
	os.Remove(tmp)
	return false
}

func (im *Images) resize(src, out string, width int) bool {
	return im.run([]string{im.cfg.FFmpeg, "-hide_banner", "-loglevel", "error", "-nostdin", "-i", src,
		"-frames:v", "1", "-vf", fmt.Sprintf("scale='min(%d,iw)':-2", width), "-q:v", "3"}, out)
}

func (im *Images) frame(path string, duration *float64, out string) bool {
	t := 30.0
	if duration != nil && *duration > 0 {
		t = *duration * 0.2
	}
	t = min(t, 600)
	vf := fmt.Sprintf("scale=%d:-2,format=yuvj420p", thumbWidth)
	ok := im.run([]string{im.cfg.FFmpeg, "-hide_banner", "-loglevel", "error", "-nostdin",
		"-ss", fmt.Sprintf("%.2f", t), "-i", path, "-map", "0:v:0", "-frames:v", "1", "-vf", vf, "-q:v", "4"}, out)
	if !ok && t > 0 { // very short clips / bad seek index
		ok = im.run([]string{im.cfg.FFmpeg, "-hide_banner", "-loglevel", "error", "-nostdin",
			"-i", path, "-map", "0:v:0", "-frames:v", "1", "-vf", vf, "-q:v", "4"}, out)
	}
	return ok
}

// ItemImage is the cached thumbnail (or poster, for movies) of an item,
// made now if needed; "" if there is none.
func (im *Images) ItemImage(id string) string {
	it, ok := im.db.GetItem(id)
	if !ok {
		return ""
	}
	out, failed := im.CachePath("item", id), im.FailedPath("item", id)
	image := it.Image
	if image != "" && !exists(image) {
		// A sidecar deleted since the last scan: judge freshness and failure
		// by what is used instead, or a failed frame grab is retried on
		// every request.
		image = ""
	}
	if image == "" && it.Kind == "movie" {
		image = im.metaPoster(id)
	}
	source := image
	if source == "" {
		source = it.Path
	}
	if fresh(out, source) {
		return out
	}
	l := im.lock(out)
	l.Lock()
	defer l.Unlock()
	if fresh(out, source) {
		return out
	}
	if fresh(failed, source) {
		return "" // tried before and failed; the scanner clears this on change
	}
	if image != "" && exists(image) && im.resize(image, out, thumbWidth) {
		return out
	}
	if im.Frame(it.Path, it.Duration, out) {
		return out
	}
	// Not for a share that is briefly unmounted, nor for an ffmpeg
	// killed because we are shutting down.
	if exists(source) && !shutdown.Stopping() {
		if f, err := os.Create(failed); err == nil {
			f.Close()
		}
	}
	return ""
}

// SeriesImage is the cached poster of a series, else the thumbnail of its first episode.
func (im *Images) SeriesImage(id string) string {
	var poster, first string
	found := false
	im.db.View(func(d *store.Data) {
		s, ok := d.Series[id]
		if !ok {
			return
		}
		found = true
		poster = s.Poster
		var eps []*store.Item
		for _, it := range d.Items {
			if it.SeriesID == id {
				eps = append(eps, it)
			}
		}
		if len(eps) > 0 {
			sort.Slice(eps, func(i, j int) bool { return store.EpisodeLess(eps[i], eps[j]) })
			first = eps[0].ID
		}
	})
	if !found {
		return ""
	}
	if poster != "" && !exists(poster) {
		poster = ""
	}
	if poster == "" {
		poster = im.metaPoster(id)
	}
	if poster != "" {
		out := im.CachePath("series", id)
		if fresh(out, poster) {
			return out
		}
		l := im.lock(out)
		l.Lock()
		ok := fresh(out, poster) || im.resize(poster, out, posterWidth)
		l.Unlock()
		if ok {
			return out
		}
	}
	if first != "" {
		return im.ItemImage(first)
	}
	return ""
}
